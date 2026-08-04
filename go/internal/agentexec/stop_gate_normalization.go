package agentexec

import (
	"context"
	"fmt"
	"math"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// StopGateTimelineNormalizationNeeded performs the same read-only planning as
// the commit path. Service uses it to avoid acquiring an edit lease when the
// timeline already has a canonical structure.
func (exec *Executor) StopGateTimelineNormalizationNeeded(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (bool, error) {
	_, merges, err := exec.planStopGateTimelineNormalization(ctx, draftID, document)
	return len(merges) > 0, err
}

// NormalizeTimelineForStopGate commits a new immutable timeline version only
// when the exact requested latest version contains a safely provable adjacent
// visual normalization: either render-equivalent split residue or a short
// same-shot source overlap. It is Harness-only and intentionally is not
// registered as a model tool.
func (exec *Executor) NormalizeTimelineForStopGate(
	ctx context.Context,
	draftID, requestedTimelineID string,
) (rushestools.ToolResult, error) {
	if rushestools.TimelineMutationOrigin(ctx) != "harness" {
		return rushestools.ToolResult{}, fmt.Errorf("stop gate 时间线归一化缺少 Harness 写入来源")
	}
	current, base, err := exec.timelineMutationSnapshot(ctx, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if requestedTimelineID == "" || current.TimelineID != requestedTimelineID {
		return rushestools.ToolResult{}, fmt.Errorf(
			"stop gate 时间线归一化版本不匹配: want=%s latest=%s",
			requestedTimelineID, current.TimelineID,
		)
	}
	document, merges, err := exec.planStopGateTimelineNormalization(ctx, draftID, current)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if len(merges) == 0 {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusSucceeded),
			Observation: "Stop Gate 未发现可安全合并的相邻主视频片段",
			Data: map[string]any{
				"timeline_id": current.TimelineID, "timeline_version": current.Version,
				"changed": false, "merged_clip_count": 0,
			},
		}, nil
	}

	trustedCurrentAudio, err := exec.preservedIndependentAudioFromStoredProof(ctx, draftID, current)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = current.Version + 1
	document.TimelineID = fmt.Sprintf("%s:v%d", document.DraftID, document.Version)
	if report := validateWithPreservedIndependentAudio(document, trustedCurrentAudio); !report.Valid {
		return rushestools.ToolResult{}, fmt.Errorf(
			"stop gate 相邻片段归一化破坏结构不变量: %v", report.Issues,
		)
	}
	mergeOperations := make([]map[string]any, 0, len(merges))
	for _, merge := range merges {
		item := map[string]any{
			"track_id":                 merge.TrackID,
			"kept_timeline_clip_id":    merge.KeptTimelineClipID,
			"removed_timeline_clip_id": merge.RemovedTimelineClipID,
			"boundary_frame":           merge.BoundaryFrame,
			"asset_id":                 merge.AssetID,
			"mode":                     merge.Mode,
		}
		if merge.ShotID != "" {
			item["shot_id"] = merge.ShotID
		}
		if merge.SourceOverlapFrames > 0 {
			item["source_overlap_frames"] = merge.SourceOverlapFrames
			item["previous_source_end_frame"] = merge.PreviousSourceEndFrame
			item["normalized_source_end_frame"] = merge.NormalizedSourceEndFrame
		}
		mergeOperations = append(mergeOperations, item)
	}
	operation := map[string]any{
		"kind":   "merge_redundant_adjacent_clips",
		"reason": "stop_gate_adjacent_continuity_cleanup",
		"merges": mergeOperations,
	}
	return exec.persistTimelineFromSnapshotWithPreservedAudioAndResultData(
		ctx,
		draftID,
		document,
		"stop_gate_coalesce",
		operation,
		base,
		trustedCurrentAudio,
		map[string]any{
			"changed": true, "merged_clip_count": len(merges),
			"normalization":     operation,
			"changed_targets":   atomicChangedTargets(current, document),
			"coordinate_effect": atomicTimelineCoordinateEffect(current, document),
		},
	)
}

func (exec *Executor) planStopGateTimelineNormalization(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (timeline.Document, []timeline.AdjacentClipMerge, error) {
	normalized, merges, err := timeline.CoalesceRedundantAdjacentClips(document)
	if err != nil {
		return timeline.Document{}, nil, err
	}
	repairs, err := exec.sameShotOverlapRepairs(ctx, draftID, normalized)
	if err != nil {
		return timeline.Document{}, nil, err
	}
	if len(repairs) == 0 {
		return normalized, merges, nil
	}
	normalized, overlapMerges, err := timeline.CoalesceApprovedOverlappingAdjacentClips(
		normalized, repairs,
	)
	if err != nil {
		return timeline.Document{}, nil, err
	}
	return normalized, append(merges, overlapMerges...), nil
}

func (exec *Executor) sameShotOverlapRepairs(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) ([]timeline.AdjacentClipOverlapRepair, error) {
	shots, err := storage.ListReadyIndexedShotsForDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	durationFrames := make(map[string]int, len(assets))
	for _, asset := range assets {
		durationSec, ok := NumericValue(asset.Probe["duration_sec"])
		if ok && durationSec > 0 {
			durationFrames[asset.ID] = int(math.Round(durationSec * float64(document.FPS)))
		}
	}
	repairs := []timeline.AdjacentClipOverlapRepair{}
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" || track.Locked {
			continue
		}
		for index := 0; index+1 < len(track.Clips); index++ {
			left, right := track.Clips[index], track.Clips[index+1]
			if !timeline.CanCoalesceOverlappingAdjacentClips(left, right) {
				continue
			}
			overlap := left.SourceEndFrame - right.SourceStartFrame
			timelineDuration := right.TimelineEndFrame - left.TimelineStartFrame
			if overlap <= 0 || overlap > document.FPS || overlap*4 > timelineDuration {
				continue
			}
			leftShot := bestStopGateIndexedShot(shots, left.AssetID, left.SourceStartFrame, left.SourceEndFrame)
			rightShot := bestStopGateIndexedShot(shots, right.AssetID, right.SourceStartFrame, right.SourceEndFrame)
			if leftShot == nil || rightShot == nil || leftShot.Shot.ShotID == "" ||
				leftShot.Shot.ShotID != rightShot.Shot.ShotID {
				continue
			}
			normalizedSourceEnd := left.SourceStartFrame + int(math.Round(
				float64(timelineDuration)*effectiveTimelinePlaybackRate(left),
			))
			if normalizedSourceEnd <= right.SourceEndFrame ||
				normalizedSourceEnd > durationFrames[left.AssetID] {
				continue
			}
			mergedShot := bestStopGateIndexedShot(
				shots, left.AssetID, left.SourceStartFrame, normalizedSourceEnd,
			)
			if mergedShot == nil || mergedShot.Shot.ShotID != leftShot.Shot.ShotID {
				continue
			}
			repairs = append(repairs, timeline.AdjacentClipOverlapRepair{
				TrackID:                  track.TrackID,
				KeptTimelineClipID:       left.TimelineClipID,
				RemovedTimelineClipID:    right.TimelineClipID,
				NormalizedSourceEndFrame: normalizedSourceEnd,
				ShotID:                   leftShot.Shot.ShotID,
			})
		}
	}
	return repairs, nil
}

func bestStopGateIndexedShot(
	shots []storage.DraftIndexedShot,
	assetID string,
	startFrame, endFrame int,
) *storage.DraftIndexedShot {
	bestIndex, bestOverlap := -1, 0
	for index := range shots {
		item := &shots[index]
		if item.AssetID != assetID {
			continue
		}
		overlap := min(endFrame, item.Shot.SourceEndFrame) -
			max(startFrame, item.Shot.SourceStartFrame)
		if overlap > bestOverlap {
			bestIndex, bestOverlap = index, overlap
		}
	}
	if bestIndex < 0 {
		return nil
	}
	return &shots[bestIndex]
}

func effectiveTimelinePlaybackRate(clip timeline.Clip) float64 {
	if clip.PlaybackRate > 0 {
		return clip.PlaybackRate
	}
	return 1
}
