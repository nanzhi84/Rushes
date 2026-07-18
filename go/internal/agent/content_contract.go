package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (service *Service) verifyContentContract(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (agentexec.ContractVerificationReport, bool, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return agentexec.ContractVerificationReport{}, false, err
	}
	contractMap, err := agentexec.ContentPlanContract(draft.ContentPlan)
	if err != nil {
		return agentexec.ContractVerificationReport{}, false, err
	}
	if contractMap == nil {
		return agentexec.ContractVerificationReport{}, false, nil
	}
	encoded, _ := json.Marshal(contractMap)
	contract := rushestools.ContentPlanContract{}
	if err := json.Unmarshal(encoded, &contract); err != nil {
		return agentexec.ContractVerificationReport{}, false, err
	}
	report := agentexec.ContractVerificationReport{Pass: true, Items: []agentexec.ContractVerificationItem{}}
	if contract.TargetDurationFrames > 0 {
		tolerance := 0
		if contract.DurationToleranceFrames == nil {
			tolerance = max(1, document.FPS/2)
		} else {
			tolerance = *contract.DurationToleranceFrames
		}
		delta := absInt(document.DurationFrames - contract.TargetDurationFrames)
		report.Items = append(report.Items, agentexec.ContractVerificationItem{
			Check: "target_duration", Pass: delta <= tolerance,
			Message: fmt.Sprintf("当前 %d 帧，目标 %d±%d 帧。", document.DurationFrames, contract.TargetDurationFrames, tolerance),
			Frames:  []int{document.DurationFrames, contract.TargetDurationFrames},
		})
	}
	if len(contract.MustKeepUtteranceIDs) > 0 {
		missing, anchors, verifyErr := service.missingRequiredUtterances(ctx, document, contract.MustKeepUtteranceIDs)
		if verifyErr != nil {
			return agentexec.ContractVerificationReport{}, false, verifyErr
		}
		report.Items = append(report.Items, agentexec.ContractVerificationItem{
			Check: "must_keep_utterances", Pass: len(missing) == 0,
			Message: map[bool]string{true: "必留台词均完整保留。", false: "必留台词已缺失或被截断：" + strings.Join(missing, "、")}[len(missing) == 0],
			Frames:  anchors, IDs: missing,
		})
	}
	if len(contract.BrollCoverageRanges) > 0 {
		uncovered := agentexec.UncoveredBrollRanges(document, contract.BrollCoverageRanges)
		frames := make([]int, 0, len(uncovered)*2)
		for _, frameRange := range uncovered {
			frames = append(frames, frameRange.StartFrame, frameRange.EndFrame)
		}
		report.Items = append(report.Items, agentexec.ContractVerificationItem{
			Check: "broll_coverage", Pass: len(uncovered) == 0,
			Message: map[bool]string{true: "B-roll 验收区间均已覆盖。", false: "存在未完整覆盖的 B-roll 验收区间。"}[len(uncovered) == 0],
			Frames:  frames,
		})
	}
	beatAlignment := agentexec.BeatAlignmentData(document)
	if contract.MinOnBeatRatio != nil {
		ratio, _ := agentexec.NumericValue(beatAlignment["alignment_ratio"])
		offBeat, _ := beatAlignment["off_beat_cut_frames"].([]int)
		item := agentexec.ContractVerificationItem{
			Check: "on_beat_ratio", Pass: ratio >= *contract.MinOnBeatRatio,
			Message: fmt.Sprintf("切点卡拍比例 %.3f，合同下限 %.3f。", ratio, *contract.MinOnBeatRatio),
			Frames:  offBeat,
		}
		if beatAlignment["beat_grid_present"] != true {
			item.Pass = false
			item.ErrorCode = "missing_beat_grid"
			item.Message = "无法核对卡拍比例：当前 BGM 无节拍网格，请先 audio.analyze_beats 或用 recut_to_beats 重建"
		}
		report.Items = append(report.Items, item)
	}
	if contract.MinCutDensityPerMinute != nil || contract.MaxCutDensityPerMinute != nil {
		cutCount, _ := agentexec.NumericValue(beatAlignment["cut_count"])
		density := 0.0
		if document.DurationFrames > 0 && document.FPS > 0 {
			density = cutCount * float64(document.FPS) * 60 / float64(document.DurationFrames)
		}
		pass := contract.MinCutDensityPerMinute == nil || density >= *contract.MinCutDensityPerMinute
		pass = pass && (contract.MaxCutDensityPerMinute == nil || density <= *contract.MaxCutDensityPerMinute)
		report.Items = append(report.Items, agentexec.ContractVerificationItem{
			Check: "cut_density", Pass: pass,
			Message: fmt.Sprintf("当前切点密度 %.2f 次/分钟。", density),
		})
	}
	for _, item := range report.Items {
		report.Pass = report.Pass && item.Pass
	}
	return report, true, nil
}

func (service *Service) missingRequiredUtterances(
	ctx context.Context,
	document timeline.Document,
	required []string,
) ([]string, []int, error) {
	wanted := make(map[string]struct{}, len(required))
	for _, id := range required {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	found := map[string]bool{}
	anchors := map[string][2]int{}
	contentClips := agentexec.ContentPreservingClips(document)
	assetIDs := map[string]struct{}{}
	for _, clip := range contentClips {
		if clip.AssetID != "" {
			assetIDs[clip.AssetID] = struct{}{}
		}
	}
	orderedAssetIDs := make([]string, 0, len(assetIDs))
	for assetID := range assetIDs {
		orderedAssetIDs = append(orderedAssetIDs, assetID)
	}
	sort.Strings(orderedAssetIDs)
	for _, assetID := range orderedAssetIDs {
		transcript, err := storage.LatestTranscript(ctx, service.database.Read(), assetID)
		if err == storage.ErrNotFound {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for _, utterance := range transcript.Utterances {
			id := agentexec.InterfaceString(utterance["utterance_id"])
			if _, required := wanted[id]; !required {
				continue
			}
			startValue, startOK := agentexec.NumericValue(utterance["source_start_frame"])
			endValue, endOK := agentexec.NumericValue(utterance["source_end_frame"])
			if !startOK || !endOK {
				continue
			}
			start, end := int(startValue), int(endValue)
			if _, exists := anchors[id]; !exists {
				anchors[id] = [2]int{start, end}
			}
			found[id] = found[id] || agentexec.UtteranceCoveredByClips(contentClips, assetID, start, end)
		}
	}
	missing := make([]string, 0)
	frameAnchors := make([]int, 0)
	for id := range wanted {
		if found[id] {
			continue
		}
		missing = append(missing, id)
		if anchor, exists := anchors[id]; exists {
			frameAnchors = append(frameAnchors, anchor[0], anchor[1])
		}
	}
	sort.Strings(missing)
	return missing, frameAnchors, nil
}
