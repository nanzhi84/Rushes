package agentexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func (exec *Executor) toolEnqueueRender(
	ctx context.Context,
	draftID, kind, orientation string,
	expectedTimelineID *string,
) (rushestools.ToolResult, error) {
	orientation, err := normalizeRenderOrientation(orientation)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if draft.TimelineCurrentVersion == nil {
		return rushestools.ToolResult{}, errors.New("当前草稿没有时间线")
	}
	timelineVersion := *draft.TimelineCurrentVersion
	document, err := timeline.Get(ctx, exec.database, draftID, timelineVersion)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if expectedTimelineID != nil && *expectedTimelineID != document.TimelineID {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "目标时间线已经变化，未创建渲染任务",
			Data: map[string]any{
				"error_code":                 string(rushestools.ErrCodeStaleTarget),
				"requested_timeline_id":      *expectedTimelineID,
				"current_timeline_id":        document.TimelineID,
				"current_timeline_version":   timelineVersion,
				"current_timeline_unchanged": true,
				"recovery":                   "调用 timeline.inspect 读取当前 timeline_id；确认仍符合目标后，只重试这一个 render.start。",
			},
		}, nil
	}
	validationReport, valid, err := exec.timelineValidationReport(ctx, draftID, document)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if !valid {
		result, applyErr := reducer.Apply(ctx, exec.database, []contracts.Event{{
			Type: "TimelineValidationFailed", DraftID: draftID,
			Payload: map[string]any{
				"timeline_version": timelineVersion, "validation_report": validationReport,
			},
		}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
		if applyErr != nil || result.Status != reducer.StatusApplied {
			return rushestools.ToolResult{}, errors.Join(
				applyErr,
				fmt.Errorf("render validation reducer status: %s", result.Status),
			)
		}
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusValidationFailed),
			Observation: "当前时间线未通过渲染前校验，未创建渲染任务",
			Data: map[string]any{
				"reason":                     "validation_failed",
				"current_timeline_unchanged": true,
				"validation_report":          validationReport,
				"recovery":                   "根据 validation_report 修复当前时间线后重试渲染。",
			},
		}, nil
	}
	baseIdempotencyKey := fmt.Sprintf("%s:%s:%d:%s", kind, draftID, timelineVersion, orientation)
	idempotencyKey := baseIdempotencyKey
	retryOfJobID := ""
	if existing, found, err := exec.FindRenderJob(ctx, kind, baseIdempotencyKey, true); err != nil {
		return rushestools.ToolResult{}, err
	} else if found {
		if existing.Status != "failed" && existing.Status != "cancelled" {
			if !draft.TimelineValidated {
				result, applyErr := reducer.Apply(ctx, exec.database, []contracts.Event{{
					Type: "TimelineValidated", DraftID: draftID,
					Payload: map[string]any{
						"timeline_version": timelineVersion, "validation_report": validationReport,
					},
				}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
				if applyErr != nil || result.Status != reducer.StatusApplied {
					return rushestools.ToolResult{}, errors.Join(
						applyErr,
						fmt.Errorf("render validation reducer status: %s", result.Status),
					)
				}
			}
			return renderJobResult(kind, existing.ID, existing.Status, timelineVersion), nil
		}
		retryOfJobID = existing.ID
		idempotencyKey = fmt.Sprintf("%s:retry:%s", baseIdempotencyKey, existing.ID)
	}
	jobID := RandomID("job")
	jobPayload := map[string]any{"timeline_version": *draft.TimelineCurrentVersion, "orientation": orientation}
	if retryOfJobID != "" {
		jobPayload["retry_of_job_id"] = retryOfJobID
	}
	// JobEnqueued 是 merge 事件，会忽略 BaseVersion。始终同批附带 exact target 的
	// strict TimelineValidated，才能在验证 vN 后若 current 已变成 vN+1 时让整批冲突，
	// 阻止旧版本 job 入队。
	events := []contracts.Event{{
		Type: "TimelineValidated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_version": timelineVersion, "validation_report": validationReport,
		},
	}, {
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": kind, "requested_by_draft_id": draftID,
			"idempotency_key": idempotencyKey,
			"job_payload":     jobPayload,
			"next_run_at":     time.Now().UTC().Format(time.RFC3339Nano),
			"priority":        30,
			"max_retries":     2,
		},
	}}
	result, err := reducer.Apply(
		ctx,
		exec.database,
		events,
		reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion},
	)
	if err != nil || result.Status != reducer.StatusApplied {
		if existing, found, lookupErr := exec.FindRenderJob(ctx, kind, idempotencyKey, false); lookupErr != nil {
			return rushestools.ToolResult{}, errors.Join(err, lookupErr)
		} else if found {
			return renderJobResult(kind, existing.ID, existing.Status, timelineVersion), nil
		}
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return renderJobResult(kind, jobID, "pending", timelineVersion), nil
}

func (exec *Executor) toolStartRender(
	ctx context.Context,
	draftID string,
	input rushestools.RenderStartInput,
) (rushestools.ToolResult, error) {
	input.TimelineID = strings.TrimSpace(input.TimelineID)
	if input.TimelineID == "" {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "render.start 需要 timeline.inspect 返回的 timeline_id",
			Data: map[string]any{
				"current_timeline_unchanged": true,
				"recovery":                   "先调用 timeline.inspect，再原样传入当前 timeline_id。",
			},
		}, nil
	}
	var kind string
	switch strings.ToLower(strings.TrimSpace(input.Kind)) {
	case "preview":
		kind = "render_preview"
	case "final":
		kind = "render_final"
	default:
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "render.start kind 只支持 preview 或 final",
			Data: map[string]any{
				"current_timeline_unchanged": true,
				"recovery":                   "根据用户要预览还是最终成片，只选择一个 kind 后重试。",
			},
		}, nil
	}
	return exec.toolEnqueueRender(
		ctx, draftID, kind, input.Orientation, &input.TimelineID,
	)
}

func normalizeRenderOrientation(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "auto", nil
	}
	switch value {
	case "auto", "portrait", "landscape":
		return value, nil
	default:
		return "", errors.New("orientation 必须是 auto、portrait 或 landscape")
	}
}

type renderJobRef struct {
	ID     string
	Status string
}

func (exec *Executor) FindRenderJob(
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
	err := exec.database.Read().QueryRowContext(ctx, query, arguments...).Scan(&job.ID, &job.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return renderJobRef{}, false, nil
	}
	if err != nil {
		return renderJobRef{}, false, err
	}
	return job, true, nil
}

func renderJobResult(kind, jobID, jobStatus string, timelineVersion int) rushestools.ToolResult {
	status := jobStatus
	observation := kind + " 任务已存在"
	switch jobStatus {
	case "pending", "running":
		status = "queued"
		observation = kind + " 任务已排队"
	case "succeeded":
		observation = kind + " 任务已完成"
	}
	renderKind := strings.TrimPrefix(kind, "render_")
	return rushestools.ToolResult{
		Status: status, Observation: observation,
		Data: map[string]any{
			"job_id": jobID, "job_status": jobStatus,
			"render_kind": renderKind, "timeline_version": timelineVersion,
		},
	}
}

func (exec *Executor) toolReadJob(
	ctx context.Context,
	draftID string,
	input rushestools.JobReadInput,
) (rushestools.ToolResult, error) {
	jobID := strings.TrimSpace(input.JobID)
	if jobID == "" {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "job.read 需要一个 job_id",
			Data: map[string]any{
				"current_state_unchanged": true,
				"recovery":                "使用检测工具或 render.start 返回的 job_id。",
			},
		}, nil
	}
	var kind, status string
	var assetID, resultJSON, errorJSON sql.NullString
	var progress sql.NullFloat64
	var attempts, maxRetries int
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT kind,status,asset_id,progress,result_json,error_json,attempts,max_retries
		FROM jobs
		WHERE job_id=? AND (draft_id=? OR requested_by_draft_id=?)`,
		jobID, draftID, draftID,
	).Scan(
		&kind, &status, &assetID, &progress, &resultJSON, &errorJSON,
		&attempts, &maxRetries,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "job 不存在或不属于当前草稿",
			Data: map[string]any{
				"error_code":              string(rushestools.ErrCodeStaleTarget),
				"job_id":                  jobID,
				"current_state_unchanged": true,
			},
		}, nil
	}
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	data := map[string]any{
		"job_id": jobID, "kind": kind, "job_status": status,
		"attempts": attempts, "max_retries": maxRetries,
	}
	if assetID.Valid && assetID.String != "" {
		data["asset_id"] = assetID.String
	}
	if progress.Valid {
		data["progress"] = progress.Float64
	}
	if resultJSON.Valid {
		var result map[string]any
		if json.Unmarshal([]byte(resultJSON.String), &result) == nil {
			if filtered := boundedJobResult(result); len(filtered) > 0 {
				data["result"] = filtered
			}
		}
	}
	if errorJSON.Valid {
		var failure map[string]any
		if json.Unmarshal([]byte(errorJSON.String), &failure) == nil {
			if filtered := boundedJobFailure(failure); len(filtered) > 0 {
				data["error"] = filtered
			}
		}
	}
	return rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded),
		Observation: fmt.Sprintf(
			"job %s 状态为 %s", jobID, status,
		),
		Data: data,
	}, nil
}

func boundedJobResult(result map[string]any) map[string]any {
	filtered := map[string]any{}
	for _, key := range []string{
		"artifact_id", "timeline_version", "profile", "orientation",
		"summary_id", "transcript_id", "asset_id",
	} {
		if value, exists := result[key]; exists {
			filtered[key] = value
		}
	}
	return filtered
}

func boundedJobFailure(failure map[string]any) map[string]any {
	filtered := map[string]any{}
	if code, ok := failure["error_code"].(string); ok {
		filtered["error_code"] = boundedJobFailureText(code, jobFailureCodeRuneLimit)
	}
	if message, ok := failure["message"].(string); ok {
		filtered["message"] = boundedJobFailureText(message, jobFailureMessageRuneLimit)
	}
	if retryable, ok := failure["retryable"].(bool); ok {
		filtered["retryable"] = retryable
	}
	return filtered
}

const (
	jobFailureCodeRuneLimit    = 64
	jobFailureMessageRuneLimit = 320
)

var quotedAbsoluteJobPathPattern = regexp.MustCompile(
	`"(?:/[^"\r\n]+|[A-Za-z]:\\[^"\r\n]+)"|'(?:/[^'\r\n]+|[A-Za-z]:\\[^'\r\n]+)'`,
)

var absoluteJobFilePathPattern = regexp.MustCompile(
	`(?i)(^|[\s=:('"\\[])(/(?:[^\r\n:;,"')\]]*?\.[[:alnum:]]{1,8})|[A-Za-z]:\\(?:[^\r\n:;,"')\]]*?\.[[:alnum:]]{1,8}))($|[\s:;,)'"\]])`,
)

var absoluteJobPathTokenPattern = regexp.MustCompile(
	`(^|[\s=:('"\\[])(/(?:[^ \t\r\n,;)'"\]]+)|[A-Za-z]:\\(?:[^ \t\r\n,;)'"\]]+))`,
)

func boundedJobFailureText(value string, limit int) string {
	value = strings.TrimSpace(value)
	value = quotedAbsoluteJobPathPattern.ReplaceAllString(value, `<local-path>`)
	value = absoluteJobFilePathPattern.ReplaceAllString(value, `${1}<local-path>${3}`)
	value = absoluteJobPathTokenPattern.ReplaceAllString(value, `${1}<local-path>`)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

func (exec *Executor) toolRenderStatus(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	return rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded), Observation: "已读取渲染状态",
		Data: map[string]any{
			"preview_id": draft.PreviewCurrentID, "export_id": draft.ExportCurrentID,
			"running_jobs": draft.RunningJobs,
		},
	}, nil
}

func (exec *Executor) toolCheckPreview(
	ctx context.Context,
	draftID string,
	input rushestools.PreviewCheckInput,
) (rushestools.PreviewInspectionResult, error) {
	check, err := NormalizePreviewCheck(input.Check)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	return exec.inspectPreviewCheck(ctx, draftID, input.PreviewID, check)
}

func (exec *Executor) inspectPreviewCheck(
	ctx context.Context,
	draftID string,
	previewID string,
	check string,
) (rushestools.PreviewInspectionResult, error) {
	var hash string
	var timelineVersion int
	var width, height sql.NullInt64
	var fps, duration sql.NullFloat64
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT object_hash,timeline_version,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE preview_id=? AND draft_id=?`, previewID, draftID).Scan(
		&hash, &timelineVersion, &width, &height, &fps, &duration,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return rushestools.PreviewInspectionResult{}, storage.ErrNotFound
	}
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	path, err := exec.database.Paths.ObjectPath(hash)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	document, err := timeline.Get(ctx, exec.database, draftID, timelineVersion)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	expected, err := media.TimelineInspectionIntent(ctx, exec.database, document)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	expected.Width = int(width.Int64)
	expected.Height = int(height.Int64)
	expected.FPS = fps.Float64
	expected.DurationSec = duration.Float64
	inspection, err := media.InspectVideo(ctx, path, expected, []string{check})
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	result := rushestools.PreviewInspectionResult{}
	if check == "visual" {
		frameContext, contextErr := exec.PreviewInspectionFrameContext(
			ctx, document, understanding.PreviewInspectionFrameNumbers(document),
		)
		if contextErr != nil {
			return rushestools.PreviewInspectionResult{}, contextErr
		}
		visual, visualErr := exec.analyzer.InspectPreview(
			ctx, exec.database.Paths, path, document, frameContext,
		)
		if visualErr != nil {
			return rushestools.PreviewInspectionResult{}, visualErr
		}
		inspection.Degraded = inspection.Degraded || visual.Degraded
		if visual.Degraded {
			inspection.Issues = append(inspection.Issues, media.InspectionIssue{
				Check: "dependencies", Severity: "warning", Message: "未配置视觉模型，已跳过 contact sheet 视觉检查。",
			})
		}
		for _, finding := range visual.Findings {
			inspection.Issues = append(inspection.Issues, media.InspectionIssue{
				Check: finding.Check, Severity: finding.Severity, Message: finding.Message, Frames: finding.Frames,
			})
		}
		result.VisualFrameCount = visual.FrameCount
		result.VisualLatencyMS = visual.LatencyMS
		result.VisualPromptTokens = visual.PromptTokens
		result.VisualTotalTokens = visual.TotalTokens
	}
	media.FinalizeInspectionSummary(&inspection)
	issues := make([]map[string]interface{}, 0, len(inspection.Issues))
	for _, issue := range inspection.Issues {
		item := map[string]interface{}{
			"check": issue.Check, "severity": issue.Severity, "message": issue.Message,
		}
		if issue.ErrorCode != "" {
			item["error_code"] = issue.ErrorCode
		}
		if len(issue.Frames) > 0 {
			item["frames"] = issue.Frames
		}
		issues = append(issues, item)
	}
	result.PreviewID = previewID
	result.Check = check
	result.Summary = inspection.Summary
	result.Degraded = inspection.Degraded
	result.Issues = issues
	return result, nil
}

func NormalizePreviewCheck(raw string) (string, error) {
	allowed := map[string]struct{}{
		"decode": {}, "black": {}, "freeze": {}, "silence": {}, "loudness": {}, "visual": {},
	}
	check := strings.TrimSpace(raw)
	if check == "" {
		return "", errors.New("preview.check 需要一个 check")
	}
	if _, ok := allowed[check]; !ok {
		return "", fmt.Errorf("未知的预览质检项 %q；只支持 decode、black、freeze、silence、loudness 或 visual", check)
	}
	return check, nil
}

func (exec *Executor) PreviewInspectionFrameContext(
	ctx context.Context,
	document timeline.Document,
	frames []int,
) (map[int]string, error) {
	transcriptCache := map[string]storage.Transcript{}
	missingTranscript := map[string]struct{}{}
	result := make(map[int]string, len(frames))
	for _, frame := range frames {
		parts := []string{}
		audioClips := audibleSpeechClipsAtFrame(document, frame)
		for _, clip := range audioClips {
			if clip.AssetID == "" {
				continue
			}
			transcript, cached := transcriptCache[clip.AssetID]
			if !cached {
				if _, missing := missingTranscript[clip.AssetID]; missing {
					continue
				}
				loaded, err := storage.LatestTranscript(ctx, exec.database.Read(), clip.AssetID)
				if errors.Is(err, storage.ErrNotFound) {
					missingTranscript[clip.AssetID] = struct{}{}
					continue
				}
				if err != nil {
					return nil, err
				}
				transcript = loaded
				transcriptCache[clip.AssetID] = loaded
			}
			rate := clip.PlaybackRate
			if rate <= 0 {
				rate = 1
			}
			timelineOffset := float64(frame - clip.TimelineStartFrame)
			sourceFrame := clip.SourceStartFrame + int(math.Floor(timelineOffset*rate))
			sourceEndFrame := clip.SourceStartFrame + int(math.Ceil((timelineOffset+1)*rate))
			sourceEndFrame = max(sourceFrame+1, sourceEndFrame)
			sourceFrame = max(clip.SourceStartFrame, sourceFrame)
			sourceEndFrame = min(clip.SourceEndFrame, sourceEndFrame)
			if sourceEndFrame <= sourceFrame {
				continue
			}
			if text := TranscriptTextForSourceRange(transcript.Utterances, sourceFrame, sourceEndFrame); text != "" {
				parts = append(parts, "同帧台词："+truncatePreviewContextText(text, 512))
			}
		}
		for _, clip := range timelineClipsAtFrame(document, frame, "subtitles") {
			if text := strings.TrimSpace(clip.Text); text != "" {
				parts = append(parts, "同帧字幕："+truncatePreviewContextText(text, 256))
			}
		}
		result[frame] = strings.Join(parts, "；")
	}
	return result, nil
}

func audibleSpeechClipsAtFrame(document timeline.Document, frame int) []timeline.Clip {
	audioTrackIDs := map[string]struct{}{
		"original_audio": {}, "voiceover": {}, "bgm": {}, "sfx": {},
	}
	hasSolo := false
	for _, track := range document.Tracks {
		if _, audio := audioTrackIDs[track.TrackID]; audio && track.Solo && !track.Muted {
			hasSolo = true
		}
	}
	result := []timeline.Clip{}
	for _, track := range document.Tracks {
		if track.TrackID != "original_audio" && track.TrackID != "voiceover" {
			continue
		}
		if track.Muted || hasSolo && !track.Solo {
			continue
		}
		if track.TrackID == "original_audio" && len(track.Clips) == 0 {
			result = append(result, primaryClipsAtFrame(document, frame)...)
			continue
		}
		for _, clip := range track.Clips {
			if frame >= clip.TimelineStartFrame && frame < clip.TimelineEndFrame {
				result = append(result, clip)
			}
		}
	}
	return result
}

func primaryClipsAtFrame(document timeline.Document, frame int) []timeline.Clip {
	result := []timeline.Clip{}
	for _, track := range document.Tracks {
		if track.TrackID != "visual_base" {
			continue
		}
		for _, clip := range track.Clips {
			if frame >= clip.TimelineStartFrame && frame < clip.TimelineEndFrame {
				result = append(result, clip)
			}
		}
	}
	return result
}

func truncatePreviewContextText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func timelineClipsAtFrame(document timeline.Document, frame int, trackIDs ...string) []timeline.Clip {
	wanted := map[string]struct{}{}
	for _, trackID := range trackIDs {
		wanted[trackID] = struct{}{}
	}
	result := []timeline.Clip{}
	for _, track := range document.Tracks {
		if _, ok := wanted[track.TrackID]; !ok || track.Muted {
			continue
		}
		for _, clip := range track.Clips {
			if frame >= clip.TimelineStartFrame && frame < clip.TimelineEndFrame {
				result = append(result, clip)
			}
		}
	}
	return result
}
