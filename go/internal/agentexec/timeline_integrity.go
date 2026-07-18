package agentexec

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

var independentAudioTrackIDs = []string{"bgm", "sfx"}

// prepareTimelineBatch keeps a full primary-track replacement executable as one
// atomic request. Models naturally emit "delete old clips, then insert new
// clips"; applying that order literally reaches an invalid empty primary track
// halfway through. Moving the new primary inserts before the old deletions keeps
// every intermediate document editable without changing the final ordering.
func prepareTimelineBatch(
	current timeline.Document,
	operations []map[string]any,
) ([]map[string]any, map[string]timeline.Track) {
	planned := reorderFullPrimaryReplacement(current, operations)
	return planned, snapshotUntouchedIndependentAudio(current, planned)
}

func reorderFullPrimaryReplacement(
	current timeline.Document,
	operations []map[string]any,
) []map[string]any {
	primaryIDs := map[string]struct{}{}
	for _, track := range current.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			primaryIDs[clip.TimelineClipID] = struct{}{}
		}
	}
	if len(primaryIDs) == 0 {
		return operations
	}
	deleted := map[string]struct{}{}
	visualInsertIndexes := map[int]struct{}{}
	for index, operation := range operations {
		switch stringValue(operation["kind"]) {
		case "delete_clip":
			clipID := valueOr(stringValue(operation["timeline_clip_id"]), stringValue(operation["clip_id"]))
			if _, isPrimary := primaryIDs[clipID]; isPrimary {
				deleted[clipID] = struct{}{}
			}
		case "insert_clip":
			if valueOr(stringValue(operation["track_id"]), "visual_base") == "visual_base" {
				visualInsertIndexes[index] = struct{}{}
			}
		}
	}
	if len(deleted) != len(primaryIDs) || len(visualInsertIndexes) == 0 {
		return operations
	}
	reordered := make([]map[string]any, 0, len(operations))
	for index, operation := range operations {
		if _, isInsert := visualInsertIndexes[index]; isInsert {
			reordered = append(reordered, operation)
		}
	}
	for index, operation := range operations {
		if _, isInsert := visualInsertIndexes[index]; !isInsert {
			reordered = append(reordered, operation)
		}
	}
	return reordered
}

func snapshotUntouchedIndependentAudio(
	current timeline.Document,
	operations []map[string]any,
) map[string]timeline.Track {
	touched := touchedTrackIDs(current, operations)
	preserved := map[string]timeline.Track{}
	for _, track := range current.Tracks {
		if !containsString(independentAudioTrackIDs, track.TrackID) {
			continue
		}
		if _, changed := touched[track.TrackID]; changed {
			continue
		}
		preserved[track.TrackID] = copyTimelineTrack(track)
	}
	return preserved
}

func touchedTrackIDs(current timeline.Document, operations []map[string]any) map[string]struct{} {
	clipTracks := map[string]string{}
	for _, track := range current.Tracks {
		for _, clip := range track.Clips {
			clipTracks[clip.TimelineClipID] = track.TrackID
		}
	}
	touched := map[string]struct{}{}
	for _, operation := range operations {
		kind := stringValue(operation["kind"])
		if kind == "delete_range" {
			for _, trackID := range independentAudioTrackIDs {
				touched[trackID] = struct{}{}
			}
		}
		trackID := stringValue(operation["track_id"])
		if kind == "insert_clip" && trackID == "" {
			trackID = "visual_base"
		}
		if trackID != "" {
			touched[trackID] = struct{}{}
		}
		if targetTrackID := stringValue(operation["target_track_id"]); targetTrackID != "" {
			touched[targetTrackID] = struct{}{}
		}
		clipID := valueOr(stringValue(operation["timeline_clip_id"]), stringValue(operation["clip_id"]))
		if sourceTrackID := clipTracks[clipID]; sourceTrackID != "" {
			touched[sourceTrackID] = struct{}{}
		}
		if kind == "insert_clip" && clipID != "" {
			clipTracks[clipID] = trackID
		}
	}
	return touched
}

func restoreIndependentAudioTracks(
	document *timeline.Document,
	preserved map[string]timeline.Track,
) error {
	for trackIndex := range document.Tracks {
		track, exists := preserved[document.Tracks[trackIndex].TrackID]
		if !exists {
			continue
		}
		for _, clip := range track.Clips {
			if clip.TimelineEndFrame > document.DurationFrames {
				return fmt.Errorf(
					"主视频批量编辑会把时间线缩到 %d 帧，但未编辑的 %s 片段 %s 仍延伸到 %d 帧",
					document.DurationFrames, track.TrackID, clip.TimelineClipID, clip.TimelineEndFrame,
				)
			}
		}
		document.Tracks[trackIndex] = copyTimelineTrack(track)
	}
	return nil
}

func copyTimelineTrack(track timeline.Track) timeline.Track {
	copy := track
	copy.Clips = append([]timeline.Clip(nil), track.Clips...)
	return copy
}

func hasBeatGrid(effects []map[string]any) bool {
	for _, effect := range effects {
		if stringValue(effect["kind"]) == "beat_grid" {
			return true
		}
	}
	return false
}

func beatAlignmentData(document timeline.Document) map[string]any {
	beatFrames := []int{}
	strongFrames := []int{}
	downbeatFrames := []int{}
	for _, track := range document.Tracks {
		if track.TrackID != "bgm" {
			continue
		}
		for _, clip := range track.Clips {
			for _, effect := range clip.Effects {
				if stringValue(effect["kind"]) != "beat_grid" {
					continue
				}
				beatFrames = append(beatFrames, mapEffectFramesToTimeline(clip, effect["beat_frames"])...)
				strongFrames = append(strongFrames, mapEffectFramesToTimeline(clip, effect["strong_beat_frames"])...)
				downbeatFrames = append(downbeatFrames, mapEffectFramesToTimeline(clip, effect["downbeat_frames"])...)
			}
		}
	}
	beatFrames = sortedUniqueInts(beatFrames)
	strongFrames = sortedUniqueInts(strongFrames)
	downbeatFrames = sortedUniqueInts(downbeatFrames)
	cutFrames := []int{}
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		clips := append([]timeline.Clip(nil), track.Clips...)
		sort.SliceStable(clips, func(i, j int) bool {
			return clips[i].TimelineStartFrame < clips[j].TimelineStartFrame
		})
		for index, clip := range clips {
			if clip.TimelineEndFrame > 0 && clip.TimelineEndFrame < document.DurationFrames {
				if index+1 < len(clips) && clipsHaveContinuousSourceBoundary(clip, clips[index+1]) {
					continue
				}
				cutFrames = append(cutFrames, clip.TimelineEndFrame)
			}
		}
	}
	onBeat := 0
	onAccent := 0
	offBeat := []int{}
	for _, frame := range cutFrames {
		if containsFrame(beatFrames, frame) {
			onBeat++
		} else {
			offBeat = append(offBeat, frame)
		}
		if containsFrame(strongFrames, frame) || containsFrame(downbeatFrames, frame) {
			onAccent++
		}
	}
	ratio := 0.0
	if len(cutFrames) > 0 {
		ratio = math.Round(float64(onBeat)/float64(len(cutFrames))*1000) / 1000
	}
	result := map[string]any{
		"beat_grid_present":     len(beatFrames) > 0,
		"cut_count":             len(cutFrames),
		"on_beat_cut_count":     onBeat,
		"on_accent_cut_count":   onAccent,
		"alignment_ratio":       ratio,
		"off_beat_cut_frames":   offBeat,
		"all_cuts_on_beat_grid": len(cutFrames) > 0 && onBeat == len(cutFrames),
		"beat_marker_count":     len(beatFrames),
		"strong_marker_count":   len(strongFrames),
		"downbeat_marker_count": len(downbeatFrames),
	}
	if len(beatFrames) == 0 {
		result["warning"] = "BGM 缺少 beat_grid 元数据；结构校验不能证明画面切点已卡音乐节拍"
	}
	return result
}

func clipsHaveContinuousSourceBoundary(previous, next timeline.Clip) bool {
	return previous.AssetID != "" && previous.AssetID == next.AssetID &&
		previous.SourceEndFrame == next.SourceStartFrame
}

func mapEffectFramesToTimeline(clip timeline.Clip, value any) []int {
	rate := clip.PlaybackRate
	if rate <= 0 {
		rate = 1
	}
	frames := []int{}
	for _, sourceFrame := range effectFrameValues(value) {
		if sourceFrame < clip.SourceStartFrame || sourceFrame > clip.SourceEndFrame {
			continue
		}
		frames = append(frames, clip.TimelineStartFrame+int(math.Round(
			float64(sourceFrame-clip.SourceStartFrame)/rate,
		)))
	}
	return frames
}

func effectFrameValues(value any) []int {
	result := []int{}
	switch frames := value.(type) {
	case []int:
		result = append(result, frames...)
	case []float64:
		for _, frame := range frames {
			if !math.IsNaN(frame) && !math.IsInf(frame, 0) && frame >= 0 && frame == math.Trunc(frame) {
				result = append(result, int(frame))
			}
		}
	case []any:
		for _, raw := range frames {
			if frame, ok := numericValue(raw); ok && frame >= 0 && frame == math.Trunc(frame) {
				result = append(result, int(frame))
			}
		}
	}
	return result
}

func sortedUniqueInts(values []int) []int {
	sort.Ints(values)
	result := values[:0]
	previous := -1
	for _, value := range values {
		if value == previous {
			continue
		}
		result = append(result, value)
		previous = value
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
