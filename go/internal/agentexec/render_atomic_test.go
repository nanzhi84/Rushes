package agentexec

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestPreviewEnqueueTargetsOneTimelineAndRetriesFailedJobs(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_enqueue_atomic"
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
	if _, err := seedTimelineVersion(
		exec, t.Context(), draftID, document, "preview_enqueue_fixture", nil,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "square", document.TimelineID,
	); err == nil {
		t.Fatal("unknown orientation should fail")
	}
	first, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "portrait", document.TimelineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "portrait", document.TimelineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := first.Data["job_id"].(string)
	if first.Status != "queued" || jobID == "" ||
		second.Data["job_id"] != jobID ||
		first.Data["timeline_id"] != draftID+":v1" ||
		first.Data["timeline_version"] != 1 ||
		first.Data["render_kind"] != "preview" ||
		first.Data["orientation"] != "portrait" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}

	var jobsBeforeStale int
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM jobs`,
	).Scan(&jobsBeforeStale); err != nil {
		t.Fatal(err)
	}
	stale, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "portrait", draftID+":v0",
	)
	if err != nil {
		t.Fatal(err)
	}
	var jobsAfterStale int
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM jobs`,
	).Scan(&jobsAfterStale); err != nil {
		t.Fatal(err)
	}
	if stale.Status != "failed" ||
		stale.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) ||
		jobsAfterStale != jobsBeforeStale {
		t.Fatalf("stale=%#v jobs=%d->%d", stale, jobsBeforeStale, jobsAfterStale)
	}

	applied, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID,
			"result": map[string]any{
				"artifact_id": "preview_atomic", "timeline_version": 1,
				"orientation": "portrait",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || applied.Status != reducer.StatusApplied {
		t.Fatalf("job terminal status=%s err=%v", applied.Status, err)
	}
	completedAgain, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "portrait", document.TimelineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completedAgain.Status != "succeeded" ||
		completedAgain.Data["job_id"] != jobID ||
		completedAgain.Data["job_status"] != "succeeded" {
		t.Fatalf("completed preview retry=%#v", completedAgain)
	}

	failedStart, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "landscape", document.TimelineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	failedJobID, _ := failedStart.Data["job_id"].(string)
	applied, err = reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobFailed", DraftID: draftID,
		Payload: map[string]any{
			"job_id": failedJobID,
			"error": map[string]any{
				"error_code": "render_failed", "message": "fixture", "retryable": true,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || applied.Status != reducer.StatusApplied {
		t.Fatalf("job failure status=%s err=%v", applied.Status, err)
	}
	retry, err := exec.enqueuePreviewRender(
		t.Context(), draftID, "landscape", document.TimelineID,
	)
	if err != nil {
		t.Fatal(err)
	}
	retryJobID, _ := retry.Data["job_id"].(string)
	if retry.Status != "queued" || retryJobID == "" || retryJobID == failedJobID {
		t.Fatalf("retry=%#v failed_job_id=%s", retry, failedJobID)
	}
	var retryOf string
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT json_extract(payload_json, '$.retry_of_job_id') FROM jobs WHERE job_id=?`,
		retryJobID,
	).Scan(&retryOf); err != nil || retryOf != failedJobID {
		t.Fatalf("retry_of_job_id=%q err=%v", retryOf, err)
	}
}

func TestPreviewGenerateReturnsCancelledRetryConflict(t *testing.T) {
	database, exec, ctx, document := previewRenderFixture(t, "draft_preview_cancelled_retry_conflict")
	baseKey := fmt.Sprintf(
		"render_preview:%s:%d:auto", document.DraftID, document.Version,
	)
	const failedJobID = "job_preview_retry_base_failed"
	const cancelledJobID = "job_preview_retry_conflict_cancelled"
	// 先插入会造成唯一键冲突的 retry，再插入 base job，保证 includeRetries
	// 按最新 rowid 选中 base failed；入队冲突回退随后读到 cancelled retry。
	insertPreviewJobStateWithKey(
		t, database, document.DraftID, cancelledJobID,
		baseKey+":retry:"+failedJobID, "cancelled", nil,
		map[string]any{"error_code": "render_cancelled", "message": "cancelled", "retryable": false},
	)
	insertPreviewJobStateWithKey(
		t, database, document.DraftID, failedJobID, baseKey, "failed", nil, nil,
	)

	result, err := exec.toolGeneratePreview(ctx, document.DraftID, rushestools.PreviewGenerateInput{
		TimelineID: document.TimelineID, Orientation: "auto",
	})
	failure, _ := result.Data["error"].(map[string]any)
	if err != nil || result.Status != string(rushestools.StatusCancelled) ||
		result.Data["job_id"] != cancelledJobID || result.Data["job_status"] != "cancelled" ||
		failure["error_code"] != "render_cancelled" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestBoundedJobDataDoesNotExposeLocalPathsOrUnboundedErrors(t *testing.T) {
	result := boundedJobResult(map[string]any{
		"artifact_id": "preview_atomic", "timeline_version": 1,
		"orientation": "portrait", "object_hash": "must_not_leak",
		"local_path": "/must/not/leak",
	})
	if result["artifact_id"] != "preview_atomic" || result["timeline_version"] != 1 ||
		result["object_hash"] != nil || result["local_path"] != nil {
		t.Fatalf("unbounded job result=%#v", result)
	}

	posixSecretPath := "/Users/editor/Private Clips/客户素材"
	windowsSecretPath := `C:\Users\editor\Private Clips\客户素材`
	longMessage := "mkdir " + posixSecretPath + ": permission denied; mkdir " +
		windowsSecretPath + ": " + strings.Repeat("错误输出", 300)
	failure := boundedJobFailure(map[string]any{
		"error_code": strings.Repeat("render_", 20),
		"message":    longMessage,
		"retryable":  true,
	})
	failureMessage, _ := failure["message"].(string)
	failureCode, _ := failure["error_code"].(string)
	if strings.Contains(failureMessage, "Private Clips") ||
		strings.Contains(failureMessage, "客户素材") ||
		strings.Contains(failureMessage, `C:\Users`) ||
		strings.Count(failureMessage, "<local-path>") != 2 ||
		utf8.RuneCountInString(failureMessage) > jobFailureMessageRuneLimit ||
		utf8.RuneCountInString(failureCode) > jobFailureCodeRuneLimit ||
		failure["retryable"] != true {
		t.Fatalf("unbounded job failure=%#v", failure)
	}
	if boundedJobFailureText("short", 64) != "short" ||
		boundedJobFailureText("long", 1) != "l" {
		t.Fatal("job failure text bounds mismatch")
	}
}
