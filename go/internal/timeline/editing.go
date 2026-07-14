package timeline

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

type clipLocation struct {
	trackIndex int
	clipIndex  int
}

func locateClip(document *Document, clipID string) (clipLocation, error) {
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			if document.Tracks[trackIndex].Clips[clipIndex].TimelineClipID == clipID {
				return clipLocation{trackIndex: trackIndex, clipIndex: clipIndex}, nil
			}
		}
	}
	return clipLocation{}, fmt.Errorf("clip 不存在: %s", clipID)
}

func editableLocation(document *Document, operation map[string]any) (clipLocation, error) {
	id := valueOr(stringValue(operation["timeline_clip_id"]), stringValue(operation["clip_id"]))
	if id == "" {
		return clipLocation{}, errors.New("patch op 缺少 timeline_clip_id")
	}
	location, err := locateClip(document, id)
	if err != nil {
		return clipLocation{}, err
	}
	if document.Tracks[location.trackIndex].Locked {
		return clipLocation{}, fmt.Errorf("轨道 %s 已锁定", document.Tracks[location.trackIndex].TrackID)
	}
	return location, nil
}

func updateEditableClip(
	document *Document,
	operation map[string]any,
	update func(*Track, *Clip) error,
) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	track := &document.Tracks[location.trackIndex]
	return update(track, &track.Clips[location.clipIndex])
}

func setTrackState(document *Document, operation map[string]any) error {
	trackID := stringValue(operation["track_id"])
	track := trackByID(document, trackID)
	if track == nil {
		return fmt.Errorf("轨道不存在: %s", trackID)
	}
	changed := false
	for key, destination := range map[string]*bool{
		"muted":  &track.Muted,
		"solo":   &track.Solo,
		"locked": &track.Locked,
	} {
		value, exists := operation[key]
		if !exists {
			continue
		}
		boolean, ok := value.(bool)
		if !ok {
			return fmt.Errorf("%s 必须是布尔值", key)
		}
		if key == "muted" && trackID == "visual_base" && boolean {
			return errors.New("主视觉轨不能静音")
		}
		*destination = boolean
		changed = true
	}
	if value, exists := operation["gain_db"]; exists {
		gain, ok := numericValue(value)
		if !ok || gain < -60 || gain > 12 {
			return errors.New("gain_db 必须在 [-60,12] 范围内")
		}
		if trackFamily(*track) != "audio" {
			return errors.New("只有音频轨支持轨道音量")
		}
		track.GainDB = gain
		changed = true
	}
	if !changed {
		return errors.New("set_track_state 没有可更新字段")
	}
	return nil
}

func setClipLinked(document *Document, operation map[string]any) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	linked, ok := operation["linked"].(bool)
	if !ok {
		return errors.New("set_clip_linked 缺少 linked 布尔值")
	}
	selected := &document.Tracks[location.trackIndex].Clips[location.clipIndex]
	if !linked {
		groupID := selected.ParentBlockID
		selected.Linked = false
		selected.ParentBlockID = ""
		if groupID != "" {
			members := linkedGroup(document, groupID)
			if len(members) == 1 {
				member := members[0]
				document.Tracks[member.trackIndex].Clips[member.clipIndex].Linked = false
				document.Tracks[member.trackIndex].Clips[member.clipIndex].ParentBlockID = ""
			}
		}
		return nil
	}
	if selected.Linked && selected.ParentBlockID != "" && len(linkedGroup(document, selected.ParentBlockID)) > 1 {
		return nil
	}
	candidate, found := linkCandidate(document, location)
	if !found {
		return errors.New("没有可与该片段联动的同源音画片段")
	}
	if document.Tracks[candidate.trackIndex].Locked {
		return fmt.Errorf("轨道 %s 已锁定", document.Tracks[candidate.trackIndex].TrackID)
	}
	groupID := selected.ParentBlockID
	if groupID == "" {
		groupID = document.Tracks[candidate.trackIndex].Clips[candidate.clipIndex].ParentBlockID
	}
	if groupID == "" {
		groupID = "link_" + selected.TimelineClipID
	}
	selected.Linked = true
	selected.ParentBlockID = groupID
	partner := &document.Tracks[candidate.trackIndex].Clips[candidate.clipIndex]
	partner.Linked = true
	partner.ParentBlockID = groupID
	return nil
}

func linkCandidate(document *Document, selected clipLocation) (clipLocation, bool) {
	clip := document.Tracks[selected.trackIndex].Clips[selected.clipIndex]
	trackID := document.Tracks[selected.trackIndex].TrackID
	wantedTrack := "visual_base"
	if trackID == "visual_base" {
		wantedTrack = "original_audio"
	} else if trackID != "original_audio" {
		return clipLocation{}, false
	}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID != wantedTrack {
			continue
		}
		for clipIndex, candidate := range document.Tracks[trackIndex].Clips {
			if candidate.AssetID == clip.AssetID &&
				candidate.TimelineStartFrame == clip.TimelineStartFrame &&
				candidate.TimelineEndFrame == clip.TimelineEndFrame &&
				candidate.SourceStartFrame == clip.SourceStartFrame &&
				candidate.SourceEndFrame == clip.SourceEndFrame {
				return clipLocation{trackIndex: trackIndex, clipIndex: clipIndex}, true
			}
		}
	}
	return clipLocation{}, false
}

func linkedGroup(document *Document, groupID string) []clipLocation {
	if groupID == "" {
		return nil
	}
	members := []clipLocation{}
	for trackIndex := range document.Tracks {
		for clipIndex, clip := range document.Tracks[trackIndex].Clips {
			if clip.Linked && clip.ParentBlockID == groupID {
				members = append(members, clipLocation{trackIndex: trackIndex, clipIndex: clipIndex})
			}
		}
	}
	return members
}

func moveClip(document *Document, operation map[string]any) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	targetFrame, err := frameValue(operation, "target_frame")
	if err != nil {
		return err
	}
	mode := valueOr(stringValue(operation["mode"]), "insert")
	if mode != "insert" && mode != "overwrite" {
		return errors.New("move_clip mode 必须是 insert 或 overwrite")
	}
	targetTrackID := valueOr(stringValue(operation["target_track_id"]), document.Tracks[location.trackIndex].TrackID)
	targetTrack := trackByID(document, targetTrackID)
	if targetTrack == nil {
		return fmt.Errorf("目标轨道不存在: %s", targetTrackID)
	}
	if targetTrack.Locked {
		return fmt.Errorf("轨道 %s 已锁定", targetTrackID)
	}
	sourceTrack := &document.Tracks[location.trackIndex]
	moving := sourceTrack.Clips[location.clipIndex]
	if !tracksCompatible(*sourceTrack, *targetTrack, moving) {
		return fmt.Errorf("片段不能从 %s 移到 %s", sourceTrack.TrackID, targetTrack.TrackID)
	}

	if moving.Linked && moving.ParentBlockID != "" {
		members := linkedGroup(document, moving.ParentBlockID)
		for _, member := range members {
			if document.Tracks[member.trackIndex].TrackID == "visual_base" {
				if targetTrackID != sourceTrack.TrackID {
					return errors.New("跨轨移动前请先取消音画联动")
				}
				return reorderClip(document, map[string]any{
					"timeline_clip_id": document.Tracks[member.trackIndex].Clips[member.clipIndex].TimelineClipID,
					"target_frame":     targetFrame,
				})
			}
		}
		if targetTrackID != sourceTrack.TrackID {
			return errors.New("跨轨移动前请先取消片段联动")
		}
	}

	if sourceTrack.TrackID == "visual_base" && targetTrackID == "visual_base" {
		return reorderClip(document, operation)
	}
	duration := moving.TimelineEndFrame - moving.TimelineStartFrame
	if duration <= 0 {
		return errors.New("移动片段时长无效")
	}
	sourceTrackID := sourceTrack.TrackID
	sourceStart := moving.TimelineStartFrame
	sourceEnd := moving.TimelineEndFrame
	sourceTrack.Clips = append(sourceTrack.Clips[:location.clipIndex], sourceTrack.Clips[location.clipIndex+1:]...)

	if sourceTrackID == "visual_base" {
		if len(sourceTrack.Clips) == 0 {
			return errors.New("主视觉轨至少保留一个片段")
		}
		if err := ensureRippleUnlocked(document, sourceStart, sourceTrackID); err != nil {
			return err
		}
		if err := deleteRange(document, map[string]any{"start_frame": sourceStart, "end_frame": sourceEnd}); err != nil {
			return err
		}
		if targetFrame > sourceEnd {
			targetFrame -= duration
		}
	}

	if targetTrackID == "visual_base" {
		moving.TrackID = targetTrackID
		moving.Linked = false
		moving.ParentBlockID = ""
		if mode == "insert" {
			return insertIntoPrimary(document, moving, targetFrame)
		}
		return overwritePrimary(document, moving, targetFrame)
	}

	targetTrack = trackByID(document, targetTrackID)
	if targetTrack == nil {
		return fmt.Errorf("目标轨道不存在: %s", targetTrackID)
	}
	if duration > document.DurationFrames {
		return errors.New("片段长于移动后的时间线，不能放入叠加轨")
	}
	targetFrame = clampInt(targetFrame, 0, max(0, document.DurationFrames-duration))
	moveDelta := targetFrame - sourceStart
	if mode == "insert" {
		shiftTrackForInsert(targetTrack, targetFrame, duration, document.DurationFrames)
	} else {
		eraseTrackRange(targetTrack, targetFrame, targetFrame+duration)
	}
	moving.TrackID = targetTrackID
	moving.TimelineStartFrame = targetFrame
	moving.TimelineEndFrame = targetFrame + duration
	shiftClipTimelineMetadata(&moving, moveDelta)
	syncClipPlacementMetadata(&moving)
	if sourceTrackID != targetTrackID {
		moving.Linked = false
		moving.ParentBlockID = ""
	}
	targetTrack.Clips = append(targetTrack.Clips, moving)
	sortTrack(targetTrack)
	return nil
}

func trimClipEdge(document *Document, operation map[string]any) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	frame, err := frameValue(operation, "timeline_frame")
	if err != nil {
		return err
	}
	edge := stringValue(operation["edge"])
	if edge != "start" && edge != "end" {
		return errors.New("trim_clip_edge edge 必须是 start 或 end")
	}
	selected := document.Tracks[location.trackIndex].Clips[location.clipIndex]
	if frame <= selected.TimelineStartFrame || frame >= selected.TimelineEndFrame {
		return errors.New("裁剪点必须位于片段内部")
	}
	members := []clipLocation{location}
	if selected.Linked && selected.ParentBlockID != "" {
		members = linkedGroup(document, selected.ParentBlockID)
	}
	hasPrimary := false
	for _, member := range members {
		track := document.Tracks[member.trackIndex]
		if track.Locked {
			return fmt.Errorf("轨道 %s 已锁定", track.TrackID)
		}
		if track.TrackID == "visual_base" {
			hasPrimary = true
		}
	}
	if hasPrimary {
		start := selected.TimelineStartFrame
		end := frame
		if edge == "end" {
			start = frame
			end = selected.TimelineEndFrame
		}
		if err := ensureRippleUnlocked(document, start, ""); err != nil {
			return err
		}
		return deleteRange(document, map[string]any{"start_frame": start, "end_frame": end})
	}
	for _, member := range members {
		clip := &document.Tracks[member.trackIndex].Clips[member.clipIndex]
		if frame <= clip.TimelineStartFrame || frame >= clip.TimelineEndFrame {
			continue
		}
		rate := effectiveRate(*clip)
		if edge == "start" {
			delta := frame - clip.TimelineStartFrame
			clip.TimelineStartFrame = frame
			if clip.AssetID != "" {
				clip.SourceStartFrame += int(math.Round(float64(delta) * rate))
			}
		} else {
			delta := clip.TimelineEndFrame - frame
			clip.TimelineEndFrame = frame
			if clip.AssetID != "" {
				clip.SourceEndFrame -= int(math.Round(float64(delta) * rate))
			}
		}
	}
	return nil
}

func deleteClip(document *Document, operation map[string]any) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	selected := document.Tracks[location.trackIndex].Clips[location.clipIndex]
	members := []clipLocation{location}
	if selected.Linked && selected.ParentBlockID != "" {
		members = linkedGroup(document, selected.ParentBlockID)
	}
	hasPrimary := false
	for _, member := range members {
		track := document.Tracks[member.trackIndex]
		if track.Locked {
			return fmt.Errorf("轨道 %s 已锁定", track.TrackID)
		}
		if track.TrackID == "visual_base" {
			hasPrimary = true
			if len(track.Clips) <= 1 {
				return errors.New("主视觉轨至少保留一个片段")
			}
		}
	}
	if hasPrimary {
		if err := ensureRippleUnlocked(document, selected.TimelineStartFrame, ""); err != nil {
			return err
		}
		return deleteRange(document, map[string]any{
			"start_frame": selected.TimelineStartFrame,
			"end_frame":   selected.TimelineEndFrame,
		})
	}
	groupID := selected.ParentBlockID
	for trackIndex := range document.Tracks {
		kept := document.Tracks[trackIndex].Clips[:0]
		for _, clip := range document.Tracks[trackIndex].Clips {
			remove := clip.TimelineClipID == selected.TimelineClipID
			if groupID != "" && selected.Linked {
				remove = remove || (clip.Linked && clip.ParentBlockID == groupID)
			}
			if !remove {
				kept = append(kept, clip)
			}
		}
		document.Tracks[trackIndex].Clips = kept
	}
	return nil
}

func insertSubtitle(document *Document, operation map[string]any) error {
	track := trackByID(document, "subtitles")
	if track == nil || track.Locked {
		return errors.New("字幕轨不存在或已锁定")
	}
	start, startErr := frameValue(operation, "start_frame")
	end, endErr := frameValue(operation, "end_frame")
	if startErr != nil || endErr != nil {
		return errors.Join(startErr, endErr)
	}
	text := strings.TrimSpace(stringValue(operation["text"]))
	if start < 0 || end <= start || end > document.DurationFrames || text == "" {
		return errors.New("insert_subtitle 时间范围或文字无效")
	}
	id := valueOr(
		stringValue(operation["timeline_clip_id"]),
		fmt.Sprintf("subtitle_v%d_%03d", document.Version+1, len(track.Clips)+1),
	)
	if _, err := locateClip(document, id); err == nil {
		return fmt.Errorf("timeline_clip_id 已存在: %s", id)
	}
	track.Clips = append(track.Clips, Clip{
		TimelineClipID:     id,
		TrackID:            "subtitles",
		Text:               text,
		TimelineStartFrame: start,
		TimelineEndFrame:   end,
		LockPolicy:         "free",
	})
	sortTrack(track)
	return nil
}

func insertIntoPrimary(document *Document, moving Clip, targetFrame int) error {
	primary := trackByID(document, "visual_base")
	if primary == nil || primary.Locked {
		return errors.New("主视觉轨不存在或已锁定")
	}
	targetFrame = clampInt(targetFrame, 0, document.DurationFrames)
	duration := moving.TimelineEndFrame - moving.TimelineStartFrame
	if err := ensureRippleUnlocked(document, targetFrame, "visual_base"); err != nil {
		return err
	}
	if err := splitDocumentPrimaryAt(document, targetFrame); err != nil {
		return err
	}
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.TimelineStartFrame >= targetFrame {
				clip.TimelineStartFrame += duration
				clip.TimelineEndFrame += duration
				shiftClipTimelineMetadata(clip, duration)
			}
		}
	}
	document.DurationFrames += duration
	moving.TimelineStartFrame = targetFrame
	moving.TimelineEndFrame = targetFrame + duration
	primary = trackByID(document, "visual_base")
	primary.Clips = append(primary.Clips, moving)
	sortTrack(primary)
	return nil
}

func overwritePrimary(document *Document, moving Clip, targetFrame int) error {
	primary := trackByID(document, "visual_base")
	if primary == nil || primary.Locked {
		return errors.New("主视觉轨不存在或已锁定")
	}
	duration := moving.TimelineEndFrame - moving.TimelineStartFrame
	if duration > document.DurationFrames {
		return errors.New("覆盖片段长于当前时间线")
	}
	targetFrame = clampInt(targetFrame, 0, document.DurationFrames-duration)
	eraseTrackRange(primary, targetFrame, targetFrame+duration)
	moving.TimelineStartFrame = targetFrame
	moving.TimelineEndFrame = targetFrame + duration
	primary.Clips = append(primary.Clips, moving)
	sortTrack(primary)
	return nil
}

func splitDocumentPrimaryAt(document *Document, frame int) error {
	if frame <= 0 {
		return nil
	}
	track := trackByID(document, "visual_base")
	if track == nil {
		return errors.New("主视觉轨不存在")
	}
	for _, clip := range track.Clips {
		if frame == clip.TimelineStartFrame || frame == clip.TimelineEndFrame {
			return nil
		}
		if frame < clip.TimelineStartFrame || frame > clip.TimelineEndFrame {
			continue
		}
		return splitClip(document, map[string]any{
			"timeline_clip_id": clip.TimelineClipID,
			"split_frame":      frame,
		})
	}
	return nil
}

func eraseTrackRange(track *Track, start, end int) {
	kept := make([]Clip, 0, len(track.Clips)+1)
	for _, clip := range track.Clips {
		if clip.TimelineEndFrame <= start || clip.TimelineStartFrame >= end {
			kept = append(kept, clip)
			continue
		}
		rate := effectiveRate(clip)
		if clip.TimelineStartFrame < start {
			left := clip
			removed := clip.TimelineEndFrame - start
			left.TimelineEndFrame = start
			if left.AssetID != "" {
				left.SourceEndFrame -= int(math.Round(float64(removed) * rate))
			}
			syncClipPlacementMetadata(&left)
			kept = append(kept, left)
		}
		if clip.TimelineEndFrame > end {
			right := clip
			removed := end - clip.TimelineStartFrame
			right.TimelineClipID = clip.TimelineClipID + fmt.Sprintf("_after_%d", end)
			right.TimelineStartFrame = end
			if right.AssetID != "" {
				right.SourceStartFrame += int(math.Round(float64(removed) * rate))
			}
			syncClipPlacementMetadata(&right)
			kept = append(kept, right)
		}
	}
	track.Clips = kept
}

func shiftTrackForInsert(track *Track, frame, duration, timelineDuration int) {
	kept := make([]Clip, 0, len(track.Clips))
	for _, clip := range track.Clips {
		if clip.TimelineStartFrame >= frame {
			clip.TimelineStartFrame += duration
			clip.TimelineEndFrame += duration
			shiftClipTimelineMetadata(&clip, duration)
		}
		if clip.TimelineStartFrame >= timelineDuration {
			continue
		}
		if clip.TimelineEndFrame > timelineDuration {
			overflow := clip.TimelineEndFrame - timelineDuration
			clip.TimelineEndFrame = timelineDuration
			if clip.AssetID != "" {
				clip.SourceEndFrame -= int(math.Round(float64(overflow) * effectiveRate(clip)))
			}
			syncClipPlacementMetadata(&clip)
		}
		kept = append(kept, clip)
	}
	track.Clips = kept
}

func ensureRippleUnlocked(document *Document, boundary int, exceptTrackID string) error {
	for _, track := range document.Tracks {
		if !track.Locked || track.TrackID == exceptTrackID {
			continue
		}
		for _, clip := range track.Clips {
			if clip.TimelineEndFrame > boundary {
				return fmt.Errorf("轨道 %s 已锁定，不能执行波纹编辑", track.TrackID)
			}
		}
	}
	return nil
}

func tracksCompatible(source, target Track, clip Clip) bool {
	sourceFamily := trackFamily(source)
	targetFamily := trackFamily(target)
	if sourceFamily != targetFamily {
		return false
	}
	switch targetFamily {
	case "visual":
		return clip.AssetID != "" && (clip.AssetKind == "" || clip.AssetKind == "video" || clip.AssetKind == "image")
	case "audio":
		return clip.AssetID != "" && (clip.AssetKind == "" || clip.AssetKind == "video" || clip.AssetKind == "audio")
	case "text":
		return strings.TrimSpace(clip.Text) != ""
	default:
		return source.TrackType == target.TrackType
	}
}

func trackFamily(track Track) string {
	switch track.TrackID {
	case "visual_base", "visual_overlay":
		return "visual"
	case "original_audio", "voiceover", "bgm", "sfx":
		return "audio"
	case "subtitles":
		return "text"
	}
	switch track.TrackType {
	case "primary_visual", "visual_overlay", "video":
		return "visual"
	case "audio":
		return "audio"
	case "text":
		return "text"
	default:
		return track.TrackType
	}
}

func effectiveRate(clip Clip) float64 {
	if clip.PlaybackRate > 0 {
		return clip.PlaybackRate
	}
	return 1
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		converted := float64(typed)
		return converted, !math.IsNaN(converted) && !math.IsInf(converted, 0)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

var semanticTimelineMetadataKeys = []string{
	"anchor_timeline_start_frame", "anchor_timeline_end_frame",
	"placement_timeline_start_frame", "placement_timeline_end_frame",
}

// B-roll 的语义锚点既是模型下一轮 context 的证据，也是前端可解释性数据。
// 时间线片段发生平移时必须同步这些绝对帧字段，否则画面虽然移动了，模型
// 仍会看到旧台词位置，并在后续编辑中继续放大错位。
func shiftClipTimelineMetadata(clip *Clip, delta int) {
	if delta == 0 || clip.Metadata == nil || stringValue(clip.Metadata["kind"]) != "b_roll_semantic_anchor" {
		return
	}
	for _, key := range semanticTimelineMetadataKeys {
		value, ok := numericValue(clip.Metadata[key])
		if !ok || math.Trunc(value) != value {
			continue
		}
		clip.Metadata[key] = int(value) + delta
	}
}

func syncClipPlacementMetadata(clip *Clip) {
	if clip.Metadata == nil || stringValue(clip.Metadata["kind"]) != "b_roll_semantic_anchor" {
		return
	}
	clip.Metadata["placement_timeline_start_frame"] = clip.TimelineStartFrame
	clip.Metadata["placement_timeline_end_frame"] = clip.TimelineEndFrame
}

func sortTrack(track *Track) {
	sort.SliceStable(track.Clips, func(i, j int) bool {
		if track.Clips[i].TimelineStartFrame == track.Clips[j].TimelineStartFrame {
			return track.Clips[i].TimelineClipID < track.Clips[j].TimelineClipID
		}
		return track.Clips[i].TimelineStartFrame < track.Clips[j].TimelineStartFrame
	})
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
