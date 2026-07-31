package agentexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestPreviewGenerateWaitsForTerminalJobAndReturnsPreviewID(t *testing.T) {
	database, exec, ctx, timelineID := previewGenerateFixture(t, "draft_preview_wait")
	exec.jobPollInterval = time.Millisecond
	exec.jobWaitTimeout = time.Second

	type outcome struct {
		result rushestools.ToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := exec.ExecuteTool(ctx, "preview.generate", rushestools.PreviewGenerateInput{
			TimelineID: timelineID, Orientation: "portrait",
		})
		if err != nil {
			done <- outcome{err: err}
			return
		}
		done <- outcome{result: raw.(rushestools.ToolResult)}
	}()

	jobID := waitForPreviewJobID(t, database, "draft_preview_wait")
	select {
	case result := <-done:
		t.Fatalf("preview.generate 在 job 终态前返回: %#v err=%v", result.result, result.err)
	default:
	}
	terminalPreviewJob(t, database, "draft_preview_wait", jobID, "preview_waited", 1)

	select {
	case got := <-done:
		if got.err != nil || got.result.Status != string(rushestools.StatusSucceeded) ||
			got.result.Data["preview_id"] != "preview_waited" ||
			got.result.Data["job_status"] != "succeeded" ||
			got.result.Data["timeline_id"] != timelineID ||
			got.result.Data["timeline_version"] != 1 ||
			got.result.Data["orientation"] != "portrait" {
			t.Fatalf("preview.generate=%#v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("preview.generate 未在 job 终态后返回")
	}
}

func TestPreviewGenerateTimeoutIsTerminalButLeavesJobReusable(t *testing.T) {
	database, exec, ctx, timelineID := previewGenerateFixture(t, "draft_preview_timeout")
	exec.jobPollInterval = time.Millisecond
	exec.jobWaitTimeout = 20 * time.Millisecond

	raw, err := exec.ExecuteTool(ctx, "preview.generate", rushestools.PreviewGenerateInput{
		TimelineID: timelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	jobID, _ := result.Data["job_id"].(string)
	if result.Status != string(rushestools.StatusTimeout) ||
		result.Data["error_code"] != string(rushestools.ErrCodeToolTimeout) ||
		result.Data["job_status"] != "pending" ||
		result.Data["underlying_job_continues"] != true || jobID == "" {
		t.Fatalf("timeout result=%#v", result)
	}
	terminalPreviewJob(t, database, "draft_preview_timeout", jobID, "preview_late", 1)
	state, err := exec.previewJobState(t.Context(), "draft_preview_timeout", jobID)
	if err != nil || state.status != "succeeded" {
		t.Fatalf("late job status=%q err=%v", state.status, err)
	}
}

func TestPreviewGenerateTurnCancellationStopsWaiterOnly(t *testing.T) {
	database, exec, baseCtx, timelineID := previewGenerateFixture(t, "draft_preview_cancel")
	exec.jobPollInterval = time.Millisecond
	exec.jobWaitTimeout = time.Second
	ctx, cancel := context.WithCancel(baseCtx)
	done := make(chan error, 1)
	go func() {
		_, err := exec.ExecuteTool(ctx, "preview.generate", rushestools.PreviewGenerateInput{
			TimelineID: timelineID,
		})
		done <- err
	}()
	jobID := waitForPreviewJobID(t, database, "draft_preview_cancel")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter 未响应 turn 取消")
	}
	state, err := exec.previewJobState(t.Context(), "draft_preview_cancel", jobID)
	if err != nil || state.status != "pending" {
		t.Fatalf("turn 取消不应取消底层 job: status=%q err=%v", state.status, err)
	}
}

func previewGenerateFixture(
	t *testing.T,
	draftID string,
) (*storage.DB, *Executor, context.Context, string) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(exec, t.Context(), draftID, document, "preview_generate_fixture", nil); err != nil {
		t.Fatal(err)
	}
	return database, exec, rushestools.WithDraftID(t.Context(), draftID), document.TimelineID
}

func waitForPreviewJobID(t *testing.T, database *storage.DB, draftID string) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var jobID string
		err := database.Read().QueryRowContext(t.Context(), `
			SELECT job_id FROM jobs
			WHERE requested_by_draft_id=? AND kind='render_preview'
			ORDER BY rowid DESC LIMIT 1`, draftID).Scan(&jobID)
		if err == nil && jobID != "" {
			return jobID
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("render_preview job 未入队")
	return ""
}

func terminalPreviewJob(
	t *testing.T,
	database *storage.DB,
	draftID, jobID, previewID string,
	timelineVersion int,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID,
			"result": map[string]any{
				"artifact_id": previewID, "timeline_version": timelineVersion,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("job terminal status=%s err=%v", result.Status, err)
	}
}
