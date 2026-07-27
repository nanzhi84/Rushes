package agentexec

import (
	"context"
	"database/sql"
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

func (exec *Executor) toolAtomicTimelineEdit(
	ctx context.Context,
	draftID string,
	toolName string,
	input any,
) (rushestools.ToolResult, error) {
	operation, err := rushestools.TimelineAtomicOperation(toolName, input)
	if err != nil {
		if failure, ok := TimelineOpFailure(toolName, err, map[string]any(operation), timeline.Document{}); ok {
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

	current, mutationBase, err := exec.timelineMutationSnapshot(ctx, draftID)
	previousTimelineID := ""
	if err == nil && mutationBase.timelineVersion == 0 {
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
	} else if err != nil {
		return rushestools.ToolResult{}, err
	} else {
		previousTimelineID = current.TimelineID
	}
	targetClipID := StringValue(operation["timeline_clip_id"])
	admission := atomicEditAdmissionFromContext(ctx)
	if admission != nil {
		if decision := admission.admit(draftID, current, operation); decision != atomicEditAdmitted {
			return atomicEditAdmissionFailure(decision, targetClipID), nil
		}
	}

	appliedOperation, err := exec.enrichTimelineOperation(ctx, draftID, operation)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if failure := exec.validateAtomicTimelineAsset(ctx, draftID, current, appliedOperation); failure != nil {
		return *failure, nil
	}

	trustedCurrentAudio, proofErr := exec.preservedIndependentAudioFromStoredProof(ctx, draftID, current)
	if proofErr != nil {
		return rushestools.ToolResult{}, proofErr
	}
	currentValid := validateWithPreservedIndependentAudio(current, trustedCurrentAudio).Valid
	preservedAudio := preserveIndependentAudioForOperation(current, appliedOperation)
	patchInput := unlockPreservedIndependentAudio(current, preservedAudio)
	document, err := timeline.ApplyPatch(patchInput, appliedOperation)
	if err != nil {
		if failure, ok := TimelineOpFailure(toolName, err, appliedOperation, current); ok {
			if semanticKind, _ := failure.Data["semantic_error_kind"].(timeline.SemanticErrorKind); semanticKind == timeline.SemanticClipNotFound {
				failure.Data["error_code"] = string(rushestools.ErrCodeStaleTarget)
				failure.Data["recovery"] = "目标可能已被前一个原子编辑改写；先调用 timeline.inspect 读取最新稳定 ID，再继续剩余编辑。"
			}
			return failure, nil
		}
		return atomicTimelineApplyFailure(appliedOperation, err), nil
	}
	restoreIndependentAudioTracks(&document, preservedAudio)
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
	audioValidationProof := deriveIndependentAudioValidationProof(
		current, document, trustedCurrentAudio, currentValid,
	)
	if report := validateWithPreservedIndependentAudio(document, audioValidationProof); !report.Valid {
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
	coordinateEffect := atomicTimelineCoordinateEffect(current, document)
	result, err := exec.persistTimelineFromSnapshotWithPreservedAudio(
		ctx,
		draftID,
		document,
		strings.TrimPrefix(toolName, "timeline."),
		appliedOperation,
		mutationBase,
		audioValidationProof,
	)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	if result.Status == string(rushestools.StatusSucceeded) {
		admission.recordSuccess(draftID, appliedOperation)
	}
	result.Data["previous_timeline_id"] = previousTimelineID
	if _, present := result.Data["timeline_id"]; !present {
		result.Data["timeline_id"] = document.TimelineID
	}
	if result.Status == string(rushestools.StatusFailed) {
		result.Data["failed_operation"] = appliedOperation
	} else {
		result.Data["applied_operation"] = appliedOperation
		result.Data["changed_targets"] = changedTargets
		result.Data["coordinate_effect"] = coordinateEffect
		result.Data["validation_summary"] = result.Data["validation_report"]
	}
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
	if kind != "insert_clip" && kind != "replace_clip" && kind != "trim_clip" {
		return nil
	}
	assetID := StringValue(operation["asset_id"])
	var target timeline.Clip
	targetExists := false
	if kind == "trim_clip" {
		target, targetExists = atomicTimelineClip(current, StringValue(operation["timeline_clip_id"]))
		if !targetExists {
			return nil
		}
		assetID = target.AssetID
	}
	assets, err := storage.ListDraftAssets(ctx, exec.database.Read(), draftID)
	if err != nil {
		result := atomicTimelineApplyFailure(operation, err)
		return &result
	}
	var asset storage.Asset
	found := false
	for _, candidate := range assets {
		if candidate.ID == assetID {
			asset = candidate
			found = true
			break
		}
	}
	if !found {
		result := atomicTimelineApplyFailure(
			operation,
			fmt.Errorf("素材 %s 不属于当前草稿", assetID),
		)
		result.Data["error_code"] = string(rushestools.ErrCodeStaleTarget)
		result.Data["recovery"] = "先调用 asset.list_assets 读取当前草稿的稳定 asset_id，再重试这一个操作。"
		return &result
	}

	trackID := ValueOr(StringValue(operation["track_id"]), "visual_base")
	if kind == "replace_clip" {
		clipID := StringValue(operation["timeline_clip_id"])
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
	if kind == "trim_clip" {
		trackID = target.TrackID
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
	if kind == "insert_clip" && trackID == "original_audio" {
		result := atomicTimelineApplyFailure(
			operation,
			errors.New("original_audio 是服务端派生轨，不能直接插入片段"),
		)
		return &result
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
	if kind == "insert_clip" && trackID == "bgm" {
		if missing := incompleteAtomicBeatGridFields(operation); len(missing) > 0 {
			result := atomicTimelineApplyFailure(
				operation,
				fmt.Errorf("metadata.beat_grid 缺少或包含无效检测证据字段: %s", strings.Join(missing, ",")),
			)
			result.Data["missing_beat_grid_fields"] = missing
			result.Data["recovery"] = "保留已成功原语，只重试本次 BGM 插入；把 audio.analyze_beats 本轮返回的 bpm、beat_frames、strong_beat_frames、downbeat_frames、bar_phase 与 analysis_method 原样写入 metadata.beat_grid。"
			return &result
		}
	}
	return nil
}

func incompleteAtomicBeatGridFields(operation map[string]any) []string {
	metadata, hasMetadata := operation["metadata"]
	if !hasMetadata {
		return nil
	}
	metadataObject, ok := metadata.(map[string]any)
	if !ok {
		return []string{"metadata"}
	}
	rawGrid, hasGrid := metadataObject["beat_grid"]
	if !hasGrid {
		return nil
	}
	grid, ok := rawGrid.(map[string]any)
	if !ok {
		return []string{"beat_grid"}
	}
	missing := []string{}
	if bpm, valid := NumericValue(grid["bpm"]); !valid || bpm <= 0 {
		missing = append(missing, "bpm")
	}
	for _, name := range []string{
		"beat_frames", "strong_beat_frames", "downbeat_frames",
	} {
		requireNonEmpty := name == "beat_frames"
		if !validAtomicFrameArray(grid[name], requireNonEmpty) {
			missing = append(missing, name)
		}
	}
	if phase, valid := NumericValue(grid["bar_phase"]); !valid ||
		phase < 0 || phase != math.Trunc(phase) {
		missing = append(missing, "bar_phase")
	}
	if strings.TrimSpace(StringValue(grid["analysis_method"])) == "" {
		missing = append(missing, "analysis_method")
	}
	return missing
}

func validAtomicFrameArray(value any, requireNonEmpty bool) bool {
	frames := reflect.ValueOf(value)
	if !frames.IsValid() ||
		(frames.Kind() != reflect.Slice && frames.Kind() != reflect.Array) ||
		requireNonEmpty && frames.Len() == 0 {
		return false
	}
	for index := 0; index < frames.Len(); index++ {
		frame, valid := NumericValue(frames.Index(index).Interface())
		if !valid || math.IsNaN(frame) || math.IsInf(frame, 0) ||
			frame < 0 || frame != math.Trunc(frame) {
			return false
		}
	}
	return true
}

func atomicReplaceTouchesPrimary(current timeline.Document, operation map[string]any) bool {
	if StringValue(operation["kind"]) != "replace_clip" {
		return false
	}
	clipID := StringValue(operation["timeline_clip_id"])
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

func atomicTimelineCoordinateEffect(before, after timeline.Document) map[string]any {
	observationRequired := existingTimelineCoordinatesChanged(before, after)
	rippleDelta := 0
	if observationRequired {
		rippleDelta = after.DurationFrames - before.DurationFrames
	}
	return map[string]any{
		"scope":                  "existing_timeline_clips",
		"duration_frames_before": before.DurationFrames,
		"duration_frames_after":  after.DurationFrames,
		"ripple_delta_frames":    rippleDelta,
		"observation_required":   observationRequired,
	}
}

func existingTimelineCoordinatesChanged(before, after timeline.Document) bool {
	type placement struct {
		trackID string
		start   int
		end     int
	}
	afterPlacements := map[string]placement{}
	for _, track := range after.Tracks {
		for _, clip := range track.Clips {
			afterPlacements[clip.TimelineClipID] = placement{
				trackID: track.TrackID,
				start:   clip.TimelineStartFrame,
				end:     clip.TimelineEndFrame,
			}
		}
	}
	for _, track := range before.Tracks {
		for _, previous := range track.Clips {
			current, exists := afterPlacements[previous.TimelineClipID]
			if !exists ||
				current.trackID != track.TrackID ||
				current.start != previous.TimelineStartFrame ||
				current.end != previous.TimelineEndFrame {
				return true
			}
		}
	}
	return false
}

type timelineMutationBase struct {
	stateVersion    int
	timelineVersion int
	timelineID      string
}

func (exec *Executor) timelineMutationSnapshot(
	ctx context.Context,
	draftID string,
) (timeline.Document, timelineMutationBase, error) {
	var stateVersion int
	var timelineVersion sql.NullInt64
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT state_version,timeline_current_version
		FROM drafts WHERE draft_id=?`, draftID,
	).Scan(&stateVersion, &timelineVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return timeline.Document{}, timelineMutationBase{}, storage.ErrNotFound
	}
	if err != nil {
		return timeline.Document{}, timelineMutationBase{}, err
	}
	base := timelineMutationBase{stateVersion: stateVersion}
	if !timelineVersion.Valid {
		document := timeline.Empty(draftID, 0)
		document.DurationFrames = 0
		return document, base, nil
	}
	base.timelineVersion = int(timelineVersion.Int64)
	document, err := timeline.Get(ctx, exec.database, draftID, base.timelineVersion)
	if err != nil {
		return timeline.Document{}, timelineMutationBase{}, err
	}
	base.timelineID = document.TimelineID
	return document, base, nil
}

func (exec *Executor) persistTimelineFromSnapshot(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperation map[string]any,
	base timelineMutationBase,
) (rushestools.ToolResult, error) {
	return exec.persistTimelineFromSnapshotWithPreservedAudio(
		ctx, draftID, document, operation, editOperation, base, nil,
	)
}

func (exec *Executor) persistTimelineFromSnapshotWithPreservedAudio(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	operation string,
	editOperation map[string]any,
	base timelineMutationBase,
	preservedAudio map[string]timeline.Track,
) (rushestools.ToolResult, error) {
	if document.Version != base.timelineVersion+1 {
		return rushestools.ToolResult{}, fmt.Errorf(
			"timeline snapshot version mismatch: base=%d attempted=%d",
			base.timelineVersion, document.Version,
		)
	}
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	reportMap, valid, err := exec.timelineValidationReportWithPreservedAudio(
		ctx, draftID, document, preservedAudio,
	)
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
	if editOperation != nil {
		editOperations = append(editOperations, editOperation)
	}
	patchID := operation + ":" + RandomID("patch")
	timelinePayload := map[string]any{
		"timeline_id": document.TimelineID, "timeline_version": document.Version,
		"patch_id": patchID, "document_json": documentMap,
		"edit_origin": origin, "edit_operations": editOperations,
	}
	if base.timelineVersion > 0 {
		timelinePayload["parent_version"] = base.timelineVersion
	}
	result, err := reducer.Apply(ctx, exec.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: timelinePayload,
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{"timeline_version": document.Version, "validation_report": reportMap},
		},
	}, reducer.Options{Actor: actor, BaseVersion: &base.stateVersion})
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if result.Status == reducer.StatusVersionConflict {
		return exec.timelineVersionConflictResult(
			ctx, draftID, document.TimelineID, base, result,
		), nil
	}
	if result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, fmt.Errorf("timeline reducer status: %s", result.Status)
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

func (exec *Executor) timelineVersionConflictResult(
	ctx context.Context,
	draftID string,
	attemptedTimelineID string,
	base timelineMutationBase,
	result reducer.Result,
) rushestools.ToolResult {
	data := map[string]any{
		"error_code":                 string(rushestools.ErrCodeStaleTarget),
		"current_timeline_unchanged": true,
		"expected_state_version":     base.stateVersion,
		"expected_timeline_version":  base.timelineVersion,
		"previous_timeline_id":       base.timelineID,
		"attempted_timeline_id":      attemptedTimelineID,
		"recovery":                   "调用 timeline.inspect 读取最新 timeline_id 与稳定 clip ID，再基于新快照重试这一项原子编辑。",
	}
	if result.Conflict != nil {
		data["actual_state_version"] = result.Conflict.ActualStateVersion
	}
	if latest, err := timeline.Latest(ctx, exec.database, draftID); err == nil {
		data["timeline_id"] = latest.TimelineID
		data["timeline_version"] = latest.Version
	}
	return rushestools.ToolFailure(
		rushestools.StatusFailed,
		"时间线在本次原子编辑提交前已被其它请求更新；本次结果未写入。",
		rushestools.ErrCodeStaleTarget,
		"调用 timeline.inspect 读取最新 timeline_id 与稳定 clip ID，再基于新快照重试这一项原子编辑。",
		data,
	)
}

func (exec *Executor) timelineValidationReport(
	ctx context.Context,
	draftID string,
	document timeline.Document,
) (map[string]any, bool, error) {
	preservedAudio, err := exec.preservedIndependentAudioFromStoredProof(ctx, draftID, document)
	if err != nil {
		return nil, false, err
	}
	return exec.timelineValidationReportWithPreservedAudio(ctx, draftID, document, preservedAudio)
}

func (exec *Executor) timelineValidationReportWithPreservedAudio(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	preservedAudio map[string]timeline.Track,
) (map[string]any, bool, error) {
	report := validateWithPreservedIndependentAudio(document, preservedAudio)
	reportMap, valid, err := exec.timelineValidationReportFromStructural(ctx, draftID, document, report)
	if err != nil || !valid {
		return reportMap, valid, err
	}
	if err := addIndependentAudioPreservationProofs(reportMap, document, preservedAudio); err != nil {
		return nil, false, err
	}
	return reportMap, true, nil
}

func (exec *Executor) timelineValidationReportFromStructural(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	report timeline.ValidationReport,
) (map[string]any, bool, error) {
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

func (exec *Executor) toolCheckTimeline(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineCheckInput,
) (rushestools.ToolResult, error) {
	document, err := exec.requestedTimeline(ctx, draftID, input.TimelineID)
	if errors.Is(err, storage.ErrNotFound) && input.TimelineID != "" {
		return requestedTimelineNotFound(input.TimelineID), nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	report, validationErr := exec.validateStoredTimeline(ctx, draftID, document)
	if validationErr != nil {
		return rushestools.ToolResult{}, validationErr
	}
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
		"timeline_id":       document.TimelineID,
		"timeline_version":  document.Version,
		"validation_report": validationReport,
		"beat_alignment":    beatAlignment,
	}
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
	input rushestools.TimelineInspectInput,
) (rushestools.ToolResult, error) {
	document, timelineExists, isCurrent, err := exec.timelineInspectionSnapshot(
		ctx, draftID, input.TimelineID,
	)
	if errors.Is(err, storage.ErrNotFound) {
		if input.TimelineID != "" {
			return requestedTimelineNotFound(input.TimelineID), nil
		}
		timelineExists = false
		isCurrent = true
		err = nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if !timelineExists {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusSucceeded),
			Observation: "当前草稿尚无时间线；请先选择素材并创建初版时间线。",
			Data: map[string]any{
				"timeline_exists": false,
				"is_current":      true,
				"fps":             timeline.DefaultFPS,
				"duration_frames": 0,
				"tracks":          []map[string]any{},
			},
		}, nil
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
			"timeline_exists":  true,
			"timeline_id":      document.TimelineID,
			"is_current":       isCurrent,
			"timeline_version": document.Version,
			"fps":              document.FPS, "duration_frames": document.DurationFrames, "tracks": tracks,
			"audio_layout":   AudioLayoutData(document),
			"beat_alignment": BeatAlignmentData(document),
		},
	}, nil
}

func (exec *Executor) timelineInspectionSnapshot(
	ctx context.Context,
	draftID string,
	timelineID string,
) (timeline.Document, bool, bool, error) {
	var currentVersion sql.NullInt64
	var requestedVersion sql.NullInt64
	var raw sql.NullString
	query := `
		SELECT d.timeline_current_version,t.version,t.document_json
		FROM drafts d
		LEFT JOIN timeline_versions t
			ON t.draft_id=d.draft_id AND t.version=d.timeline_current_version
		WHERE d.draft_id=?`
	args := []any{draftID}
	if timelineID != "" {
		query = `
			SELECT d.timeline_current_version,t.version,t.document_json
			FROM drafts d
			LEFT JOIN timeline_versions t
				ON t.draft_id=d.draft_id AND t.timeline_id=?
			WHERE d.draft_id=?`
		args = []any{timelineID, draftID}
	}
	err := exec.database.Read().QueryRowContext(ctx, query, args...).Scan(
		&currentVersion, &requestedVersion, &raw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return timeline.Document{}, false, false, storage.ErrNotFound
	}
	if err != nil {
		return timeline.Document{}, false, false, err
	}
	if !requestedVersion.Valid || !raw.Valid {
		if timelineID == "" && !currentVersion.Valid {
			return timeline.Document{}, false, true, nil
		}
		return timeline.Document{}, false, false, storage.ErrNotFound
	}
	document, err := decodeTimelineInspectionDocument(raw.String)
	if err != nil {
		return timeline.Document{}, false, false, err
	}
	isCurrent := currentVersion.Valid && currentVersion.Int64 == requestedVersion.Int64
	return document, true, isCurrent, nil
}

func decodeTimelineInspectionDocument(raw string) (timeline.Document, error) {
	var document timeline.Document
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return timeline.Document{}, err
	}
	existing := make(map[string]struct{}, len(document.Tracks))
	for _, track := range document.Tracks {
		existing[track.TrackID] = struct{}{}
	}
	for _, required := range timeline.Empty(document.DraftID, document.Version).Tracks {
		if _, found := existing[required.TrackID]; !found {
			document.Tracks = append(document.Tracks, required)
		}
	}
	return document, nil
}

func (exec *Executor) requestedTimeline(
	ctx context.Context,
	draftID string,
	timelineID string,
) (timeline.Document, error) {
	if timelineID == "" {
		return timeline.Latest(ctx, exec.database, draftID)
	}
	return timeline.GetByID(ctx, exec.database, draftID, timelineID)
}

func requestedTimelineNotFound(timelineID string) rushestools.ToolResult {
	return rushestools.ToolFailure(
		rushestools.StatusFailed,
		"指定 timeline_id 不属于当前草稿或已不存在。",
		rushestools.ErrCodeStaleTarget,
		"省略 timeline_id 调用 timeline.inspect 读取当前稳定版本，再使用返回的 timeline_id 重试。",
		map[string]any{
			"requested_timeline_id":      timelineID,
			"current_timeline_unchanged": true,
		},
	)
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
