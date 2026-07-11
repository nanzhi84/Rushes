package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const DefaultFPS = 30

var requiredTracks = []struct {
	ID   string
	Type string
}{
	{"visual_base", "primary_visual"}, {"visual_overlay", "visual_overlay"},
	{"original_audio", "audio"}, {"voiceover", "audio"}, {"bgm", "audio"},
	{"subtitles", "text"},
}

type Document struct {
	TimelineID     string  `json:"timeline_id"`
	DraftID        string  `json:"draft_id"`
	Version        int     `json:"version"`
	FPS            int     `json:"fps"`
	DurationFrames int     `json:"duration_frames"`
	Tracks         []Track `json:"tracks"`
}

type Track struct {
	TrackID   string  `json:"track_id"`
	TrackType string  `json:"track_type"`
	Clips     []Clip  `json:"clips"`
	Muted     bool    `json:"muted,omitempty"`
	Solo      bool    `json:"solo,omitempty"`
	Locked    bool    `json:"locked,omitempty"`
	GainDB    float64 `json:"gain_db,omitempty"`
}

type Clip struct {
	TimelineClipID     string           `json:"timeline_clip_id"`
	TrackID            string           `json:"track_id"`
	AssetID            string           `json:"asset_id,omitempty"`
	AssetKind          string           `json:"asset_kind,omitempty"`
	ClipID             *string          `json:"clip_id,omitempty"`
	Role               string           `json:"role,omitempty"`
	Text               string           `json:"text,omitempty"`
	TimelineStartFrame int              `json:"timeline_start_frame"`
	TimelineEndFrame   int              `json:"timeline_end_frame"`
	SourceStartFrame   int              `json:"source_start_frame,omitempty"`
	SourceEndFrame     int              `json:"source_end_frame,omitempty"`
	PlaybackRate       float64          `json:"playback_rate,omitempty"`
	GainDB             float64          `json:"gain_db,omitempty"`
	LockPolicy         string           `json:"lock_policy,omitempty"`
	ParentBlockID      string           `json:"parent_block_id,omitempty"`
	Linked             bool             `json:"linked,omitempty"`
	Effects            []map[string]any `json:"effects,omitempty"`
}

type Selection struct {
	AssetID          string
	AssetKind        string
	SourceStartFrame int
	SourceEndFrame   int
	Role             string
	HasAudio         bool
}

type ValidationIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ValidationReport struct {
	Valid  bool              `json:"valid"`
	Checks []string          `json:"checks"`
	Issues []ValidationIssue `json:"issues"`
}

func ComposeInitial(draftID string, version int, selections []Selection) (Document, error) {
	if draftID == "" || version < 1 || len(selections) == 0 {
		return Document{}, errors.New("compose_initial 参数无效")
	}
	document := Empty(draftID, version)
	primary := &document.Tracks[0]
	originalAudio := &document.Tracks[2]
	cursor := 0
	for index, selection := range selections {
		if selection.AssetID == "" || selection.SourceStartFrame < 0 || selection.SourceEndFrame <= selection.SourceStartFrame {
			return Document{}, fmt.Errorf("clip %d 源范围无效", index)
		}
		duration := selection.SourceEndFrame - selection.SourceStartFrame
		clipID := fmt.Sprintf("clip_v%d_%03d", version, index+1)
		parentBlockID := fmt.Sprintf("block_%03d", index+1)
		primary.Clips = append(primary.Clips, Clip{
			TimelineClipID: clipID, TrackID: primary.TrackID, AssetID: selection.AssetID,
			AssetKind: selection.AssetKind,
			Role:      selection.Role, TimelineStartFrame: cursor, TimelineEndFrame: cursor + duration,
			SourceStartFrame: selection.SourceStartFrame,
			SourceEndFrame:   selection.SourceEndFrame,
			PlaybackRate:     1, LockPolicy: "free", ParentBlockID: parentBlockID,
			Linked: selection.HasAudio,
		})
		if selection.HasAudio {
			originalAudio.Clips = append(originalAudio.Clips, Clip{
				TimelineClipID: clipID + "_audio", TrackID: originalAudio.TrackID,
				AssetID: selection.AssetID, AssetKind: selection.AssetKind,
				Role: "original_audio", TimelineStartFrame: cursor, TimelineEndFrame: cursor + duration,
				SourceStartFrame: selection.SourceStartFrame, SourceEndFrame: selection.SourceEndFrame,
				PlaybackRate: 1, LockPolicy: "free", ParentBlockID: parentBlockID, Linked: true,
			})
		}
		cursor += duration
	}
	document.DurationFrames = cursor
	return document, nil
}

func Empty(draftID string, version int) Document {
	document := Document{
		TimelineID: fmt.Sprintf("%s:v%d", draftID, version), DraftID: draftID,
		Version: version, FPS: DefaultFPS, DurationFrames: 1,
	}
	for _, required := range requiredTracks {
		document.Tracks = append(document.Tracks, Track{
			TrackID: required.ID, TrackType: required.Type, Clips: []Clip{},
		})
	}
	return document
}

func Validate(document Document) ValidationReport {
	report := ValidationReport{Valid: true, Checks: []string{
		"schema", "required_tracks", "clip_ranges", "primary_visual_coverage",
	}, Issues: []ValidationIssue{}}
	add := func(code, message string) {
		report.Valid = false
		report.Issues = append(report.Issues, ValidationIssue{Code: code, Message: message})
	}
	if document.FPS <= 0 || document.DurationFrames <= 0 || document.Version < 1 {
		add("invalid_document", "fps、duration_frames、version 必须为正数")
	}
	tracks := map[string]Track{}
	for _, track := range document.Tracks {
		if _, duplicate := tracks[track.TrackID]; duplicate {
			add("duplicate_track", track.TrackID)
		}
		tracks[track.TrackID] = track
		if track.GainDB < -60 || track.GainDB > 12 {
			add("invalid_track_gain", track.TrackID)
		}
		for _, clip := range track.Clips {
			if clip.TrackID != track.TrackID {
				add("clip_track_mismatch", clip.TimelineClipID)
			}
			if clip.TimelineStartFrame < 0 || clip.TimelineEndFrame <= clip.TimelineStartFrame || clip.TimelineEndFrame > document.DurationFrames {
				add("invalid_clip_range", clip.TimelineClipID)
			}
			if clip.AssetID != "" && clip.SourceEndFrame <= clip.SourceStartFrame {
				add("invalid_source_range", clip.TimelineClipID)
			}
			if clip.PlaybackRate < 0 {
				add("invalid_playback_rate", clip.TimelineClipID)
			}
			if clip.GainDB < -60 || clip.GainDB > 12 {
				add("invalid_clip_gain", clip.TimelineClipID)
			}
		}
	}
	for _, required := range requiredTracks {
		if _, exists := tracks[required.ID]; !exists {
			add("missing_track", required.ID)
		}
	}
	primary, exists := tracks["visual_base"]
	if !exists || len(primary.Clips) == 0 {
		add("empty_primary_visual", "主视觉轨没有 clip")
	} else {
		sorted := append([]Clip(nil), primary.Clips...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].TimelineStartFrame < sorted[j].TimelineStartFrame })
		cursor := 0
		for _, clip := range sorted {
			if clip.TimelineStartFrame != cursor {
				add("primary_visual_gap_or_overlap", clip.TimelineClipID)
			}
			cursor = clip.TimelineEndFrame
		}
		if cursor != document.DurationFrames {
			add("primary_visual_not_full_coverage", fmt.Sprintf("coverage=%d duration=%d", cursor, document.DurationFrames))
		}
	}
	return report
}

func ApplyPatch(document Document, operation map[string]any) (Document, error) {
	copy, err := clone(document)
	if err != nil {
		return Document{}, err
	}
	kind := stringValue(operation["kind"])
	switch kind {
	case "trim_clip":
		err = trimClip(&copy, operation)
	case "split_clip":
		err = splitClip(&copy, operation)
	case "reorder_clip":
		err = reorderClip(&copy, operation)
	case "move_clip":
		err = moveClip(&copy, operation)
	case "trim_clip_edge":
		err = trimClipEdge(&copy, operation)
	case "delete_clip":
		err = deleteClip(&copy, operation)
	case "set_track_state":
		err = setTrackState(&copy, operation)
	case "set_clip_linked":
		err = setClipLinked(&copy, operation)
	case "insert_subtitle":
		err = insertSubtitle(&copy, operation)
	case "delete_range":
		err = deleteRange(&copy, operation)
	case "insert_clip":
		err = insertClip(&copy, operation)
	case "replace_clip":
		err = replaceClip(&copy, operation)
	case "set_playback_rate":
		err = setPlaybackRate(&copy, operation)
	case "adjust_gain":
		err = updateEditableClip(&copy, operation, func(_ *Track, clip *Clip) error {
			gain := numberValue(operation["gain_db"])
			if gain < -60 || gain > 12 {
				return errors.New("gain_db 必须在 [-60,12] 范围内")
			}
			clip.GainDB = gain
			return nil
		})
	case "edit_subtitle_text":
		err = updateEditableClip(&copy, operation, func(track *Track, clip *Clip) error {
			if track.TrackID != "subtitles" {
				return errors.New("只能编辑字幕轨文字")
			}
			text := strings.TrimSpace(stringValue(operation["text"]))
			if text == "" {
				return errors.New("字幕文字不能为空")
			}
			clip.Text = text
			return nil
		})
	case "remove_track_clips":
		err = removeTrackClips(&copy, stringValue(operation["track_id"]))
	default:
		return Document{}, fmt.Errorf("不支持的 patch op: %s", kind)
	}
	if err != nil {
		return Document{}, err
	}
	copy.Version++
	copy.TimelineID = fmt.Sprintf("%s:v%d", copy.DraftID, copy.Version)
	return copy, nil
}

func Inspect(document Document) string {
	counts := map[string]int{}
	for _, track := range document.Tracks {
		counts[track.TrackID] = len(track.Clips)
	}
	return fmt.Sprintf(
		"时间线 v%d：%.2f 秒，%d fps；主视觉 %d 段，叠加 %d 段，字幕 %d 段。",
		document.Version, float64(document.DurationFrames)/float64(document.FPS), document.FPS,
		counts["visual_base"], counts["visual_overlay"], counts["subtitles"],
	)
}

func ToMap(document Document) (map[string]any, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(data, &result)
	return result, err
}

func trimClip(document *Document, operation map[string]any) error {
	return updateEditableClip(document, operation, func(_ *Track, clip *Clip) error {
		start, startErr := frameValue(operation, "source_start_frame")
		end, endErr := frameValue(operation, "source_end_frame")
		if startErr != nil || endErr != nil {
			return errors.Join(startErr, endErr)
		}
		if start < 0 || end <= start {
			return errors.New("trim_clip 源范围无效")
		}
		oldDuration := clip.TimelineEndFrame - clip.TimelineStartFrame
		clip.SourceStartFrame = start
		clip.SourceEndFrame = end
		rate := clip.PlaybackRate
		if rate <= 0 {
			rate = 1
		}
		newDuration := max(1, int(math.Round(float64(end-start)/rate)))
		clip.TimelineEndFrame = clip.TimelineStartFrame + newDuration
		shiftAfter(document, clip.TimelineStartFrame+oldDuration, newDuration-oldDuration)
		return nil
	})
}

func splitClip(document *Document, operation map[string]any) error {
	location, err := editableLocation(document, operation)
	if err != nil {
		return err
	}
	splitFrame, err := frameValue(operation, "split_frame")
	if err != nil {
		return err
	}
	selected := document.Tracks[location.trackIndex].Clips[location.clipIndex]
	groupID := ""
	if selected.Linked {
		groupID = selected.ParentBlockID
	}
	rightGroupID := groupID
	if rightGroupID != "" {
		rightGroupID = fmt.Sprintf("%s_split_%d", groupID, splitFrame)
	}
	for _, member := range linkedGroup(document, groupID) {
		track := document.Tracks[member.trackIndex]
		clip := track.Clips[member.clipIndex]
		if track.Locked {
			return fmt.Errorf("轨道 %s 已锁定", track.TrackID)
		}
		if splitFrame <= clip.TimelineStartFrame || splitFrame >= clip.TimelineEndFrame {
			return errors.New("联动片段未覆盖同一个切点")
		}
	}
	selectedID := selected.TimelineClipID
	for trackIndex := range document.Tracks {
		track := &document.Tracks[trackIndex]
		next := make([]Clip, 0, len(track.Clips)+1)
		for _, clip := range track.Clips {
			shouldSplit := clip.TimelineClipID == selectedID
			if groupID != "" {
				shouldSplit = shouldSplit || (clip.Linked && clip.ParentBlockID == groupID)
			}
			if !shouldSplit {
				next = append(next, clip)
				continue
			}
			left, right, splitErr := splitClipValue(clip, splitFrame, groupID, rightGroupID)
			if splitErr != nil {
				return splitErr
			}
			if clip.TimelineClipID == selectedID {
				right.TimelineClipID = valueOr(
					stringValue(operation["new_timeline_clip_id"]),
					right.TimelineClipID,
				)
			}
			next = append(next, left, right)
		}
		track.Clips = next
	}
	return nil
}

func splitClipValue(clip Clip, splitFrame int, leftGroupID, rightGroupID string) (Clip, Clip, error) {
	if clip.AssetID == "" || splitFrame <= clip.TimelineStartFrame || splitFrame >= clip.TimelineEndFrame {
		return Clip{}, Clip{}, errors.New("split_clip 切点必须位于可分割素材片段内部")
	}
	sourceSplit := clip.SourceStartFrame + int(math.Round(
		float64(splitFrame-clip.TimelineStartFrame)*effectiveRate(clip),
	))
	if sourceSplit <= clip.SourceStartFrame || sourceSplit >= clip.SourceEndFrame {
		return Clip{}, Clip{}, errors.New("split_clip 源切点无效")
	}
	left := clip
	left.TimelineEndFrame = splitFrame
	left.SourceEndFrame = sourceSplit
	right := clip
	right.TimelineClipID = fmt.Sprintf("%s_split_%d", clip.TimelineClipID, splitFrame)
	right.TimelineStartFrame = splitFrame
	right.SourceStartFrame = sourceSplit
	if clip.Linked {
		left.ParentBlockID = leftGroupID
		right.ParentBlockID = rightGroupID
	}
	return left, right, nil
}

// reorder_clip 只改变主视觉片段顺序，并重新紧密排布该轨道。其他轨道保留
// 时间坐标，因此不会制造主视觉空洞，也不会绕开后续 Validate/Reducer 门禁。
func reorderClip(document *Document, operation map[string]any) error {
	id := valueOr(stringValue(operation["timeline_clip_id"]), stringValue(operation["clip_id"]))
	if id == "" {
		return errors.New("reorder_clip 缺少 timeline_clip_id")
	}
	targetFrame, err := frameValue(operation, "target_frame")
	if err != nil {
		return err
	}
	if targetFrame < 0 || targetFrame > document.DurationFrames {
		return errors.New("reorder_clip target_frame 超出时间线")
	}
	track := trackByID(document, "visual_base")
	if track == nil || track.Locked {
		return errors.New("reorder_clip 缺少主视觉轨或轨道已锁定")
	}
	for _, clip := range track.Clips {
		if !clip.Linked || clip.ParentBlockID == "" {
			continue
		}
		for _, member := range linkedGroup(document, clip.ParentBlockID) {
			memberTrack := document.Tracks[member.trackIndex]
			if memberTrack.TrackID != "visual_base" && memberTrack.Locked {
				return fmt.Errorf("联动轨道 %s 已锁定", memberTrack.TrackID)
			}
		}
	}
	clips := append([]Clip(nil), track.Clips...)
	sort.SliceStable(clips, func(i, j int) bool {
		return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
	})
	clipIndex := -1
	var moving Clip
	for index, clip := range clips {
		if clip.TimelineClipID == id {
			clipIndex = index
			moving = clip
			break
		}
	}
	if clipIndex < 0 {
		return fmt.Errorf("clip 不存在或不在主视觉轨: %s", id)
	}
	clips = append(clips[:clipIndex], clips[clipIndex+1:]...)
	insertAt := len(clips)
	for index, clip := range clips {
		midpoint := clip.TimelineStartFrame + (clip.TimelineEndFrame-clip.TimelineStartFrame)/2
		if targetFrame < midpoint {
			insertAt = index
			break
		}
	}
	clips = append(clips, Clip{})
	copy(clips[insertAt+1:], clips[insertAt:])
	clips[insertAt] = moving
	cursor := 0
	for index := range clips {
		duration := clips[index].TimelineEndFrame - clips[index].TimelineStartFrame
		clips[index].TimelineStartFrame = cursor
		clips[index].TimelineEndFrame = cursor + duration
		cursor += duration
	}
	track.Clips = clips
	for _, clip := range track.Clips {
		syncLinkedTiming(document, clip)
	}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID != "visual_base" {
			sortTrack(&document.Tracks[trackIndex])
		}
	}
	return nil
}

func syncLinkedTiming(document *Document, primary Clip) {
	if !primary.Linked || primary.ParentBlockID == "" {
		return
	}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID == "visual_base" {
			continue
		}
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.Linked && clip.ParentBlockID == primary.ParentBlockID {
				clip.TimelineStartFrame = primary.TimelineStartFrame
				clip.TimelineEndFrame = primary.TimelineEndFrame
			}
		}
	}
}

func deleteRange(document *Document, operation map[string]any) error {
	start, startErr := frameValue(operation, "start_frame")
	end, endErr := frameValue(operation, "end_frame")
	if startErr != nil || endErr != nil {
		return errors.Join(startErr, endErr)
	}
	if start < 0 || end <= start || end > document.DurationFrames || end-start >= document.DurationFrames {
		return errors.New("delete_range 范围无效")
	}
	if err := ensureRippleUnlocked(document, start, ""); err != nil {
		return err
	}
	delta := end - start
	for trackIndex := range document.Tracks {
		clips := document.Tracks[trackIndex].Clips
		kept := clips[:0]
		for _, clip := range clips {
			if clip.TimelineEndFrame <= start {
				kept = append(kept, clip)
				continue
			}
			if clip.TimelineStartFrame >= end {
				clip.TimelineStartFrame -= delta
				clip.TimelineEndFrame -= delta
				kept = append(kept, clip)
				continue
			}
			if clip.TimelineStartFrame < start && clip.TimelineEndFrame > end {
				clip.TimelineEndFrame -= delta
				if clip.AssetID != "" {
					clip.SourceEndFrame -= int(math.Round(float64(delta) * effectiveRate(clip)))
				}
				kept = append(kept, clip)
			} else if clip.TimelineStartFrame < start {
				clip.TimelineEndFrame = start
				if clip.AssetID != "" {
					clip.SourceEndFrame = clip.SourceStartFrame + int(math.Round(
						float64(clip.TimelineEndFrame-clip.TimelineStartFrame)*effectiveRate(clip),
					))
				}
				kept = append(kept, clip)
			} else if clip.TimelineEndFrame > end {
				removed := end - clip.TimelineStartFrame
				clip.TimelineStartFrame = start
				clip.TimelineEndFrame -= delta
				if clip.AssetID != "" {
					clip.SourceStartFrame += int(math.Round(float64(removed) * effectiveRate(clip)))
				}
				kept = append(kept, clip)
			}
		}
		document.Tracks[trackIndex].Clips = kept
	}
	document.DurationFrames -= delta
	for trackIndex := range document.Tracks {
		sortTrack(&document.Tracks[trackIndex])
	}
	return nil
}

func insertClip(document *Document, operation map[string]any) error {
	assetID := stringValue(operation["asset_id"])
	start, startErr := frameValue(operation, "source_start_frame")
	end, endErr := frameValue(operation, "source_end_frame")
	if startErr != nil || endErr != nil {
		return errors.Join(startErr, endErr)
	}
	if assetID == "" || start < 0 || end <= start {
		return errors.New("insert_clip 参数无效")
	}
	track := trackByID(document, valueOr(stringValue(operation["track_id"]), "visual_base"))
	if track == nil || track.Locked {
		return errors.New("insert_clip track 不存在或已锁定")
	}
	duration := end - start
	startFrame := document.DurationFrames
	clip := Clip{
		TimelineClipID: valueOr(stringValue(operation["timeline_clip_id"]), fmt.Sprintf("clip_v%d_%03d", document.Version+1, len(track.Clips)+1)),
		TrackID:        track.TrackID, AssetID: assetID, Role: valueOr(stringValue(operation["role"]), "b_roll"),
		TimelineStartFrame: startFrame, TimelineEndFrame: startFrame + duration,
		SourceStartFrame: start, SourceEndFrame: end,
		PlaybackRate: 1, LockPolicy: "free",
	}
	track.Clips = append(track.Clips, clip)
	if track.TrackID == "visual_base" {
		document.DurationFrames += duration
	}
	return nil
}

func replaceClip(document *Document, operation map[string]any) error {
	return updateEditableClip(document, operation, func(_ *Track, clip *Clip) error {
		assetID := stringValue(operation["asset_id"])
		if assetID == "" {
			return errors.New("replace_clip 缺少 asset_id")
		}
		clip.AssetID = assetID
		if role := stringValue(operation["role"]); role != "" {
			clip.Role = role
		}
		return nil
	})
}

func setPlaybackRate(document *Document, operation map[string]any) error {
	return updateEditableClip(document, operation, func(_ *Track, clip *Clip) error {
		rate := numberValue(operation["playback_rate"])
		if rate <= 0 || rate > 8 {
			return errors.New("playback_rate 必须在 (0,8]")
		}
		oldDuration := clip.TimelineEndFrame - clip.TimelineStartFrame
		newDuration := max(1, int(math.Round(float64(clip.SourceEndFrame-clip.SourceStartFrame)/rate)))
		clip.PlaybackRate = rate
		clip.TimelineEndFrame = clip.TimelineStartFrame + newDuration
		shiftAfter(document, clip.TimelineStartFrame+oldDuration, newDuration-oldDuration)
		return nil
	})
}

func removeTrackClips(document *Document, trackID string) error {
	track := trackByID(document, trackID)
	if track == nil || trackID == "visual_base" || track.Locked {
		return errors.New("不能清空不存在、锁定的轨道或主视觉轨")
	}
	track.Clips = []Clip{}
	return nil
}

func shiftAfter(document *Document, boundary, delta int) {
	if delta == 0 {
		return
	}
	for trackIndex := range document.Tracks {
		for clipIndex := range document.Tracks[trackIndex].Clips {
			clip := &document.Tracks[trackIndex].Clips[clipIndex]
			if clip.TimelineStartFrame >= boundary {
				clip.TimelineStartFrame += delta
				clip.TimelineEndFrame += delta
			}
		}
	}
	document.DurationFrames += delta
}

func trackByID(document *Document, trackID string) *Track {
	for index := range document.Tracks {
		if document.Tracks[index].TrackID == trackID {
			return &document.Tracks[index]
		}
	}
	return nil
}

func clone(document Document) (Document, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return Document{}, err
	}
	var result Document
	err = json.Unmarshal(data, &result)
	return result, err
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func frameValue(operation map[string]any, key string) (int, error) {
	value, exists := operation[key]
	if !exists {
		return 0, fmt.Errorf("patch op 缺少 %s", key)
	}
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		return typed, nil
	case int64:
		if int64(int(typed)) != typed {
			return 0, fmt.Errorf("%s 超出整数范围", key)
		}
		return int(typed), nil
	default:
		return 0, fmt.Errorf("%s 必须是整数帧", key)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return 0, fmt.Errorf("%s 必须是整数帧", key)
	}
	converted := int(number)
	if float64(converted) != number {
		return 0, fmt.Errorf("%s 超出整数范围", key)
	}
	return converted, nil
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
