package agentexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type ContractVerificationItem struct {
	Check     string   `json:"check"`
	Pass      bool     `json:"pass"`
	ErrorCode string   `json:"error_code,omitempty"`
	Message   string   `json:"message"`
	Frames    []int    `json:"frames,omitempty"`
	IDs       []string `json:"ids,omitempty"`
}

type ContractVerificationReport struct {
	Pass  bool                       `json:"pass"`
	Items []ContractVerificationItem `json:"items"`
}

func ContentPlanContract(plan map[string]any) (map[string]any, error) {
	raw, exists := plan["contract"]
	if !exists || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("验收合同不是有效 JSON 对象")
	}
	contract := rushestools.ContentPlanContract{}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		return nil, fmt.Errorf("验收合同字段类型无效")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("验收合同不是单个有效 JSON 对象")
	}
	contractFields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &contractFields); err != nil {
		return nil, fmt.Errorf("验收合同不是有效 JSON 对象")
	}
	if _, provided := contractFields["must_keep_utterance_ids"]; provided {
		if len(contract.MustKeepUtteranceIDs) == 0 {
			return nil, fmt.Errorf("验收合同的 must_keep_utterance_ids 不能为空")
		}
		normalized := make([]string, 0, len(contract.MustKeepUtteranceIDs))
		seen := make(map[string]struct{}, len(contract.MustKeepUtteranceIDs))
		for _, rawID := range contract.MustKeepUtteranceIDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				return nil, fmt.Errorf("验收合同的 must_keep_utterance_ids 不能包含空 ID")
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			normalized = append(normalized, id)
		}
		contract.MustKeepUtteranceIDs = normalized
	}
	if contract.TargetDurationFrames < 0 || contract.DurationToleranceFrames != nil && *contract.DurationToleranceFrames < 0 {
		return nil, fmt.Errorf("验收合同的目标时长与误差不能为负数")
	}
	if contract.MinOnBeatRatio != nil && (*contract.MinOnBeatRatio < 0 || *contract.MinOnBeatRatio > 1) {
		return nil, fmt.Errorf("验收合同的 min_on_beat_ratio 必须在 0 到 1 之间")
	}
	if contract.MinCutDensityPerMinute != nil && *contract.MinCutDensityPerMinute < 0 ||
		contract.MaxCutDensityPerMinute != nil && *contract.MaxCutDensityPerMinute < 0 {
		return nil, fmt.Errorf("验收合同的切点密度不能为负数")
	}
	if contract.MinCutDensityPerMinute != nil && contract.MaxCutDensityPerMinute != nil &&
		*contract.MinCutDensityPerMinute > *contract.MaxCutDensityPerMinute {
		return nil, fmt.Errorf("验收合同的切点密度下限不能高于上限")
	}
	for _, frameRange := range contract.BrollCoverageRanges {
		if frameRange.StartFrame < 0 || frameRange.EndFrame <= frameRange.StartFrame {
			return nil, fmt.Errorf("验收合同含无效 B-roll 覆盖区间")
		}
	}
	return CanonicalContentPlanValue(contract)
}

func ContentPreservingClips(document timeline.Document) []timeline.Clip {
	result := []timeline.Clip{}
	hasAudioSolo := false
	for _, track := range document.Tracks {
		if isAudioTrack(track.TrackID) && track.Solo && !track.Muted {
			hasAudioSolo = true
			break
		}
	}
	originalAudioExplicit := false
	originalAudioEnabled := false
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "original_audio":
			originalAudioEnabled = !track.Muted && (!hasAudioSolo || track.Solo)
			if originalAudioEnabled && len(track.Clips) > 0 {
				originalAudioExplicit = true
				result = append(result, track.Clips...)
			}
		case "voiceover":
			if !track.Muted && (!hasAudioSolo || track.Solo) {
				result = append(result, track.Clips...)
			}
		}
	}
	if originalAudioEnabled && !originalAudioExplicit {
		for _, track := range document.Tracks {
			if track.TrackID == "visual_base" && !track.Muted {
				result = append(result, track.Clips...)
			}
		}
	}
	return result
}

func isAudioTrack(trackID string) bool {
	switch trackID {
	case "original_audio", "voiceover", "bgm", "sfx":
		return true
	default:
		return false
	}
}

func UtteranceCoveredByClips(clips []timeline.Clip, assetID string, start, end int) bool {
	if assetID == "" || end <= start {
		return false
	}
	candidates := make([]timeline.Clip, 0)
	for _, clip := range clips {
		if clip.AssetID != assetID || clip.SourceEndFrame <= start || clip.SourceStartFrame >= end ||
			!contentClipPreservesSourceTiming(clip) {
			continue
		}
		candidates = append(candidates, clip)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].TimelineStartFrame != candidates[j].TimelineStartFrame {
			return candidates[i].TimelineStartFrame < candidates[j].TimelineStartFrame
		}
		return candidates[i].SourceStartFrame < candidates[j].SourceStartFrame
	})
	for index, clip := range candidates {
		if clip.SourceStartFrame > start || clip.SourceEndFrame <= start {
			continue
		}
		previous := clip
		sourceCursor := previous.SourceEndFrame
		playbackRate := contentClipPlaybackRate(clip)
		if sourceCursor >= end {
			return true
		}
		for next := index + 1; next < len(candidates); next++ {
			candidate := candidates[next]
			if !clipsHaveContinuousSourceBoundary(previous, candidate) ||
				candidate.TimelineStartFrame != previous.TimelineEndFrame ||
				math.Abs(contentClipPlaybackRate(candidate)-playbackRate) > 0.000001 {
				continue
			}
			previous = candidate
			sourceCursor = candidate.SourceEndFrame
			if sourceCursor >= end {
				return true
			}
		}
	}
	return false
}

func contentClipPreservesSourceTiming(clip timeline.Clip) bool {
	if clip.SourceEndFrame <= clip.SourceStartFrame || clip.TimelineEndFrame <= clip.TimelineStartFrame || clip.PlaybackRate < 0 {
		return false
	}
	rate := contentClipPlaybackRate(clip)
	sourceDuration := float64(clip.SourceEndFrame - clip.SourceStartFrame)
	timelineDuration := float64(clip.TimelineEndFrame - clip.TimelineStartFrame)
	return math.Abs(sourceDuration-timelineDuration*rate) <= 1
}

func contentClipPlaybackRate(clip timeline.Clip) float64 {
	if clip.PlaybackRate > 0 {
		return clip.PlaybackRate
	}
	return 1
}

func UncoveredBrollRanges(document timeline.Document, required []rushestools.ContentPlanFrameRange) []rushestools.ContentPlanFrameRange {
	overlays := []timeline.Clip{}
	for _, track := range document.Tracks {
		if track.TrackID == "visual_overlay" && !track.Muted {
			overlays = append(overlays, track.Clips...)
		}
	}
	uncovered := make([]rushestools.ContentPlanFrameRange, 0)
	for _, frameRange := range required {
		cursor := frameRange.StartFrame
		for cursor < frameRange.EndFrame {
			furthest := cursor
			for _, clip := range overlays {
				if clip.TimelineStartFrame <= cursor && clip.TimelineEndFrame > furthest {
					furthest = clip.TimelineEndFrame
				}
			}
			if furthest == cursor {
				uncovered = append(uncovered, frameRange)
				break
			}
			cursor = furthest
		}
	}
	return uncovered
}

func ContractFailureItems(report ContractVerificationReport) []ContractVerificationItem {
	failures := make([]ContractVerificationItem, 0)
	for _, item := range report.Items {
		if !item.Pass {
			failures = append(failures, item)
		}
	}
	return failures
}
