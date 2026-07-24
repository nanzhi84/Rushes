package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (exec *Executor) toolComposeInitial(
	ctx context.Context,
	draftID string,
	input rushestools.ComposeInitialInput,
) (rushestools.ToolResult, error) {
	version, err := timeline.NextVersion(ctx, exec.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	selections := make([]timeline.Selection, 0, len(input.Clips))
	for index, clip := range input.Clips {
		asset, assetErr := storage.GetAsset(ctx, exec.database.Read(), clip.AssetID)
		if assetErr != nil {
			return composeInitialFailure(index, clip, storage.Asset{}, assetErr.Error()), nil
		}
		durationSec, _ := NumericValue(asset.Probe["duration_sec"])
		durationFrames := int(math.Round(durationSec * timeline.DefaultFPS))
		if asset.Kind != "video" && asset.Kind != "image" {
			return composeInitialFailure(index, clip, asset, "主视觉轨只支持 video/image 素材"), nil
		}
		if clip.SourceStartFrame < 0 || clip.SourceEndFrame <= clip.SourceStartFrame ||
			durationFrames > 0 && clip.SourceEndFrame > durationFrames {
			return composeInitialFailure(index, clip, asset, "源帧范围无效或超出素材时长"), nil
		}
		hasAudio, _ := asset.Probe["has_audio"].(bool)
		selections = append(selections, timeline.Selection{
			AssetID: clip.AssetID, AssetKind: asset.Kind, HasAudio: hasAudio,
			SourceStartFrame: clip.SourceStartFrame, SourceEndFrame: clip.SourceEndFrame,
			Role: clip.Role,
		})
	}
	document, err := timeline.ComposeInitial(draftID, version, selections)
	if err != nil {
		return rushestools.ToolResult{
			Status: string(rushestools.StatusFailed), Observation: "初版时间线参数校验失败，当前时间线未更新",
			Data: map[string]any{
				"error_code": string(rushestools.ErrCodeComposeInitialInvalid), "reason": err.Error(),
				"current_timeline_unchanged": true,
				"recovery":                   "根据 failed_clip 与 asset_facts 修正源帧范围或素材类型后重试。",
			},
		}, nil
	}
	return exec.PersistTimeline(ctx, draftID, document, "compose_initial", []map[string]any{{
		"kind": "compose_initial", "clip_count": len(input.Clips),
	}})
}

func composeInitialFailure(
	index int,
	clip rushestools.ComposeClip,
	asset storage.Asset,
	reason string,
) rushestools.ToolResult {
	durationSec, _ := NumericValue(asset.Probe["duration_sec"])
	assetID := asset.ID
	if assetID == "" {
		assetID = clip.AssetID
	}
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusFailed),
		Observation: fmt.Sprintf("初版时间线第 %d 个片段参数无效，当前时间线未更新", index+1),
		Data: map[string]any{
			"error_code": string(rushestools.ErrCodeComposeInitialInvalid), "failed_clip_index": index + 1,
			"failed_clip": map[string]any{
				"asset_id": clip.AssetID, "source_start_frame": clip.SourceStartFrame,
				"source_end_frame": clip.SourceEndFrame, "role": clip.Role,
			},
			"asset_facts": map[string]any{
				"asset_id": assetID, "kind": asset.Kind,
				"duration_frames": int(math.Round(durationSec * timeline.DefaultFPS)),
			},
			"reason": reason, "current_timeline_unchanged": true,
			"recovery": "改用 video/image 素材，并把 source_start_frame/source_end_frame 限制在 duration_frames 内。",
		},
	}
}

func (exec *Executor) toolApplyPatches(
	ctx context.Context,
	draftID string,
	input rushestools.TimelinePatchBatchInput,
) (rushestools.ToolResult, error) {
	if len(input.Ops) == 0 {
		return rushestools.ToolResult{}, errors.New("timeline.apply_patches 至少需要一个 op")
	}
	if len(input.Ops) > 100 {
		return rushestools.ToolResult{}, errors.New("timeline.apply_patches 单次最多 100 个 op")
	}
	current, err := timeline.Latest(ctx, exec.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	operations := make([]map[string]any, len(input.Ops))
	for index := range input.Ops {
		operations[index] = map[string]any(input.Ops[index])
	}
	enrichedOperations, err := exec.enrichTimelineOperations(ctx, draftID, operations)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	plannedOperations, preservedAudio := PrepareTimelineBatch(current, enrichedOperations)
	document := current
	for index, operation := range plannedOperations {
		beforeOperation := document
		document, err = timeline.ApplyPatch(document, operation)
		if err != nil {
			if failure, ok := TimelineOpFailureAt(err, operation, index+1, beforeOperation); ok {
				return failure, nil
			}
			message := fmt.Sprintf("第 %d 个时间线补丁失败: %v", index+1, err)
			return rushestools.ToolResult{
				Status: string(rushestools.StatusFailed), Observation: message,
				Data: map[string]any{
					"failed_op_index":            index + 1,
					"failed_op":                  operation,
					"reason":                     err.Error(),
					"current_timeline_unchanged": true,
					"recovery": "读取 failed_op 和 reason 后修正这一批；完整卡点重剪必须改用 timeline.recut_to_beats。" +
						"如需整批替换主视频，应把新 insert_clip 与旧 delete_clip 放在同一次调用中，工具会自动规划安全顺序并保护 BGM/SFX。",
				},
			}, nil
		}
	}
	if restoreErr := RestoreIndependentAudioTracks(&document, preservedAudio); restoreErr != nil {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "批量主视频编辑会破坏未被本批直接编辑的 BGM/SFX，当前时间线未更新",
			Data: map[string]any{
				"reason":                     restoreErr.Error(),
				"current_timeline_unchanged": true,
				"recovery": "把完整的新主视频 insert_clip 与旧主视频 delete_clip 放在同一次 timeline.apply_patches 调用中，" +
					"保证最终时长能容纳现有音轨；卡点混剪改用 timeline.recut_to_beats。",
			},
		}, nil
	}
	attachedBeatGrids, beatWarnings := exec.attachMissingBGMBeatGrids(ctx, draftID, &document)
	if report := timeline.Validate(document); !report.Valid {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "批量补丁结果未通过时间线校验，当前时间线未更新",
			Data: map[string]any{
				"failed_op_index":            len(plannedOperations),
				"reason":                     "validation_failed",
				"current_timeline_unchanged": true,
				"recovery":                   "根据 validation_report 修正整批参数后重试；卡点重剪不要降级为低层补丁。",
				"validation_report": map[string]any{
					"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
				},
			},
		}, nil
	}
	next, err := timeline.NextVersion(ctx, exec.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	result, err := exec.PersistTimeline(ctx, draftID, document, "apply_patches", plannedOperations)
	appendBeatMetadataResult(&result, attachedBeatGrids, beatWarnings)
	return result, err
}

func (exec *Executor) toolAtomicTimelineEdit(
	ctx context.Context,
	draftID string,
	toolName string,
	input any,
) (rushestools.ToolResult, error) {
	operation, err := rushestools.TimelineAtomicOperation(toolName, input)
	if err != nil {
		if failure, ok := TimelineOpFailureAt(err, map[string]any(operation), 0, timeline.Document{}); ok {
			return failure, nil
		}
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "原子时间线编辑输入不属于当前工具",
			Data: map[string]any{
				"reason":                     err.Error(),
				"current_timeline_unchanged": true,
				"recovery":                   "按当前工具 schema 只提交一个受支持的 kind；多个目标必须拆成多个工具调用。",
			},
		}, nil
	}

	current, err := timeline.Latest(ctx, exec.database, draftID)
	previousTimelineID := ""
	if errors.Is(err, storage.ErrNotFound) {
		if toolName != "timeline.insert" ||
			StringValue(operation["kind"]) != "insert_clip" ||
			ValueOr(StringValue(operation["track_id"]), "visual_base") != "visual_base" {
			return rushestools.ToolResult{
				Status:      string(rushestools.StatusFailed),
				Observation: "当前草稿尚无时间线，只有 visual_base clip 可以作为第一次原子插入",
				Data: map[string]any{
					"error_code":                 string(rushestools.ErrCodeTimelineAbsent),
					"current_timeline_unchanged": true,
					"recovery":                   "先用 timeline.insert 插入一个 visual_base clip，再继续字幕、叠加或其它编辑。",
				},
			}, nil
		}
		current = timeline.Empty(draftID, 0)
		current.DurationFrames = 0
	} else if err != nil {
		return rushestools.ToolResult{}, err
	} else {
		previousTimelineID = current.TimelineID
	}

	enriched, err := exec.enrichTimelineOperations(ctx, draftID, []map[string]any{operation})
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	appliedOperation := enriched[0]
	if failure := exec.validateAtomicTimelineAsset(ctx, draftID, current, appliedOperation); failure != nil {
		return *failure, nil
	}

	planned, preservedAudio := PrepareTimelineBatch(current, enriched)
	document, err := timeline.ApplyPatch(current, planned[0])
	if err != nil {
		if failure, ok := TimelineOpFailureAt(err, appliedOperation, 0, current); ok {
			if semanticKind, _ := failure.Data["semantic_error_kind"].(timeline.SemanticErrorKind); semanticKind == timeline.SemanticClipNotFound {
				failure.Data["error_code"] = string(rushestools.ErrCodeStaleTarget)
				failure.Data["recovery"] = "目标可能已被前一个原子编辑改写；先调用 timeline.inspect 读取最新稳定 ID，再继续剩余编辑。"
			}
			return failure, nil
		}
		return atomicTimelineApplyFailure(appliedOperation, err), nil
	}
	if restoreErr := RestoreIndependentAudioTracks(&document, preservedAudio); restoreErr != nil {
		return atomicTimelineApplyFailure(appliedOperation, restoreErr), nil
	}
	if atomicReplaceTouchesPrimary(current, appliedOperation) {
		audioAssetIDs, listErr := exec.draftAudioVideoAssetIDs(ctx, draftID)
		if listErr != nil {
			return rushestools.ToolResult{}, listErr
		}
		document, err = timeline.DeriveOriginalAudio(document, audioAssetIDs)
		if err != nil {
			return atomicTimelineApplyFailure(appliedOperation, err), nil
		}
	}
	attachedBeatGrids, beatWarnings := exec.attachMissingBGMBeatGrids(ctx, draftID, &document)
	if report := timeline.Validate(document); !report.Valid {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "原子编辑结果未通过结构校验，当前时间线未更新",
			Data: map[string]any{
				"failed_operation":           appliedOperation,
				"current_timeline_unchanged": true,
				"validation_summary": map[string]any{
					"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
				},
				"recovery": "读取 validation_summary；只修正这一个原子操作后重试。",
			},
		}, nil
	}

	changedTargets := atomicChangedTargets(current, document)
	result, err := exec.PersistTimeline(
		ctx,
		draftID,
		document,
		strings.TrimPrefix(toolName, "timeline."),
		[]map[string]any{appliedOperation},
	)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	result.Data["previous_timeline_id"] = previousTimelineID
	result.Data["timeline_id"] = document.TimelineID
	result.Data["applied_operation"] = appliedOperation
	result.Data["changed_targets"] = changedTargets
	result.Data["validation_summary"] = result.Data["validation_report"]
	appendBeatMetadataResult(&result, attachedBeatGrids, beatWarnings)
	return result, nil
}

func atomicTimelineApplyFailure(operation map[string]any, err error) rushestools.ToolResult {
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusFailed),
		Observation: "原子时间线编辑失败，当前时间线未更新",
		Data: map[string]any{
			"failed_operation":           operation,
			"reason":                     err.Error(),
			"current_timeline_unchanged": true,
			"recovery":                   "读取当前时间线事实并只修正这个操作；不要把多个目标合并到一次调用。",
		},
	}
}

func (exec *Executor) validateAtomicTimelineAsset(
	ctx context.Context,
	draftID string,
	current timeline.Document,
	operation map[string]any,
) *rushestools.ToolResult {
	kind := StringValue(operation["kind"])
	if kind != "insert_clip" && kind != "replace_clip" {
		return nil
	}
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		result := atomicTimelineApplyFailure(operation, err)
		return &result
	}
	var asset storage.Asset
	found := false
	for _, candidate := range assets {
		if candidate.ID == StringValue(operation["asset_id"]) {
			asset = candidate
			found = true
			break
		}
	}
	if !found {
		result := atomicTimelineApplyFailure(
			operation,
			fmt.Errorf("素材 %s 不属于当前草稿", StringValue(operation["asset_id"])),
		)
		result.Data["error_code"] = string(rushestools.ErrCodeStaleTarget)
		result.Data["recovery"] = "先调用 asset.list_assets 读取当前草稿的稳定 asset_id，再重试这一个操作。"
		return &result
	}

	trackID := ValueOr(StringValue(operation["track_id"]), "visual_base")
	if kind == "replace_clip" {
		clipID := ValueOr(
			StringValue(operation["timeline_clip_id"]),
			StringValue(operation["clip_id"]),
		)
		trackID = atomicClipTrackID(current, clipID)
		if target, exists := atomicTimelineClip(current, clipID); exists {
			durationSec, _ := NumericValue(asset.Probe["duration_sec"])
			durationFrames := int(math.Round(durationSec * timeline.DefaultFPS))
			if durationFrames > 0 && target.SourceEndFrame > durationFrames {
				result := atomicTimelineApplyFailure(
					operation,
					fmt.Errorf(
						"替换素材 %s 只有 %d 帧，无法覆盖目标源区间 %d-%d",
						asset.ID, durationFrames, target.SourceStartFrame, target.SourceEndFrame,
					),
				)
				result.Data["asset_facts"] = map[string]any{
					"asset_id": asset.ID, "kind": asset.Kind, "duration_frames": durationFrames,
				}
				return &result
			}
		}
	}
	if (trackID == "visual_base" || trackID == "visual_overlay") &&
		asset.Kind != "video" && asset.Kind != "image" {
		result := atomicTimelineApplyFailure(operation, fmt.Errorf("%s 轨只支持 video/image 素材", trackID))
		return &result
	}
	if trackID == "voiceover" || trackID == "bgm" || trackID == "sfx" {
		if asset.Kind != "audio" && asset.Kind != "video" {
			result := atomicTimelineApplyFailure(operation, fmt.Errorf("%s 轨只支持 audio/video 素材", trackID))
			return &result
		}
	}
	if kind == "insert_clip" {
		start, startOK := NumericValue(operation["source_start_frame"])
		end, endOK := NumericValue(operation["source_end_frame"])
		durationSec, _ := NumericValue(asset.Probe["duration_sec"])
		durationFrames := int(math.Round(durationSec * timeline.DefaultFPS))
		if !startOK || !endOK || start < 0 || end <= start ||
			durationFrames > 0 && int(end) > durationFrames {
			result := atomicTimelineApplyFailure(
				operation,
				fmt.Errorf("素材源帧范围无效；asset=%s duration_frames=%d", asset.ID, durationFrames),
			)
			result.Data["asset_facts"] = map[string]any{
				"asset_id": asset.ID, "kind": asset.Kind, "duration_frames": durationFrames,
			}
			return &result
		}
	}
	return nil
}

func atomicReplaceTouchesPrimary(current timeline.Document, operation map[string]any) bool {
	if StringValue(operation["kind"]) != "replace_clip" {
		return false
	}
	clipID := ValueOr(StringValue(operation["timeline_clip_id"]), StringValue(operation["clip_id"]))
	return atomicClipTrackID(current, clipID) == "visual_base"
}

func atomicClipTrackID(document timeline.Document, clipID string) string {
	clip, exists := atomicTimelineClip(document, clipID)
	if !exists {
		return ""
	}
	return clip.TrackID
}

func atomicTimelineClip(document timeline.Document, clipID string) (timeline.Clip, bool) {
	for _, track := range document.Tracks {
		for _, clip := range track.Clips {
			if clip.TimelineClipID == clipID {
				return clip, true
			}
		}
	}
	return timeline.Clip{}, false
}

func (exec *Executor) draftAudioVideoAssetIDs(ctx context.Context, draftID string) ([]string, error) {
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(assets))
	for _, asset := range assets {
		hasAudio, _ := asset.Probe["has_audio"].(bool)
		if asset.Kind == "video" && hasAudio {
			result = append(result, asset.ID)
		}
	}
	sort.Strings(result)
	return result, nil
}

func atomicChangedTargets(before, after timeline.Document) []map[string]any {
	type clipTarget struct {
		trackID string
		clip    timeline.Clip
	}
	beforeClips := map[string]clipTarget{}
	afterClips := map[string]clipTarget{}
	beforeTracks := map[string]timeline.Track{}
	afterTracks := map[string]timeline.Track{}
	for _, track := range before.Tracks {
		trackCopy := track
		trackCopy.Clips = nil
		beforeTracks[track.TrackID] = trackCopy
		for _, clip := range track.Clips {
			beforeClips[clip.TimelineClipID] = clipTarget{trackID: track.TrackID, clip: clip}
		}
	}
	for _, track := range after.Tracks {
		trackCopy := track
		trackCopy.Clips = nil
		afterTracks[track.TrackID] = trackCopy
		for _, clip := range track.Clips {
			afterClips[clip.TimelineClipID] = clipTarget{trackID: track.TrackID, clip: clip}
		}
	}
	targets := []map[string]any{}
	clipIDs := map[string]struct{}{}
	for clipID := range beforeClips {
		clipIDs[clipID] = struct{}{}
	}
	for clipID := range afterClips {
		clipIDs[clipID] = struct{}{}
	}
	sortedClipIDs := make([]string, 0, len(clipIDs))
	for clipID := range clipIDs {
		sortedClipIDs = append(sortedClipIDs, clipID)
	}
	sort.Strings(sortedClipIDs)
	for _, clipID := range sortedClipIDs {
		previous, existedBefore := beforeClips[clipID]
		current, existsAfter := afterClips[clipID]
		if existedBefore && existsAfter && reflect.DeepEqual(previous, current) {
			continue
		}
		change := "updated"
		trackID := current.trackID
		if !existedBefore {
			change = "inserted"
		} else if !existsAfter {
			change = "deleted"
			trackID = previous.trackID
		}
		targets = append(targets, map[string]any{
			"target_type": "clip", "timeline_clip_id": clipID,
			"track_id": trackID, "change": change,
		})
	}
	trackIDs := make([]string, 0, len(afterTracks))
	for trackID := range afterTracks {
		trackIDs = append(trackIDs, trackID)
	}
	sort.Strings(trackIDs)
	for _, trackID := range trackIDs {
		if reflect.DeepEqual(beforeTracks[trackID], afterTracks[trackID]) {
			continue
		}
		targets = append(targets, map[string]any{
			"target_type": "track", "track_id": trackID, "change": "updated",
		})
	}
	return targets
}

func appendBeatMetadataResult(
	result *rushestools.ToolResult,
	attached int,
	warnings []string,
) {
	if result == nil || attached == 0 && len(warnings) == 0 {
		return
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	result.Data["beat_grid_attached_count"] = attached
	if len(warnings) > 0 {
		result.Data["beat_grid_warnings"] = warnings
	}
}

func (exec *Executor) PersistTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperationBatches ...[]map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	reportMap, valid, err := exec.timelineValidationReport(ctx, draftID, document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	validationType := "TimelineValidated"
	if !valid {
		validationType = "TimelineValidationFailed"
	}
	actor := contracts.ActorAgent
	origin := rushestools.TimelineMutationOrigin(ctx)
	if origin == "manual" {
		actor = contracts.ActorUser
	}
	if origin == "" {
		origin = "agent"
	}
	editOperations := []map[string]any{}
	if len(editOperationBatches) > 0 {
		editOperations = editOperationBatches[0]
	}
	patchID := operation + ":" + RandomID("patch")
	result, err := reducer.Apply(ctx, exec.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: map[string]any{
				"timeline_id": document.TimelineID, "timeline_version": document.Version,
				"patch_id": patchID, "document_json": documentMap,
				"edit_origin": origin, "edit_operations": editOperations,
			},
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{"timeline_version": document.Version, "validation_report": reportMap},
		},
	}, reducer.Options{Actor: actor, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("timeline reducer status: %s", result.Status))
	}
	status := "succeeded"
	if !valid {
		status = "validation_failed"
	}
	toolResult := rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{
			"validation_report": reportMap,
			"beat_alignment":    BeatAlignmentData(document),
		},
	}
	if contractReport, hasContract := reportMap["content_contract"].(ContractVerificationReport); hasContract {
		failures := ContractFailureItems(contractReport)
		if len(failures) > 0 {
			encoded, _ := json.Marshal(failures)
			toolResult.Observation += " 验收合同未通过项：" + string(encoded)
			toolResult.Data["contract_failures"] = failures
		}
	}
	return toolResult, nil
}

func (exec *Executor) timelineValidationReport(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (map[string]any, bool, error) {
	report := timeline.Validate(document)
	reportMap := map[string]any{
		"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
	}
	contractReport, hasContract, err := exec.VerifyContentContract(ctx, draftID, document)
	if err != nil {
		return nil, false, err
	}
	if hasContract {
		reportMap["content_contract"] = contractReport
	}
	return reportMap, report.Valid, nil
}

func (exec *Executor) toolCheckTimeline(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, exec.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	beatAlignment := BeatAlignmentData(document)
	contractReport, hasContract, contractErr := exec.VerifyContentContract(ctx, draftID, document)
	if contractErr != nil {
		return rushestools.ToolResult{}, contractErr
	}
	validationReport := map[string]any{
		"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
	}
	if hasContract {
		validationReport["content_contract"] = contractReport
	}
	observation := timeline.Inspect(document)
	if report.Valid {
		if present, _ := beatAlignment["beat_grid_present"].(bool); !present {
			observation += " 结构校验通过，但 BGM 缺少节拍元数据，当前结果不能证明画面切点已卡点。"
		} else {
			observation += fmt.Sprintf(
				" 节拍诊断：%v/%v 个画面切点落在真实拍点。",
				beatAlignment["on_beat_cut_count"], beatAlignment["cut_count"],
			)
		}
	}
	data := map[string]any{
		"validation_report": validationReport,
		"beat_alignment":    beatAlignment,
	}
	// timeline.check 是只读诊断：口播质检读取失败（如 transcript 缺失/损坏）时跳过附加，
	// 不让合法时间线因增强信息读取失败而报错（与 toolEditTalkingHead 的软跳过一致）。
	if quality, qualityErr := exec.SpeechQualityReport(ctx, document); qualityErr == nil {
		if present, _ := quality["a_roll_present"].(bool); present {
			data["speech_quality"] = quality
			observation += TalkingHeadQualitySummary(quality)
		}
	}
	if hasContract {
		data["content_contract"] = contractReport
		failures := ContractFailureItems(contractReport)
		data["contract_failures"] = failures
		if len(failures) == 0 {
			observation += " 验收合同全部通过。"
		} else {
			encoded, _ := json.Marshal(failures)
			observation += " 验收合同未通过项：" + string(encoded)
		}
	}
	return rushestools.ToolResult{
		Status:      map[bool]string{true: "succeeded", false: "validation_failed"}[report.Valid],
		Observation: observation,
		Data:        data,
	}, nil
}

func (exec *Executor) toolInspectTimeline(
	ctx context.Context,
	draftID string,
	_ rushestools.TimelineInspectInput,
) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, exec.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusSucceeded),
			Observation: "当前草稿尚无时间线；请先选择素材并创建初版时间线。",
			Data: map[string]any{
				"timeline_exists": false,
				"fps":             timeline.DefaultFPS,
				"duration_frames": 0,
				"tracks":          []map[string]any{},
			},
		}, nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	tracks := make([]map[string]any, 0, len(document.Tracks))
	for _, track := range document.Tracks {
		clips := make([]map[string]any, 0, len(track.Clips))
		for _, clip := range track.Clips {
			clipData := map[string]any{
				"timeline_clip_id":     clip.TimelineClipID,
				"asset_id":             clip.AssetID,
				"asset_kind":           clip.AssetKind,
				"role":                 clip.Role,
				"timeline_start_frame": clip.TimelineStartFrame,
				"timeline_end_frame":   clip.TimelineEndFrame,
				"source_start_frame":   clip.SourceStartFrame,
				"source_end_frame":     clip.SourceEndFrame,
				"text":                 clip.Text,
				"fade_in_frames":       clip.FadeInFrames,
				"fade_out_frames":      clip.FadeOutFrames,
				"subtitle_style":       clip.SubtitleStyle,
			}
			if len(clip.Effects) > 0 {
				clipData["effects"] = clip.Effects
			}
			if len(clip.Metadata) > 0 {
				clipData["metadata"] = clip.Metadata
			}
			clips = append(clips, clipData)
		}
		trackData := map[string]any{
			"track_id": track.TrackID, "track_type": track.TrackType,
			"muted": track.Muted, "locked": track.Locked, "clips": clips,
		}
		if track.Ducking != nil {
			trackData["ducking"] = track.Ducking
		}
		tracks = append(tracks, trackData)
	}
	return rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded), Observation: timeline.Inspect(document),
		Data: map[string]any{
			"timeline_exists": true,
			"fps":             document.FPS, "duration_frames": document.DurationFrames, "tracks": tracks,
			"audio_layout":   AudioLayoutData(document),
			"beat_alignment": BeatAlignmentData(document),
		},
	}, nil
}

func AudioLayoutData(document timeline.Document) map[string]any {
	bgmClips := []timeline.Clip{}
	sfxClips := []timeline.Clip{}
	for _, track := range document.Tracks {
		switch track.TrackID {
		case "bgm":
			bgmClips = append(bgmClips, track.Clips...)
		case "sfx":
			sfxClips = append(sfxClips, track.Clips...)
		}
	}
	bgmEnd := 0
	bgmRanges := make([]map[string]int, 0, len(bgmClips))
	for _, clip := range bgmClips {
		bgmEnd = max(bgmEnd, clip.TimelineEndFrame)
		bgmRanges = append(bgmRanges, map[string]int{
			"start_frame": clip.TimelineStartFrame, "end_frame": clip.TimelineEndFrame,
		})
	}
	sfxRanges := make([]map[string]any, 0, len(sfxClips))
	sfxWithoutBGM := []string{}
	for _, sfx := range sfxClips {
		overlapsBGM := false
		for _, bgm := range bgmClips {
			if sfx.TimelineStartFrame < bgm.TimelineEndFrame && bgm.TimelineStartFrame < sfx.TimelineEndFrame {
				overlapsBGM = true
				break
			}
		}
		sfxRanges = append(sfxRanges, map[string]any{
			"timeline_clip_id": sfx.TimelineClipID,
			"start_frame":      sfx.TimelineStartFrame, "end_frame": sfx.TimelineEndFrame,
			"overlaps_bgm": overlapsBGM,
		})
		if len(bgmClips) > 0 && !overlapsBGM {
			sfxWithoutBGM = append(sfxWithoutBGM, sfx.TimelineClipID)
		}
	}
	warnings := []string{}
	if len(bgmClips) > 0 && bgmEnd < document.DurationFrames {
		warnings = append(warnings, fmt.Sprintf(
			"BGM 在 %d 帧结束，时间线到 %d 帧，尾部没有音乐覆盖",
			bgmEnd, document.DurationFrames,
		))
	}
	if len(sfxWithoutBGM) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"SFX %s 没有与 BGM 重叠，无法作为音乐点缀",
			strings.Join(sfxWithoutBGM, ", "),
		))
	}
	return map[string]any{
		"bgm_ranges": bgmRanges, "sfx_ranges": sfxRanges,
		"bgm_coverage_end_frame": bgmEnd, "sfx_without_bgm": sfxWithoutBGM,
		"warnings": warnings,
	}
}
