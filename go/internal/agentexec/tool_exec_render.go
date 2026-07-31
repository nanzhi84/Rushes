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

func (exec *Executor) enqueuePreviewRender(
	ctx context.Context,
	draftID, orientation string,
	expectedTimelineID string,
) (rushestools.ToolResult, error) {
	const kind = "render_preview"
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
	if expectedTimelineID != document.TimelineID {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "目标时间线已经变化，未创建渲染任务",
			Data: map[string]any{
				"error_code":                 string(rushestools.ErrCodeStaleTarget),
				"requested_timeline_id":      expectedTimelineID,
				"current_timeline_id":        document.TimelineID,
				"current_timeline_version":   timelineVersion,
				"current_timeline_unchanged": true,
				"recovery":                   "调用 timeline.inspect 读取当前 timeline_id；确认仍符合目标后，只重试当前预览生成。",
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
			return renderJobResult(
				kind, existing.ID, existing.Status, document.TimelineID, timelineVersion, orientation,
			), nil
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
			return renderJobResult(
				kind, existing.ID, existing.Status, document.TimelineID, timelineVersion, orientation,
			), nil
		}
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return renderJobResult(kind, jobID, "pending", document.TimelineID, timelineVersion, orientation), nil
}

func (exec *Executor) toolGeneratePreview(
	ctx context.Context,
	draftID string,
	input rushestools.PreviewGenerateInput,
) (rushestools.ToolResult, error) {
	input.TimelineID = strings.TrimSpace(input.TimelineID)
	if input.TimelineID == "" {
		return rushestools.ToolResult{
			Status:      string(rushestools.StatusFailed),
			Observation: "preview.generate 需要 timeline.inspect 返回的 timeline_id",
			Data: map[string]any{
				"current_timeline_unchanged": true,
				"recovery":                   "先调用 timeline.inspect，再原样传入当前 timeline_id。",
			},
		}, nil
	}
	queued, err := exec.enqueuePreviewRender(
		ctx, draftID, input.Orientation, input.TimelineID,
	)
	if err != nil || queued.Status == string(rushestools.StatusFailed) ||
		queued.Status == string(rushestools.StatusValidationFailed) {
		return queued, err
	}
	jobID := strings.TrimSpace(InterfaceString(queued.Data["job_id"]))
	if jobID == "" {
		return rushestools.ToolResult{}, errors.New("preview.generate 入队结果缺少 job_id")
	}
	timelineVersion, ok := positiveIntValue(queued.Data["timeline_version"])
	if !ok {
		return rushestools.ToolResult{}, errors.New("preview.generate 入队结果缺少 timeline_version")
	}
	orientation := strings.TrimSpace(InterfaceString(queued.Data["orientation"]))
	return exec.waitForPreviewJob(
		ctx, draftID, jobID, input.TimelineID, timelineVersion, orientation,
	)
}

type previewJobState struct {
	status     string
	resultJSON sql.NullString
	errorJSON  sql.NullString
}

func (exec *Executor) waitForPreviewJob(
	ctx context.Context,
	draftID, jobID, timelineID string,
	timelineVersion int,
	orientation string,
) (rushestools.ToolResult, error) {
	waitStarted := time.Now()
	terminalStatus := "failed"
	defer func() { exec.observeSameTurnToolWait("preview", terminalStatus, waitStarted) }()
	interval := exec.jobPollInterval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	waitTimeout := exec.jobWaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Minute
	}
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	var lastStatus string
	for {
		state, err := exec.previewJobState(ctx, draftID, jobID)
		if err != nil {
			return rushestools.ToolResult{}, err
		}
		lastStatus = state.status
		switch state.status {
		case "succeeded":
			terminalStatus = "succeeded"
			var result map[string]any
			if !state.resultJSON.Valid || json.Unmarshal([]byte(state.resultJSON.String), &result) != nil {
				return rushestools.ToolResult{}, errors.New("preview.generate 成功 job 缺少有效 result_json")
			}
			previewID := strings.TrimSpace(InterfaceString(result["artifact_id"]))
			resultTimelineVersion, validVersion := positiveIntValue(result["timeline_version"])
			if previewID == "" || !validVersion || resultTimelineVersion != timelineVersion {
				return rushestools.ToolResult{}, fmt.Errorf(
					"preview.generate job 结果与请求不一致: preview_id=%q timeline_version=%d",
					previewID, resultTimelineVersion,
				)
			}
			return rushestools.ToolResult{
				Status:      string(rushestools.StatusSucceeded),
				Observation: "预览已生成，可直接使用 preview_id 继续质检",
				Data: map[string]any{
					"preview_id": previewID, "job_id": jobID, "job_status": state.status,
					"timeline_id": timelineID, "timeline_version": timelineVersion,
					"orientation": orientation,
				},
			}, nil
		case "failed", "cancelled":
			terminalStatus = state.status
			data := map[string]any{
				"job_id": jobID, "job_status": state.status,
				"timeline_id": timelineID, "timeline_version": timelineVersion,
				"orientation": orientation,
			}
			if state.errorJSON.Valid {
				var failure map[string]any
				if json.Unmarshal([]byte(state.errorJSON.String), &failure) == nil {
					data["error"] = boundedJobFailure(failure)
				}
			}
			status := rushestools.StatusFailed
			if state.status == "cancelled" {
				status = rushestools.StatusCancelled
			}
			return rushestools.ToolResult{
				Status: string(status),
				Observation: fmt.Sprintf(
					"预览渲染已到终态：%s", state.status,
				),
				Data: data,
			}, nil
		case "pending", "running":
			// 同一次工具调用持续等待；父 turn 取消只停止 waiter，不取消可复用 job。
		default:
			return rushestools.ToolResult{}, fmt.Errorf(
				"preview.generate job 状态无效: %s", state.status,
			)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				terminalStatus = "timeout"
				return previewWaitTimeoutResult(
					jobID, lastStatus, timelineID, timelineVersion, orientation,
				), nil
			}
			terminalStatus = "turn_cancelled"
			return rushestools.ToolResult{}, ctx.Err()
		case <-timer.C:
			terminalStatus = "timeout"
			return previewWaitTimeoutResult(
				jobID, lastStatus, timelineID, timelineVersion, orientation,
			), nil
		case <-ticker.C:
		}
	}
}

func previewWaitTimeoutResult(
	jobID, underlyingStatus, timelineID string,
	timelineVersion int,
	orientation string,
) rushestools.ToolResult {
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusTimeout),
		Observation: "等待预览渲染终态超时；底层 job 保持运行，迟到完成不会自动续跑模型",
		Data: map[string]any{
			"error_code": string(rushestools.ErrCodeToolTimeout),
			"job_id":     jobID, "job_status": underlyingStatus,
			"underlying_job_continues": underlyingStatus == "pending" || underlyingStatus == "running",
			"timeline_id":              timelineID, "timeline_version": timelineVersion,
			"orientation": orientation,
		},
	}
}

func (exec *Executor) previewJobState(
	ctx context.Context,
	draftID, jobID string,
) (previewJobState, error) {
	var state previewJobState
	err := exec.database.Read().QueryRowContext(ctx, `
		SELECT status,result_json,error_json
		FROM jobs
		WHERE job_id=? AND kind='render_preview'
		AND (draft_id=? OR requested_by_draft_id=?)`,
		jobID, draftID, draftID,
	).Scan(&state.status, &state.resultJSON, &state.errorJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return previewJobState{}, errors.New("preview.generate job 不存在或不属于当前草稿")
	}
	return state, err
}

func positiveIntValue(value any) (int, bool) {
	number, ok := NumericValue(value)
	if !ok || number <= 0 || math.Trunc(number) != number || number > float64(math.MaxInt) {
		return 0, false
	}
	return int(number), true
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

func renderJobResult(
	kind, jobID, jobStatus, timelineID string,
	timelineVersion int,
	orientation string,
) rushestools.ToolResult {
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
			"render_kind": renderKind, "timeline_id": timelineID,
			"timeline_version": timelineVersion, "orientation": orientation,
		},
	}
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

var delimitedAbsoluteJobPathPattern = regexp.MustCompile(
	`(^|[\s=('"\\[])(/[^\r\n]*?|[A-Za-z]:\\[^\r\n]*?)(: |$)`,
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
	value = delimitedAbsoluteJobPathPattern.ReplaceAllString(value, `${1}<local-path>${3}`)
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
