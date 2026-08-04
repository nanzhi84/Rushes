package timeline

import (
	"fmt"
	"math"
	"reflect"
)

// AdjacentClipMerge describes one safely normalized boundary removed by the
// Harness before terminal validation. The retained clip keeps the stable ID of
// the left-hand segment so later audit records can explain exactly what changed.
type AdjacentClipMerge struct {
	TrackID                  string `json:"track_id"`
	KeptTimelineClipID       string `json:"kept_timeline_clip_id"`
	RemovedTimelineClipID    string `json:"removed_timeline_clip_id"`
	BoundaryFrame            int    `json:"boundary_frame"`
	AssetID                  string `json:"asset_id"`
	Mode                     string `json:"mode"`
	ShotID                   string `json:"shot_id,omitempty"`
	SourceOverlapFrames      int    `json:"source_overlap_frames,omitempty"`
	PreviousSourceEndFrame   int    `json:"previous_source_end_frame,omitempty"`
	NormalizedSourceEndFrame int    `json:"normalized_source_end_frame,omitempty"`
}

// AdjacentClipOverlapRepair is a Harness-approved continuity repair. The
// caller proves that both clips resolve to the same indexed shot and that the
// normalized source range remains within the asset. This package rechecks all
// rendering parameters and frame arithmetic before applying it.
type AdjacentClipOverlapRepair struct {
	TrackID                  string
	KeptTimelineClipID       string
	RemovedTimelineClipID    string
	NormalizedSourceEndFrame int
	ShotID                   string
}

// CoalesceRedundantAdjacentClips removes structural split residue from the
// primary visual track. It deliberately handles only boundaries that are
// provably render-equivalent: the same source continues at the same rate with
// identical parameters and no transition, linked group, effect or metadata
// difference at the boundary.
//
// The returned document is a deep copy and keeps the input version. Versioning
// and durable audit persistence remain the Reducer caller's responsibility.
func CoalesceRedundantAdjacentClips(
	document Document,
) (Document, []AdjacentClipMerge, error) {
	copy, err := clone(document)
	if err != nil {
		return Document{}, nil, err
	}
	merges := []AdjacentClipMerge{}
	for trackIndex := range copy.Tracks {
		track := &copy.Tracks[trackIndex]
		if track.TrackID != "visual_base" || track.Locked || len(track.Clips) < 2 {
			continue
		}
		next := make([]Clip, 0, len(track.Clips))
		for _, candidate := range track.Clips {
			if len(next) == 0 {
				next = append(next, candidate)
				continue
			}
			left := &next[len(next)-1]
			if !renderEquivalentAdjacentVisualClips(*left, candidate) {
				next = append(next, candidate)
				continue
			}
			boundary := left.TimelineEndFrame
			merges = append(merges, AdjacentClipMerge{
				TrackID: track.TrackID, KeptTimelineClipID: left.TimelineClipID,
				RemovedTimelineClipID: candidate.TimelineClipID,
				BoundaryFrame:         boundary, AssetID: left.AssetID,
				Mode: "render_equivalent",
			})
			left.TimelineEndFrame = candidate.TimelineEndFrame
			left.SourceEndFrame = candidate.SourceEndFrame
			left.FadeOutFrames = candidate.FadeOutFrames
			syncClipPlacementMetadata(left)
		}
		track.Clips = next
	}
	return copy, merges, nil
}

func renderEquivalentAdjacentVisualClips(left, right Clip) bool {
	if !mergeCompatibleAdjacentVisualClips(left, right) ||
		left.SourceEndFrame != right.SourceStartFrame {
		return false
	}
	rate := effectiveRate(left)
	leftTimelineDuration := left.TimelineEndFrame - left.TimelineStartFrame
	rightTimelineDuration := right.TimelineEndFrame - right.TimelineStartFrame
	leftSourceDuration := left.SourceEndFrame - left.SourceStartFrame
	rightSourceDuration := right.SourceEndFrame - right.SourceStartFrame
	mergedSourceDuration := right.SourceEndFrame - left.SourceStartFrame
	return leftTimelineDuration > 0 && rightTimelineDuration > 0 &&
		leftSourceDuration == int(math.Round(float64(leftTimelineDuration)*rate)) &&
		rightSourceDuration == int(math.Round(float64(rightTimelineDuration)*rate)) &&
		mergedSourceDuration == int(math.Round(float64(leftTimelineDuration+rightTimelineDuration)*rate))
}

func mergeCompatibleAdjacentVisualClips(left, right Clip) bool {
	return !(left.AssetID == "" || left.AssetID != right.AssetID ||
		(left.AssetKind != "" && left.AssetKind != "video") ||
		(right.AssetKind != "" && right.AssetKind != "video") ||
		left.AssetKind != right.AssetKind || left.Role != right.Role ||
		left.Text != right.Text || left.SubtitleStyle != right.SubtitleStyle ||
		left.TrackID != right.TrackID || left.TrackID != "visual_base" ||
		left.Linked || right.Linked || left.ParentBlockID != "" || right.ParentBlockID != "" ||
		left.TimelineEndFrame != right.TimelineStartFrame ||
		left.FadeOutFrames != 0 || right.FadeInFrames != 0 ||
		math.Abs(left.GainDB-right.GainDB) > 0.000001 ||
		math.Abs(effectiveRate(left)-effectiveRate(right)) > 0.000001 ||
		!reflect.DeepEqual(left.Effects, right.Effects) ||
		!equivalentMergeMetadata(left.Metadata, right.Metadata))
}

// CanCoalesceOverlappingAdjacentClips exposes the structural half of the
// same-shot overlap decision. The Harness still has to prove shot identity,
// overlap size and asset bounds from persisted analysis facts.
func CanCoalesceOverlappingAdjacentClips(left, right Clip) bool {
	return mergeCompatibleAdjacentVisualClips(left, right) &&
		right.SourceStartFrame < left.SourceEndFrame &&
		right.SourceEndFrame > left.SourceEndFrame
}

// CoalesceApprovedOverlappingAdjacentClips repairs short same-shot source
// overlaps selected by the Harness. It keeps timeline coordinates stable while
// linearizing playback from the left clip's source start, thereby removing the
// repeated frames and the artificial timeline boundary together.
func CoalesceApprovedOverlappingAdjacentClips(
	document Document,
	repairs []AdjacentClipOverlapRepair,
) (Document, []AdjacentClipMerge, error) {
	copy, err := clone(document)
	if err != nil {
		return Document{}, nil, err
	}
	if len(repairs) == 0 {
		return copy, []AdjacentClipMerge{}, nil
	}
	type repairKey struct{ kept, removed string }
	requested := make(map[repairKey]AdjacentClipOverlapRepair, len(repairs))
	for _, repair := range repairs {
		key := repairKey{repair.KeptTimelineClipID, repair.RemovedTimelineClipID}
		if key.kept == "" || key.removed == "" || key.kept == key.removed {
			return Document{}, nil, fmt.Errorf("相邻重叠修复包含无效 clip ID")
		}
		if _, duplicate := requested[key]; duplicate {
			return Document{}, nil, fmt.Errorf("相邻重叠修复重复: %s -> %s", key.kept, key.removed)
		}
		requested[key] = repair
	}
	applied := map[repairKey]struct{}{}
	merges := []AdjacentClipMerge{}
	for trackIndex := range copy.Tracks {
		track := &copy.Tracks[trackIndex]
		if track.TrackID != "visual_base" || track.Locked || len(track.Clips) < 2 {
			continue
		}
		next := make([]Clip, 0, len(track.Clips))
		for _, candidate := range track.Clips {
			if len(next) == 0 {
				next = append(next, candidate)
				continue
			}
			left := &next[len(next)-1]
			key := repairKey{left.TimelineClipID, candidate.TimelineClipID}
			repair, selected := requested[key]
			if !selected {
				next = append(next, candidate)
				continue
			}
			if repair.TrackID != track.TrackID ||
				!CanCoalesceOverlappingAdjacentClips(*left, candidate) {
				return Document{}, nil, fmt.Errorf(
					"相邻重叠修复不满足连续参数约束: %s -> %s", key.kept, key.removed,
				)
			}
			timelineDuration := candidate.TimelineEndFrame - left.TimelineStartFrame
			expectedSourceEnd := left.SourceStartFrame + int(math.Round(
				float64(timelineDuration)*effectiveRate(*left),
			))
			if repair.NormalizedSourceEndFrame != expectedSourceEnd ||
				repair.NormalizedSourceEndFrame <= candidate.SourceEndFrame {
				return Document{}, nil, fmt.Errorf(
					"相邻重叠修复源帧终点无效: want=%d got=%d",
					expectedSourceEnd, repair.NormalizedSourceEndFrame,
				)
			}
			boundary := left.TimelineEndFrame
			previousSourceEnd := candidate.SourceEndFrame
			overlap := left.SourceEndFrame - candidate.SourceStartFrame
			merges = append(merges, AdjacentClipMerge{
				TrackID: track.TrackID, KeptTimelineClipID: left.TimelineClipID,
				RemovedTimelineClipID: candidate.TimelineClipID,
				BoundaryFrame:         boundary, AssetID: left.AssetID,
				Mode: "same_shot_overlap_repair", ShotID: repair.ShotID,
				SourceOverlapFrames: overlap, PreviousSourceEndFrame: previousSourceEnd,
				NormalizedSourceEndFrame: repair.NormalizedSourceEndFrame,
			})
			left.TimelineEndFrame = candidate.TimelineEndFrame
			left.SourceEndFrame = repair.NormalizedSourceEndFrame
			left.FadeOutFrames = candidate.FadeOutFrames
			syncClipPlacementMetadata(left)
			applied[key] = struct{}{}
		}
		track.Clips = next
	}
	if len(applied) != len(requested) {
		return Document{}, nil, fmt.Errorf(
			"相邻重叠修复未完全应用: requested=%d applied=%d", len(requested), len(applied),
		)
	}
	return copy, merges, nil
}

func equivalentMergeMetadata(left, right map[string]any) bool {
	if reflect.DeepEqual(left, right) {
		return true
	}
	if stringValue(left["kind"]) != "b_roll_semantic_anchor" ||
		stringValue(right["kind"]) != "b_roll_semantic_anchor" {
		return false
	}
	leftCopy := cloneMetadataWithoutPlacement(left)
	rightCopy := cloneMetadataWithoutPlacement(right)
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func cloneMetadataWithoutPlacement(metadata map[string]any) map[string]any {
	copy := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if key == "placement_timeline_start_frame" || key == "placement_timeline_end_frame" {
			continue
		}
		copy[key] = value
	}
	return copy
}
