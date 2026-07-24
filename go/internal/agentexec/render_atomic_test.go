package agentexec

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestRenderStartTargetsOneTimelineAndJobReadStaysPure(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_render_atomic"
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "fixture", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.PersistTimeline(t.Context(), draftID, document, "render_atomic_fixture"); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	input := rushestools.RenderStartInput{
		Kind: "preview", TimelineID: document.TimelineID, Orientation: "portrait",
	}
	firstRaw, err := exec.ExecuteTool(ctx, "render.start", input)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := exec.ExecuteTool(ctx, "render.start", input)
	if err != nil {
		t.Fatal(err)
	}
	first := firstRaw.(rushestools.ToolResult)
	second := secondRaw.(rushestools.ToolResult)
	jobID, _ := first.Data["job_id"].(string)
	if first.Status != "queued" || jobID == "" ||
		second.Data["job_id"] != jobID ||
		first.Data["timeline_version"] != 1 ||
		first.Data["render_kind"] != "preview" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	var jobsBeforeStale int
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM jobs`).Scan(&jobsBeforeStale); err != nil {
		t.Fatal(err)
	}
	staleRaw, err := exec.ExecuteTool(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "preview", TimelineID: draftID + ":v0",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := staleRaw.(rushestools.ToolResult)
	var jobsAfterStale int
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM jobs`).Scan(&jobsAfterStale); err != nil {
		t.Fatal(err)
	}
	if stale.Status != "failed" ||
		stale.Data["error_code"] != string(rushestools.ErrCodeStaleTarget) ||
		jobsAfterStale != jobsBeforeStale {
		t.Fatalf("stale=%#v jobs=%d->%d", stale, jobsBeforeStale, jobsAfterStale)
	}

	beforeRead := databaseBusinessSnapshot(t, database)
	pendingRaw, err := exec.ExecuteTool(ctx, "job.read", rushestools.JobReadInput{JobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingRaw.(rushestools.ToolResult)
	if pending.Status != "succeeded" || pending.Data["job_status"] != "pending" {
		t.Fatalf("pending=%#v", pending)
	}
	if afterRead := databaseBusinessSnapshot(t, database); afterRead != beforeRead {
		t.Fatal("job.read changed database business state")
	}

	applied, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID,
			"result": map[string]any{
				"artifact_id": "preview_atomic", "timeline_version": 1,
				"orientation": "portrait", "object_hash": "must_not_leak",
				"local_path": "/must/not/leak",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || applied.Status != reducer.StatusApplied {
		t.Fatalf("job terminal status=%s err=%v", applied.Status, err)
	}
	completedRaw, err := exec.ExecuteTool(ctx, "job.read", rushestools.JobReadInput{JobID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	completed := completedRaw.(rushestools.ToolResult)
	result, _ := completed.Data["result"].(map[string]any)
	if completed.Data["job_status"] != "succeeded" ||
		result["artifact_id"] != "preview_atomic" ||
		result["timeline_version"] != float64(1) ||
		result["object_hash"] != nil ||
		result["local_path"] != nil {
		t.Fatalf("completed=%#v", completed)
	}

	failedStartRaw, err := exec.ExecuteTool(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "final", TimelineID: document.TimelineID, Orientation: "portrait",
	})
	if err != nil {
		t.Fatal(err)
	}
	failedJobID, _ := failedStartRaw.(rushestools.ToolResult).Data["job_id"].(string)
	posixSecretPath := "/Users/editor/Private Clips/客户素材"
	windowsSecretPath := `C:\Users\editor\Private Clips\客户素材`
	longMessage := "mkdir " + posixSecretPath + ": permission denied; mkdir " +
		windowsSecretPath + ": " + strings.Repeat("错误输出", 300)
	applied, err = reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobFailed", DraftID: draftID,
		Payload: map[string]any{
			"job_id": failedJobID,
			"error": map[string]any{
				"error_code": strings.Repeat("render_", 20),
				"message":    longMessage, "retryable": true,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || applied.Status != reducer.StatusApplied {
		t.Fatalf("job failure status=%s err=%v", applied.Status, err)
	}
	failedReadRaw, err := exec.ExecuteTool(
		ctx, "job.read", rushestools.JobReadInput{JobID: failedJobID},
	)
	if err != nil {
		t.Fatal(err)
	}
	failure, _ := failedReadRaw.(rushestools.ToolResult).Data["error"].(map[string]any)
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

	const foreignDraftID = "draft_render_atomic_foreign"
	agenttest.CreateAgentDraft(t, database, foreignDraftID)
	foreignRaw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), foreignDraftID),
		"job.read",
		rushestools.JobReadInput{JobID: jobID},
	)
	if err != nil {
		t.Fatal(err)
	}
	foreign := foreignRaw.(rushestools.ToolResult)
	if foreign.Status != "failed" || foreign.Data["job_id"] != jobID {
		t.Fatalf("foreign=%#v", foreign)
	}
}
