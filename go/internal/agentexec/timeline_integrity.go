package agentexec

import (
	"context"
	"math"
	"sort"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

var independentAudioTrackIDs = []string{"bgm", "sfx"}

func preserveIndependentAudioForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]timeline.Track {
	touched := touchedTrackIDsForOperation(current, operation)
	preserved := map[string]timeline.Track{}
	for _, track := range current.Tracks {
		if !ContainsString(independentAudioTrackIDs, track.TrackID) {
			continue
		}
		if _, changed := touched[track.TrackID]; changed {
			continue
		}
		preserved[track.TrackID] = copyTimelineTrack(track)
	}
	return preserved
}

func touchedTrackIDsForOperation(
	current timeline.Document,
	operation map[string]any,
) map[string]struct{} {
	clipTracks := map[string]string{}
	for _, track := range current.Tracks {
		for _, clip := range track.Clips {
			clipTracks[clip.TimelineClipID] = track.TrackID
		}
	}
	touched := map[string]struct{}{}
	kind := StringValue(operation["kind"])
	if kind == "delete_range" || kind == "delete_source_range" {
		for _, trackID := range independentAudioTrackIDs {
			touched[trackID] = struct{}{}
		}
	}
	trackID := StringValue(operation["track_id"])
	if kind == "insert_clip" && trackID == "" {
		trackID = "visual_base"
	}
	if trackID != "" {
		touched[trackID] = struct{}{}
	}
	if targetTrackID := StringValue(operation["target_track_id"]); targetTrackID != "" {
		touched[targetTrackID] = struct{}{}
	}
	clipID := StringValue(operation["timeline_clip_id"])
	if sourceTrackID := clipTracks[clipID]; sourceTrackID != "" {
		touched[sourceTrackID] = struct{}{}
	}
	return touched
}

func restoreIndependentAudioTracks(
	document *timeline.Document,
	preserved map[string]timeline.Track,
) {
	for trackIndex := range document.Tracks {
		track, exists := preserved[document.Tracks[trackIndex].TrackID]
		if !exists {
			continue
		}
		document.Tracks[trackIndex] = copyTimelineTrack(track)
	}
}

func copyTimelineTrack(track timeline.Track) timeline.Track {
	copy := track
	copy.Clips = append([]timeline.Clip(nil), track.Clips...)
	return copy
}

func BeatAlignmentData(document timeline.Document) map[string]any {
	beatFrames := []int{}
	strongFrames := []int{}
	downbeatFrames := []int{}
	for _, track := range document.Tracks {
		if track.TrackID != "bgm" {
			continue
		}
		for _, clip := range track.Clips {
			if explicitGrid, ok := clip.Metadata["beat_grid"].(map[string]any); ok {
				beatFrames = append(beatFrames, mapEffectFramesToTimeline(clip, explicitGrid["beat_frames"])...)
				strongFrames = append(strongFrames, mapEffectFramesToTimeline(clip, explicitGrid["strong_beat_frames"])...)
				downbeatFrames = append(downbeatFrames, mapEffectFramesToTimeline(clip, explicitGrid["downbeat_frames"])...)
			}
			for _, effect := range clip.Effects {
				if StringValue(effect["kind"]) != "beat_grid" {
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
		if ContainsFrame(beatFrames, frame) {
			onBeat++
		} else {
			offBeat = append(offBeat, frame)
		}
		if ContainsFrame(strongFrames, frame) || ContainsFrame(downbeatFrames, frame) {
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
	for _, sourceFrame := range EffectFrameValues(value) {
		if sourceFrame < clip.SourceStartFrame || sourceFrame > clip.SourceEndFrame {
			continue
		}
		frames = append(frames, clip.TimelineStartFrame+int(math.Round(
			float64(sourceFrame-clip.SourceStartFrame)/rate,
		)))
	}
	return frames
}

func EffectFrameValues(value any) []int {
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
			if frame, ok := NumericValue(raw); ok && frame >= 0 && frame == math.Trunc(frame) {
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

func ContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func StringValue(value any) string {
	text, _ := value.(string)
	return text
}

func ValueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func (exec *Executor) enrichTimelineOperation(
	ctx context.Context,
	draftID string,
	original map[string]any,
) (map[string]any, error) {
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assetByID := make(map[string]storage.Asset, len(assets))
	for _, asset := range assets {
		assetByID[asset.ID] = asset
	}

	operation := make(map[string]any, len(original)+2)
	for key, value := range original {
		operation[key] = value
	}
	switch StringValue(operation["kind"]) {
	case "insert_clip", "replace_clip":
		asset, exists := assetByID[StringValue(operation["asset_id"])]
		if !exists {
			break
		}
		if StringValue(operation["asset_kind"]) == "" {
			operation["asset_kind"] = asset.Kind
		}
		if StringValue(operation["kind"]) == "replace_clip" {
			break
		}
		if ValueOr(StringValue(operation["track_id"]), "visual_base") != "visual_base" {
			break
		}
		if _, explicit := operation["include_original_audio"]; !explicit {
			hasAudio, _ := asset.Probe["has_audio"].(bool)
			operation["include_original_audio"] = asset.Kind == "video" && hasAudio
		}
	}
	return operation, nil
}
