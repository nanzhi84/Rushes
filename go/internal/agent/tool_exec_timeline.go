package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (service *Service) toolComposeInitial(
	ctx context.Context,
	draftID string,
	input rushestools.ComposeInitialInput,
) (rushestools.ToolResult, error) {
	version, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	selections := make([]timeline.Selection, 0, len(input.Clips))
	for index, clip := range input.Clips {
		asset, assetErr := storage.GetAsset(ctx, service.database.Read(), clip.AssetID)
		if assetErr != nil {
			return composeInitialFailure(index, clip, storage.Asset{}, assetErr.Error()), nil
		}
		durationSec, _ := numericValue(asset.Probe["duration_sec"])
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
			Status: "failed", Observation: "初版时间线参数校验失败，当前时间线未更新",
			Data: map[string]any{
				"error_code": "compose_initial_invalid", "reason": err.Error(),
				"current_timeline_unchanged": true,
				"recovery":                   "根据 failed_clip 与 asset_facts 修正源帧范围或素材类型后重试。",
			},
		}, nil
	}
	return service.persistTimeline(ctx, draftID, document, "compose_initial", []map[string]any{{
		"kind": "compose_initial", "clip_count": len(input.Clips),
	}})
}

func composeInitialFailure(
	index int,
	clip rushestools.ComposeClip,
	asset storage.Asset,
	reason string,
) rushestools.ToolResult {
	durationSec, _ := numericValue(asset.Probe["duration_sec"])
	assetID := asset.ID
	if assetID == "" {
		assetID = clip.AssetID
	}
	return rushestools.ToolResult{
		Status:      "failed",
		Observation: fmt.Sprintf("初版时间线第 %d 个片段参数无效，当前时间线未更新", index+1),
		Data: map[string]any{
			"error_code": "compose_initial_invalid", "failed_clip_index": index + 1,
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

func (service *Service) toolApplyPatches(
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
	current, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	operations := make([]map[string]any, len(input.Ops))
	for index := range input.Ops {
		operations[index] = map[string]any(input.Ops[index])
	}
	enrichedOperations, err := service.enrichTimelineOperations(ctx, draftID, operations)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	plannedOperations, preservedAudio := prepareTimelineBatch(current, enrichedOperations)
	document := current
	for index, operation := range plannedOperations {
		beforeOperation := document
		document, err = timeline.ApplyPatch(document, operation)
		if err != nil {
			if failure, ok := timelineOpFailureAt(err, operation, index+1, beforeOperation); ok {
				return failure, nil
			}
			message := fmt.Sprintf("第 %d 个时间线补丁失败: %v", index+1, err)
			return rushestools.ToolResult{
				Status: "failed", Observation: message,
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
	if restoreErr := restoreIndependentAudioTracks(&document, preservedAudio); restoreErr != nil {
		return rushestools.ToolResult{
			Status:      "failed",
			Observation: "批量主视频编辑会破坏未被本批直接编辑的 BGM/SFX，当前时间线未更新",
			Data: map[string]any{
				"reason":                     restoreErr.Error(),
				"current_timeline_unchanged": true,
				"recovery": "把完整的新主视频 insert_clip 与旧主视频 delete_clip 放在同一次 timeline.apply_patches 调用中，" +
					"保证最终时长能容纳现有音轨；卡点混剪改用 timeline.recut_to_beats。",
			},
		}, nil
	}
	attachedBeatGrids, beatWarnings := service.attachMissingBGMBeatGrids(ctx, draftID, &document)
	if report := timeline.Validate(document); !report.Valid {
		return rushestools.ToolResult{
			Status:      "failed",
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
	next, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	result, err := service.persistTimeline(ctx, draftID, document, "apply_patches", plannedOperations)
	appendBeatMetadataResult(&result, attachedBeatGrids, beatWarnings)
	return result, err
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

func (service *Service) persistTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperationBatches ...[]map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	validationType := "TimelineValidated"
	if !report.Valid {
		validationType = "TimelineValidationFailed"
	}
	reportMap := map[string]any{"valid": report.Valid, "checks": report.Checks, "issues": report.Issues}
	contractReport, hasContract, contractErr := service.verifyContentContract(ctx, draftID, document)
	if contractErr != nil {
		return rushestools.ToolResult{}, contractErr
	}
	if hasContract {
		reportMap["content_contract"] = contractReport
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
	patchID := operation + ":" + randomID("patch")
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{
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
	if !report.Valid {
		status = "validation_failed"
	}
	toolResult := rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{
			"validation_report": reportMap,
			"beat_alignment":    beatAlignmentData(document),
		},
	}
	if hasContract {
		failures := contractFailureItems(contractReport)
		if len(failures) > 0 {
			encoded, _ := json.Marshal(failures)
			toolResult.Observation += " 验收合同未通过项：" + string(encoded)
			toolResult.Data["contract_failures"] = failures
		}
	}
	return toolResult, nil
}

func (service *Service) toolValidateTimeline(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report := timeline.Validate(document)
	beatAlignment := beatAlignmentData(document)
	contractReport, hasContract, contractErr := service.verifyContentContract(ctx, draftID, document)
	if contractErr != nil {
		return rushestools.ToolResult{}, contractErr
	}
	validationReport := map[string]any{
		"valid": report.Valid, "checks": report.Checks, "issues": report.Issues,
	}
	if hasContract {
		validationReport["content_contract"] = contractReport
	}
	eventType := "TimelineValidated"
	if !report.Valid {
		eventType = "TimelineValidationFailed"
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: eventType, DraftID: draftID,
		Payload: map[string]any{
			"timeline_version":  document.Version,
			"validation_report": validationReport,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("validation reducer status: %s", result.Status))
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
	if hasContract {
		data["content_contract"] = contractReport
		failures := contractFailureItems(contractReport)
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

func (service *Service) toolInspectTimeline(
	ctx context.Context,
	draftID string,
	_ rushestools.TimelineInspectInput,
) (rushestools.ToolResult, error) {
	document, err := timeline.Latest(ctx, service.database, draftID)
	if errors.Is(err, storage.ErrNotFound) {
		return rushestools.ToolResult{
			Status:      "succeeded",
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
		Status: "succeeded", Observation: timeline.Inspect(document),
		Data: map[string]any{
			"timeline_exists": true,
			"fps":             document.FPS, "duration_frames": document.DurationFrames, "tracks": tracks,
			"audio_layout":   audioLayoutData(document),
			"beat_alignment": beatAlignmentData(document),
		},
	}, nil
}

func audioLayoutData(document timeline.Document) map[string]any {
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
