package worker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestProductionRegistryMatchesJobKindCatalog(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	registry := NewRegistry()
	if err := RegisterIngest(registry, database); err != nil {
		t.Fatal(err)
	}
	if err := RegisterRender(registry, database); err != nil {
		t.Fatal(err)
	}
	if err := RegisterUnderstand(registry, database, understanding.NewAnalyzer(nil)); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateCatalog(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("fake", func(context.Context, Job, ProgressReporter) (map[string]any, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateCatalog(); err == nil || !strings.Contains(err.Error(), "fake") {
		t.Fatalf("fake handler parity err=%v", err)
	}

	partial := NewRegistry()
	if err := RegisterRender(partial, database); err != nil {
		t.Fatal(err)
	}
	if err := partial.ValidateCatalog(); err == nil || !strings.Contains(err.Error(), "ingest") {
		t.Fatalf("missing spec parity err=%v", err)
	}
}

func TestClaimMatchingSeparatesRenderAndGeneralJobs(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 16, 15, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_claim_lanes", now)
	for _, fixture := range []struct {
		id       string
		kind     string
		priority int
	}{
		{id: "job_render", kind: "render_preview", priority: 1},
		{id: "job_understand", kind: "understand", priority: 2},
	} {
		apply(t, database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_claim_lanes", Payload: map[string]any{
				"job_id": fixture.id, "kind": fixture.kind, "idempotency_key": fixture.id,
				"priority": fixture.priority, "next_run_at": now.Format(time.RFC3339Nano),
			},
		}}, contracts.ActorJob, now)
	}
	renderKinds := contracts.JobKindsByExecutionClass(contracts.JobExecutionRender)
	general, err := ClaimMatching(t.Context(), database, "general", now, ClaimFilter{ExcludeKinds: renderKinds})
	if err != nil || general == nil || general.ID != "job_understand" {
		t.Fatalf("general claim=%#v err=%v", general, err)
	}
	render, err := ClaimMatching(t.Context(), database, "render", now, ClaimFilter{IncludeKinds: renderKinds})
	if err != nil || render == nil || render.ID != "job_render" {
		t.Fatalf("render claim=%#v err=%v", render, err)
	}
	if _, err := ClaimMatching(t.Context(), database, "invalid", now, ClaimFilter{
		IncludeKinds: []string{"render_preview"}, ExcludeKinds: []string{"understand"},
	}); err == nil {
		t.Fatal("include + exclude filter should fail")
	}
}

func TestClaimMatchingReturnsTheJobClaimedByThisCall(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 16, 15, 30, 0, 0, time.UTC)
	createDraft(t, database, "draft_claim_fence", now)
	for _, jobID := range []string{"job_a", "job_b"} {
		apply(t, database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_claim_fence", Payload: map[string]any{
				"job_id": jobID, "kind": "understand", "idempotency_key": jobID,
				"priority": 1, "next_run_at": now.Format(time.RFC3339Nano),
			},
		}}, contracts.ActorJob, now)
	}
	first, err := ClaimMatching(t.Context(), database, "same-worker", now, ClaimFilter{})
	if err != nil || first == nil || first.ID != "job_a" {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	second, err := ClaimMatching(t.Context(), database, "same-worker", now, ClaimFilter{})
	if err != nil || second == nil || second.ID != "job_b" {
		t.Fatalf("second claim=%#v err=%v", second, err)
	}
}

func TestRunnerKeepsGeneralLaneMovingWhileRenderLaneIsBlocked(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_lane_fairness", now)
	for _, fixture := range []struct {
		id       string
		kind     string
		priority int
	}{
		{id: "render_a", kind: "render_preview", priority: 1},
		{id: "render_b", kind: "render_final", priority: 1},
		{id: "understand_a", kind: "understand", priority: 2},
	} {
		apply(t, database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_lane_fairness", Payload: map[string]any{
				"job_id": fixture.id, "kind": fixture.kind, "idempotency_key": fixture.id,
				"priority": fixture.priority, "next_run_at": now.Format(time.RFC3339Nano),
			},
		}}, contracts.ActorJob, now)
	}
	registry := NewRegistry()
	renderStarted := make(chan struct{})
	releaseRender := make(chan struct{})
	var startedOnce sync.Once
	renderHandler := func(context.Context, Job, ProgressReporter) (map[string]any, error) {
		startedOnce.Do(func() { close(renderStarted) })
		<-releaseRender
		return map[string]any{"rendered": true}, nil
	}
	if err := registry.Register("render_preview", renderHandler); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("render_final", renderHandler); err != nil {
		t.Fatal(err)
	}
	understandHandled := make(chan struct{})
	if err := registry.Register("understand", func(context.Context, Job, ProgressReporter) (map[string]any, error) {
		close(understandHandled)
		return map[string]any{"understood": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "lane-worker", Concurrency: 2,
		PollInterval: time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		HeartbeatTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- runner.Run(ctx) }()
	select {
	case <-renderStarted:
	case <-time.After(time.Second):
		t.Fatal("render lane did not start")
	}
	select {
	case <-understandHandled:
	case <-time.After(time.Second):
		t.Fatal("general lane was blocked behind queued renders")
	}
	deadline := time.Now().Add(time.Second)
	for {
		job, getErr := GetJob(t.Context(), database, "understand_a")
		if getErr == nil && job.Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("understand terminal job=%#v err=%v", job, getErr)
		}
		time.Sleep(time.Millisecond)
	}
	var runningRenders, pendingRenders int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			SUM(CASE WHEN status='running' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END)
		FROM jobs WHERE kind IN ('render_preview','render_final')`,
	).Scan(&runningRenders, &pendingRenders); err != nil {
		t.Fatal(err)
	}
	if runningRenders != 1 || pendingRenders != 1 {
		t.Fatalf("render lanes running=%d pending=%d", runningRenders, pendingRenders)
	}
	cancel()
	close(releaseRender)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	released, err := GetJob(t.Context(), database, "render_a")
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "pending" || released.WorkerID != nil || released.StartedAt != nil {
		t.Fatalf("shutdown left render claim=%#v", released)
	}
}

func TestSingleConcurrencyUsesOneUnfilteredLane(t *testing.T) {
	t.Parallel()
	runner := &Runner{workerID: "single", concurrency: 1}
	lanes := runner.claimLanes()
	if len(lanes) != 1 || len(lanes[0].filter.IncludeKinds) != 0 || len(lanes[0].filter.ExcludeKinds) != 0 {
		t.Fatalf("single lane=%#v", lanes)
	}
}
