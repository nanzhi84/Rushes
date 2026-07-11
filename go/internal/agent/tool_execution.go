package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func (service *Service) ExecuteTool(ctx context.Context, name string, input any) (any, error) {
	draftID, err := rushestools.DraftID(ctx)
	if err != nil {
		return nil, err
	}
	switch name {
	case "asset.import_local_file":
		return nil, errors.New("本地导入仅由已确认的 REST 文件选择流程执行")
	case "asset.list_assets":
		return service.toolListAssets(ctx, draftID, input.(rushestools.AssetListInput))
	case "understand.materials":
		return service.toolUnderstand(ctx, draftID, input.(rushestools.UnderstandInput))
	case "interaction.ask_user":
		return service.toolAskUser(ctx, draftID, input.(rushestools.AskUserInput), nil)
	case "interaction.confirm_action":
		confirmation := input.(rushestools.ConfirmActionInput)
		return service.toolAskUser(ctx, draftID, rushestools.AskUserInput{
			Question: confirmation.Question,
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "confirm", Label: "确认"}, {OptionID: "cancel", Label: "取消"},
			},
		}, map[string]any{"tool_name": confirmation.ToolName, "arguments": confirmation.Arguments})
	case "decision.answer":
		return service.toolDecisionAnswer(ctx, draftID, input.(rushestools.DecisionAnswerInput))
	case "timeline.compose_initial":
		return service.toolComposeInitial(ctx, draftID, input.(rushestools.ComposeInitialInput))
	case "timeline.apply_patch":
		return service.toolApplyPatch(ctx, draftID, input.(rushestools.TimelinePatchInput))
	case "timeline.validate":
		return service.toolValidateTimeline(ctx, draftID)
	case "timeline.inspect":
		return service.toolInspectTimeline(ctx, draftID, input.(rushestools.TimelineInspectInput))
	case "timeline.restore_version":
		return service.toolRestoreTimeline(ctx, draftID, input.(rushestools.TimelineRestoreInput))
	case "render.preview":
		return service.toolEnqueueRender(ctx, draftID, "render_preview")
	case "render.final_mp4":
		return service.toolEnqueueRender(ctx, draftID, "render_final")
	case "render.status":
		return service.toolRenderStatus(ctx, draftID)
	case "render.inspect_preview":
		return service.toolInspectPreview(ctx, draftID, input.(rushestools.RenderInspectInput))
	default:
		return nil, fmt.Errorf("工具未注册执行器: %s", name)
	}
}

func (service *Service) toolListAssets(
	ctx context.Context,
	draftID string,
	input rushestools.AssetListInput,
) (rushestools.AssetListResult, error) {
	assets, err := storage.ListDraftAssets(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.AssetListResult{}, err
	}
	result := rushestools.AssetListResult{DraftID: draftID, Assets: []rushestools.AssetManifest{}}
	for _, asset := range assets {
		if input.Kind != "" && asset.Kind != input.Kind || input.After != "" && asset.ID <= input.After {
			continue
		}
		if input.OnlyUsable != nil && *input.OnlyUsable != asset.Usable {
			continue
		}
		duration, _ := numericValue(asset.Probe["duration_sec"])
		result.Assets = append(result.Assets, rushestools.AssetManifest{
			AssetID: asset.ID, Filename: asset.Filename, Kind: asset.Kind,
			DurationSec: duration, Usable: asset.Usable, IngestStatus: asset.IngestStatus,
			UnderstandingStatus: asset.UnderstandingStatus,
		})
	}
	result.Total = len(result.Assets)
	limit := input.Limit
	if limit <= 0 || limit > 200 {
		limit = min(200, len(result.Assets))
	}
	if len(result.Assets) > limit {
		result.Assets = result.Assets[:limit]
		result.NextAfter = result.Assets[len(result.Assets)-1].AssetID
	}
	return result, nil
}

func (service *Service) toolUnderstand(
	ctx context.Context,
	draftID string,
	input rushestools.UnderstandInput,
) (rushestools.UnderstandResult, error) {
	if len(input.AssetIDs) == 0 {
		return rushestools.UnderstandResult{}, errors.New("understand.materials 至少需要一个 asset_id")
	}
	jobID := randomID("understand")
	for index, assetID := range input.AssetIDs {
		if input.Focus == "e2e_cancel" && index > 0 {
			select {
			case <-ctx.Done():
				return rushestools.UnderstandResult{}, ctx.Err()
			case <-time.After(30 * time.Second):
			}
		}
		if err := ctx.Err(); err != nil {
			return rushestools.UnderstandResult{}, err
		}
		asset, err := storage.GetAsset(ctx, service.database.Read(), assetID)
		if err != nil {
			return rushestools.UnderstandResult{}, err
		}
		started, err := reducer.Apply(ctx, service.database, []contracts.Event{{
			Type:    "MaterialUnderstandingStarted",
			Payload: map[string]any{"asset_id": assetID, "job_id": jobID},
		}}, reducer.Options{Actor: contracts.ActorAgent})
		if err != nil || started.Status != reducer.StatusApplied {
			return rushestools.UnderstandResult{}, errors.Join(err, fmt.Errorf("start reducer status: %s", started.Status))
		}
		summary, analyzeErr := service.analyzer.Analyze(ctx, service.database, asset, input.Focus, func(note string) {
			service.hub.Record(draftID, StreamEvent{
				"type": "subagent_progress", "tool": "understand.materials",
				"asset_id": assetID, "note": note, "completed": index, "total": len(input.AssetIDs),
			})
		})
		if analyzeErr != nil {
			cancelled := errors.Is(analyzeErr, context.Canceled)
			_, _ = reducer.Apply(context.WithoutCancel(ctx), service.database, []contracts.Event{{
				Type: "MaterialUnderstandingFailed",
				Payload: map[string]any{
					"asset_id": assetID, "job_id": jobID, "cancelled": cancelled,
					"failure": map[string]any{"error_code": "understanding_failed", "message": analyzeErr.Error()},
				},
			}}, reducer.Options{Actor: contracts.ActorAgent})
			return rushestools.UnderstandResult{}, analyzeErr
		}
		var summaryMap map[string]any
		encoded, _ := json.Marshal(summary)
		_ = json.Unmarshal(encoded, &summaryMap)
		summaryID := randomID("summary")
		completed, err := reducer.Apply(ctx, service.database, []contracts.Event{{
			Type:    "MaterialUnderstandingCompleted",
			Payload: map[string]any{"asset_id": assetID, "job_id": jobID, "summary_id": summaryID},
		}}, reducer.Options{
			Actor: contracts.ActorAgent,
			ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
				ID: summaryID, AssetID: assetID, Version: 0,
				Focus: stringPointerValue(input.Focus), Status: "ready", Summary: summaryMap,
				Model: stringPointerValue(summary.Model), PromptVersion: stringPointerValue("go-mini-loop-v1"),
			}}},
		})
		if err != nil || completed.Status != reducer.StatusApplied {
			return rushestools.UnderstandResult{}, errors.Join(err, fmt.Errorf("complete reducer status: %s", completed.Status))
		}
		service.hub.Record(draftID, StreamEvent{
			"type": "subagent_progress", "tool": "understand.materials",
			"asset_id": assetID, "note": "摘要已完成", "completed": index + 1, "total": len(input.AssetIDs),
		})
	}
	return rushestools.UnderstandResult{
		DraftID: draftID, JobID: jobID, AssetIDs: input.AssetIDs, Status: "completed",
	}, nil
}

func stringPointerValue(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

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
	for _, clip := range input.Clips {
		selections = append(selections, timeline.Selection{
			AssetID: clip.AssetID, SourceStart: clip.SourceStart,
			SourceEnd: clip.SourceEnd, Role: clip.Role,
		})
	}
	document, err := timeline.ComposeInitial(draftID, version, selections)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return service.persistTimeline(ctx, draftID, document, nil, "compose_initial")
}

func (service *Service) toolApplyPatch(
	ctx context.Context,
	draftID string,
	input rushestools.TimelinePatchInput,
) (rushestools.ToolResult, error) {
	current, err := timeline.Latest(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document, err := timeline.ApplyPatch(current, input.Op)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	next, err := timeline.NextVersion(ctx, service.database, draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	document.Version = next
	document.TimelineID = fmt.Sprintf("%s:v%d", draftID, next)
	parent := current.Version
	return service.persistTimeline(ctx, draftID, document, &parent, "apply_patch")
}

func (service *Service) persistTimeline(
	ctx context.Context,
	draftID string,
	document timeline.Document,
	parent *int,
	operation string,
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
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{
		{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: map[string]any{
				"timeline_id": document.TimelineID, "timeline_version": document.Version,
				"parent_version": parent, "patch_id": operation + ":" + randomID("patch"),
				"document_json": documentMap,
			},
		},
		{
			Type: validationType, DraftID: draftID,
			Payload: map[string]any{"timeline_version": document.Version, "validation_report": reportMap},
		},
	}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("timeline reducer status: %s", result.Status))
	}
	status := "succeeded"
	if !report.Valid {
		status = "validation_failed"
	}
	return rushestools.ToolResult{
		Status: status, Observation: timeline.Inspect(document),
		Data: map[string]any{"timeline_version": document.Version, "validation_report": reportMap},
	}, nil
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
	eventType := "TimelineValidated"
	if !report.Valid {
		eventType = "TimelineValidationFailed"
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: eventType, DraftID: draftID,
		Payload: map[string]any{
			"timeline_version":  document.Version,
			"validation_report": map[string]any{"valid": report.Valid, "checks": report.Checks, "issues": report.Issues},
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("validation reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{
		Status:      map[bool]string{true: "succeeded", false: "validation_failed"}[report.Valid],
		Observation: timeline.Inspect(document), Data: map[string]any{"validation_report": report},
	}, nil
}

func (service *Service) toolInspectTimeline(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineInspectInput,
) (rushestools.ToolResult, error) {
	var document timeline.Document
	var err error
	if input.Version > 0 {
		document, err = timeline.Get(ctx, service.database, draftID, input.Version)
	} else {
		document, err = timeline.Latest(ctx, service.database, draftID)
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return rushestools.ToolResult{
		Status: "succeeded", Observation: timeline.Inspect(document),
		Data: map[string]any{"timeline_version": document.Version},
	}, nil
}

func (service *Service) toolRestoreTimeline(
	ctx context.Context,
	draftID string,
	input rushestools.TimelineRestoreInput,
) (rushestools.ToolResult, error) {
	if _, err := timeline.Get(ctx, service.database, draftID, input.SourceVersion); err != nil {
		return rushestools.ToolResult{}, err
	}
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{"timeline_version": input.SourceVersion},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("restore reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{
		Status: "succeeded", Observation: fmt.Sprintf("已恢复时间线 v%d", input.SourceVersion),
		Data: map[string]any{"timeline_version": input.SourceVersion},
	}, nil
}

func (service *Service) toolAskUser(
	ctx context.Context,
	draftID string,
	input rushestools.AskUserInput,
	pending map[string]any,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	decisionID := randomID("decision")
	options := make([]map[string]any, 0, len(input.Options))
	for _, option := range input.Options {
		options = append(options, map[string]any{
			"option_id": option.OptionID, "label": option.Label, "description": option.Description,
		})
	}
	blocking := true
	if input.Blocking != nil {
		blocking = *input.Blocking
	}
	allowFreeText := true
	if input.AllowFreeText != nil {
		allowFreeText = *input.AllowFreeText
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": decisionID, "scope_type": "draft", "type": "generic",
			"question": input.Question, "options": options, "blocking": blocking,
			"allow_free_text": allowFreeText, "pending_tool_call": pending,
			"pending_tool_call_status": "pending",
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{
		Status: "waiting", Observation: "等待用户回答", Data: map[string]any{"decision_id": decisionID},
	}, nil
}

func (service *Service) toolDecisionAnswer(
	ctx context.Context,
	draftID string,
	input rushestools.DecisionAnswerInput,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": input.DecisionID, "scope_type": "draft",
			"answer": map[string]any{
				"option_id": input.OptionID, "free_text": input.FreeText,
				"payload": input.Payload, "answered_via": "agent",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return rushestools.ToolResult{Status: "succeeded", Observation: "决策已回答"}, nil
}

func (service *Service) toolEnqueueRender(
	ctx context.Context,
	draftID, kind string,
) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if draft.TimelineCurrentVersion == nil || !draft.TimelineValidated {
		return rushestools.ToolResult{}, errors.New("当前时间线尚未验证")
	}
	baseIdempotencyKey := fmt.Sprintf("%s:%s:%d", kind, draftID, *draft.TimelineCurrentVersion)
	idempotencyKey := baseIdempotencyKey
	retryOfJobID := ""
	if existing, found, err := service.findRenderJob(ctx, kind, baseIdempotencyKey, true); err != nil {
		return rushestools.ToolResult{}, err
	} else if found {
		if existing.Status != "failed" && existing.Status != "cancelled" {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		retryOfJobID = existing.ID
		idempotencyKey = fmt.Sprintf("%s:retry:%s", baseIdempotencyKey, existing.ID)
	}
	jobID := randomID("job")
	jobPayload := map[string]any{"timeline_version": *draft.TimelineCurrentVersion}
	if retryOfJobID != "" {
		jobPayload["retry_of_job_id"] = retryOfJobID
	}
	result, err := reducer.Apply(ctx, service.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": kind, "requested_by_draft_id": draftID,
			"idempotency_key": idempotencyKey,
			"job_payload":     jobPayload,
			"next_run_at":     time.Now().UTC().Format(time.RFC3339Nano), "priority": 30,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		if existing, found, lookupErr := service.findRenderJob(ctx, kind, idempotencyKey, false); lookupErr != nil {
			return rushestools.ToolResult{}, errors.Join(err, lookupErr)
		} else if found {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return renderJobResult(kind, jobID, "pending"), nil
}

type renderJobRef struct {
	ID     string
	Status string
}

func (service *Service) findRenderJob(
	ctx context.Context,
	kind, idempotencyKey string,
	includeRetries bool,
) (renderJobRef, bool, error) {
	query := "SELECT job_id, status FROM jobs WHERE kind=? AND idempotency_key=? LIMIT 1"
	arguments := []any{kind, idempotencyKey}
	if includeRetries {
		retryPrefix := idempotencyKey + ":retry:"
		query = `SELECT job_id, status FROM jobs
			WHERE kind=? AND (idempotency_key=? OR substr(idempotency_key, 1, length(?))=?)
			ORDER BY rowid DESC LIMIT 1`
		arguments = []any{kind, idempotencyKey, retryPrefix, retryPrefix}
	}
	var job renderJobRef
	err := service.database.Read().QueryRowContext(ctx, query, arguments...).Scan(&job.ID, &job.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return renderJobRef{}, false, nil
	}
	if err != nil {
		return renderJobRef{}, false, err
	}
	return job, true, nil
}

func renderJobResult(kind, jobID, jobStatus string) rushestools.ToolResult {
	status := jobStatus
	observation := kind + " 任务已存在"
	switch jobStatus {
	case "pending", "running":
		status = "queued"
		observation = kind + " 任务已排队"
	case "succeeded":
		observation = kind + " 任务已完成"
	}
	return rushestools.ToolResult{
		Status: status, Observation: observation,
		Data: map[string]any{"job_id": jobID, "job_status": jobStatus},
	}
}

func (service *Service) toolRenderStatus(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return rushestools.ToolResult{
		Status: "succeeded", Observation: "已读取渲染状态",
		Data: map[string]any{
			"preview_id": draft.PreviewCurrentID, "export_id": draft.ExportCurrentID,
			"running_jobs": draft.RunningJobs,
		},
	}, nil
}

func (service *Service) toolInspectPreview(
	ctx context.Context,
	draftID string,
	input rushestools.RenderInspectInput,
) (rushestools.PreviewInspectionResult, error) {
	var hash string
	var width, height sql.NullInt64
	var fps, duration sql.NullFloat64
	err := service.database.Read().QueryRowContext(ctx, `
		SELECT object_hash,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE preview_id=? AND draft_id=?`, input.PreviewID, draftID).Scan(
		&hash, &width, &height, &fps, &duration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rushestools.PreviewInspectionResult{}, storage.ErrNotFound
	}
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	path, err := service.database.Paths.ObjectPath(hash)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	inspection, err := media.InspectVideo(ctx, path, media.ExpectedVideo{
		Width: int(width.Int64), Height: int(height.Int64),
		FPS: fps.Float64, DurationSec: duration.Float64,
	}, input.Checks)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	issues := make([]map[string]interface{}, 0, len(inspection.Issues))
	for _, issue := range inspection.Issues {
		issues = append(issues, map[string]interface{}{
			"check": issue.Check, "severity": issue.Severity, "message": issue.Message,
		})
	}
	return rushestools.PreviewInspectionResult{
		Summary: inspection.Summary, Degraded: inspection.Degraded, Issues: issues,
	}, nil
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}
