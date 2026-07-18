package agentexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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
) (rushestools.ToolResult, error) {
	orientation, err := normalizeRenderOrientation(orientation)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if draft.TimelineCurrentVersion == nil || !draft.TimelineValidated {
		return rushestools.ToolResult{}, errors.New("当前时间线尚未验证")
	}
	baseIdempotencyKey := fmt.Sprintf("%s:%s:%d:%s", kind, draftID, *draft.TimelineCurrentVersion, orientation)
	idempotencyKey := baseIdempotencyKey
	retryOfJobID := ""
	if existing, found, err := exec.FindRenderJob(ctx, kind, baseIdempotencyKey, true); err != nil {
		return rushestools.ToolResult{}, err
	} else if found {
		if existing.Status != "failed" && existing.Status != "cancelled" {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		retryOfJobID = existing.ID
		idempotencyKey = fmt.Sprintf("%s:retry:%s", baseIdempotencyKey, existing.ID)
	}
	jobID := RandomID("job")
	jobPayload := map[string]any{"timeline_version": *draft.TimelineCurrentVersion, "orientation": orientation}
	if retryOfJobID != "" {
		jobPayload["retry_of_job_id"] = retryOfJobID
	}
	result, err := reducer.Apply(ctx, exec.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": kind, "requested_by_draft_id": draftID,
			"idempotency_key": idempotencyKey,
			"job_payload":     jobPayload,
			"next_run_at":     time.Now().UTC().Format(time.RFC3339Nano),
			"priority":        30,
			"max_retries":     2,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		if existing, found, lookupErr := exec.FindRenderJob(ctx, kind, idempotencyKey, false); lookupErr != nil {
			return rushestools.ToolResult{}, errors.Join(err, lookupErr)
		} else if found {
			return renderJobResult(kind, existing.ID, existing.Status), nil
		}
		return rushestools.ToolResult{}, errors.Join(err, fmt.Errorf("reducer status: %s", result.Status))
	}
	return renderJobResult(kind, jobID, "pending"), nil
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

func (exec *Executor) toolRenderStatus(ctx context.Context, draftID string) (rushestools.ToolResult, error) {
	draft, err := storage.GetDraft(ctx, exec.database.Read(), draftID)
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

func (exec *Executor) ToolInspectPreview(
	ctx context.Context,
	draftID string,
	input rushestools.RenderInspectInput,
) (rushestools.PreviewInspectionResult, error) {
	checks, err := NormalizePreviewInspectionChecks(input.Checks)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	input.Checks = checks
	var hash string
	var timelineVersion int
	var width, height sql.NullInt64
	var fps, duration sql.NullFloat64
	err = exec.database.Read().QueryRowContext(ctx, `
		SELECT object_hash,timeline_version,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE preview_id=? AND draft_id=?`, input.PreviewID, draftID).Scan(
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
	inspection, err := media.InspectVideo(ctx, path, expected, input.Checks)
	if err != nil {
		return rushestools.PreviewInspectionResult{}, err
	}
	result := rushestools.PreviewInspectionResult{}
	if ContainsString(input.Checks, "visual") {
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
	result.Summary = inspection.Summary
	result.Degraded = inspection.Degraded
	result.Issues = issues
	return result, nil
}

func NormalizePreviewInspectionChecks(checks []string) ([]string, error) {
	allowed := map[string]struct{}{
		"decode": {}, "black": {}, "freeze": {}, "silence": {}, "loudness": {}, "visual": {},
	}
	normalized := make([]string, 0, len(checks))
	seen := make(map[string]struct{}, len(checks))
	for _, raw := range checks {
		check := strings.TrimSpace(raw)
		if check == "" {
			return nil, errors.New("checks 不能包含空白检查项")
		}
		if _, ok := allowed[check]; !ok {
			return nil, fmt.Errorf("未知的预览质检项 %q；只支持 decode、black、freeze、silence、loudness 或 visual", check)
		}
		if _, duplicate := seen[check]; duplicate {
			continue
		}
		seen[check] = struct{}{}
		normalized = append(normalized, check)
	}
	return normalized, nil
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
