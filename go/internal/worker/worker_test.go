package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestClaimPriorityHeartbeatRetryAndStaleRecovery(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_jobs", now)
	for _, item := range []struct {
		id       string
		priority int
	}{{"job_slow", 50}, {"job_first", 1}} {
		apply(t, database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_jobs",
			Payload: map[string]any{
				"job_id": item.id, "kind": "noop", "idempotency_key": item.id,
				"priority": item.priority, "max_retries": 3,
				"next_run_at": now.Format(time.RFC3339Nano),
			},
		}}, contracts.ActorSystem, now)
	}
	job, err := Claim(t.Context(), database, "worker_a", now)
	if err != nil || job == nil || job.ID != "job_first" {
		t.Fatalf("claim=%#v err=%v", job, err)
	}
	if job.WorkerID == nil || *job.WorkerID != "worker_a" || job.StartedAt == nil {
		t.Fatalf("claim identity=%#v", job)
	}
	if ok, err := Heartbeat(t.Context(), database, *job, now.Add(5*time.Second)); err != nil || !ok {
		t.Fatalf("heartbeat ok=%v err=%v", ok, err)
	}
	if got := RetryDelay(1); got != time.Second {
		t.Fatalf("retry delay 1=%s", got)
	}
	if got := RetryDelay(20); got != 60*time.Second {
		t.Fatalf("retry delay cap=%s", got)
	}
	if ok, err := ScheduleRetry(t.Context(), database, *job, now, map[string]any{"message": "again"}); err != nil || !ok {
		t.Fatalf("retry ok=%v err=%v", ok, err)
	}
	stored, err := GetJob(t.Context(), database, job.ID)
	if err != nil || stored.Status != "pending" || stored.Attempts != 1 {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status='running', worker_id='dead', heartbeat_at=?, started_at=?
		WHERE job_id=?`, now.Add(-2*time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverStale(t.Context(), database, now, 60*time.Second)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	replacement, err := Claim(t.Context(), database, "worker_b", now.Add(time.Second))
	if err != nil || replacement == nil || replacement.ID != job.ID ||
		replacement.WorkerID == nil || *replacement.WorkerID != "worker_b" || replacement.Attempts != 2 {
		t.Fatalf("replacement=%#v err=%v", replacement, err)
	}
	if ok, err := Heartbeat(t.Context(), database, *job, now.Add(2*time.Second)); err != nil || ok {
		t.Fatalf("stale heartbeat ok=%v err=%v", ok, err)
	}
	if ok, err := ScheduleRetry(t.Context(), database, *job, now.Add(2*time.Second), nil); err != nil || ok {
		t.Fatalf("stale retry ok=%v err=%v", ok, err)
	}
	claimRunner := &Runner{database: database}
	if err := claimRunner.emitTerminal(t.Context(), *job, "JobSucceeded", map[string]any{"stale": true}, nil); !errors.Is(err, reducer.ErrJobClaimLost) {
		t.Fatalf("stale terminal err=%v", err)
	}
	stored, err = GetJob(t.Context(), database, job.ID)
	if err != nil || stored.Status != "running" || !sameClaim(stored, *replacement) {
		t.Fatalf("replacement overwritten stored=%#v err=%v", stored, err)
	}
}

func TestRecoverStaleIncrementsAttemptsAndDeadLettersExhaustedJobs(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_stale_budget", now)
	for _, job := range []struct {
		id         string
		attempts   int
		maxRetries int
	}{
		{id: "job_stale_retry", attempts: 0, maxRetries: 2},
		{id: "job_stale_exhausted", attempts: 1, maxRetries: 1},
		{id: "job_stale_cancelled", attempts: 0, maxRetries: 2},
	} {
		apply(t, database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_stale_budget",
			Payload: map[string]any{
				"job_id": job.id, "kind": "render_preview",
				"requested_by_draft_id": "draft_stale_budget",
				"idempotency_key":       job.id, "attempts": job.attempts,
				"max_retries": job.maxRetries, "next_run_at": now.Format(time.RFC3339Nano),
			},
		}}, contracts.ActorSystem, now)
	}
	apply(t, database, []contracts.Event{{
		Type: "JobCancelled", DraftID: "draft_stale_budget", Payload: map[string]any{
			"job_id": "job_stale_cancelled", "kind": "render_preview",
			"requested_by_draft_id": "draft_stale_budget", "reason": "user_cancelled",
		},
	}}, contracts.ActorUser, now)
	oldHeartbeat := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	startedAt := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status='running', worker_id='dead-worker', started_at=?, heartbeat_at=?
		WHERE job_id IN ('job_stale_retry','job_stale_exhausted')`, startedAt, oldHeartbeat); err != nil {
		t.Fatal(err)
	}

	recovered, err := RecoverStale(t.Context(), database, now, time.Minute)
	if err != nil || recovered != 2 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	retry, err := GetJob(t.Context(), database, "job_stale_retry")
	if err != nil || retry.Status != "pending" || retry.Attempts != 1 ||
		retry.WorkerID != nil || retry.StartedAt != nil || retry.HeartbeatAt != nil {
		t.Fatalf("retry=%#v err=%v", retry, err)
	}
	exhausted, err := GetJob(t.Context(), database, "job_stale_exhausted")
	if err != nil || exhausted.Status != "failed" || exhausted.Attempts != 2 {
		t.Fatalf("exhausted=%#v err=%v", exhausted, err)
	}
	cancelled, err := GetJob(t.Context(), database, "job_stale_cancelled")
	if err != nil || cancelled.Status != "cancelled" || cancelled.Attempts != 0 {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	var failureJSON string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT error_json FROM jobs WHERE job_id='job_stale_exhausted'",
	).Scan(&failureJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failureJSON, `"error_code":"stale_recovery_exhausted"`) ||
		!strings.Contains(failureJSON, `"worker_id":"dead-worker"`) ||
		!strings.Contains(failureJSON, oldHeartbeat) {
		t.Fatalf("failure=%s", failureJSON)
	}
	var failedEvents, exhaustedRunning, retryRunning int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='JobFailed'
		AND json_extract(payload_json,'$.payload.job_id')='job_stale_exhausted'`,
	).Scan(&failedEvents); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			SUM(CASE WHEN json_extract(value,'$.job_id')='job_stale_exhausted' THEN 1 ELSE 0 END),
			SUM(CASE WHEN json_extract(value,'$.job_id')='job_stale_retry' THEN 1 ELSE 0 END)
		FROM drafts, json_each(drafts.running_jobs_json)
		WHERE draft_id='draft_stale_budget'`,
	).Scan(&exhaustedRunning, &retryRunning); err != nil {
		t.Fatal(err)
	}
	if failedEvents != 1 || exhaustedRunning != 0 || retryRunning != 1 {
		t.Fatalf("failed events=%d exhausted running=%d retry running=%d",
			failedEvents, exhaustedRunning, retryRunning)
	}
}

func TestStaleRecoveryFailureIsAppliedOnlyOnce(t *testing.T) {
	t.Parallel()
	database, now, _ := staleRecoveryTerminalFixture(t, "job_stale_once")
	type recoveryResult struct {
		count int64
		err   error
	}
	start := make(chan struct{})
	results := make(chan recoveryResult, 2)
	for range 2 {
		go func() {
			<-start
			count, err := RecoverStale(t.Context(), database, now, time.Minute)
			results <- recoveryResult{count: count, err: err}
		}()
	}
	close(start)
	var recovered int64
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		recovered += result.count
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d", recovered)
	}

	var failedEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='JobFailed'
		AND json_extract(payload_json,'$.payload.job_id')='job_stale_once'`,
	).Scan(&failedEvents); err != nil {
		t.Fatal(err)
	}
	if failedEvents != 1 {
		t.Fatalf("JobFailed events=%d", failedEvents)
	}
}

func TestStaleRecoveryFailureCannotOverwriteSuccess(t *testing.T) {
	t.Parallel()
	database, now, failure := staleRecoveryTerminalFixture(t, "job_stale_after_success")
	claim := failure.Payload
	apply(t, database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: failure.DraftID, Payload: map[string]any{
			"job_id": "job_stale_after_success", "kind": "render_preview",
			"requested_by_draft_id": failure.DraftID,
			"worker_id":             claim["worker_id"], "started_at": claim["started_at"],
			"progress": 1.0, "result": map[string]any{"preview_id": "preview_done"},
		},
	}}, contracts.ActorJob, now)

	if _, err := reducer.Apply(t.Context(), database, []contracts.Event{failure}, reducer.Options{
		Actor: contracts.ActorJob, CreatedAt: now.Add(time.Second),
	}); !errors.Is(err, reducer.ErrJobClaimLost) {
		t.Fatalf("stale recovery after success err=%v", err)
	}
	stored, err := GetJob(t.Context(), database, "job_stale_after_success")
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	var failedEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='JobFailed'
		AND json_extract(payload_json,'$.payload.job_id')='job_stale_after_success'`,
	).Scan(&failedEvents); err != nil {
		t.Fatal(err)
	}
	if failedEvents != 0 {
		t.Fatalf("JobFailed events=%d", failedEvents)
	}
}

func TestRunnerRetriesRenderThenEmitsSingleSuccess(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_runner", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_runner",
		Payload: map[string]any{
			"job_id": "job_retry", "kind": "render_preview", "idempotency_key": "job_retry",
			"max_retries": 2, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	registry := NewRegistry()
	calls := 0
	if err := registry.Register("render_preview", func(
		_ context.Context,
		_ Job,
		_ ProgressReporter,
	) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return map[string]any{"ok": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := now
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "runner",
		HeartbeatInterval: 10 * time.Second, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("first worked=%v err=%v", worked, err)
	}
	stored, _ := GetJob(t.Context(), database, "job_retry")
	if stored.Status != "pending" || stored.Attempts != 1 {
		t.Fatalf("after retry=%#v", stored)
	}
	clock = now.Add(time.Second)
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("second worked=%v err=%v", worked, err)
	}
	stored, _ = GetJob(t.Context(), database, "job_retry")
	if stored.Status != "succeeded" {
		t.Fatalf("after success=%#v", stored)
	}
	var succeeded int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE event_type='JobSucceeded' AND draft_id='draft_runner'`).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 {
		t.Fatalf("JobSucceeded=%d", succeeded)
	}
}

func TestRunnerFailsRenderAfterRetryBudgetWithLastError(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_render_failure", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_render_failure",
		Payload: map[string]any{
			"job_id": "job_render_failure", "kind": "render_final",
			"idempotency_key": "job_render_failure", "max_retries": 2,
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	registry := NewRegistry()
	calls := 0
	if err := registry.Register("render_final", func(
		context.Context, Job, ProgressReporter,
	) (map[string]any, error) {
		calls++
		return nil, fmt.Errorf("render attempt %d failed", calls)
	}); err != nil {
		t.Fatal(err)
	}
	clock := now
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "render-failure-worker",
		HeartbeatInterval: 10 * time.Second, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt, offset := range []time.Duration{0, time.Second, 3 * time.Second} {
		clock = now.Add(offset)
		if worked, runErr := runner.RunOnce(t.Context()); runErr != nil || !worked {
			t.Fatalf("attempt %d worked=%v err=%v", attempt+1, worked, runErr)
		}
	}
	stored, err := GetJob(t.Context(), database, "job_render_failure")
	if err != nil || stored.Status != "failed" || stored.Attempts != 2 || calls != 3 {
		t.Fatalf("stored=%#v calls=%d err=%v", stored, calls, err)
	}
	var failed, succeeded int
	var failureJSON string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			SUM(CASE WHEN event_type='JobFailed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN event_type='JobSucceeded' THEN 1 ELSE 0 END)
		FROM event_log WHERE draft_id='draft_render_failure'`,
	).Scan(&failed, &succeeded); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT error_json FROM jobs WHERE job_id='job_render_failure'",
	).Scan(&failureJSON); err != nil {
		t.Fatal(err)
	}
	if failed != 1 || succeeded != 0 || !strings.Contains(failureJSON, "render attempt 3 failed") {
		t.Fatalf("failed=%d succeeded=%d failure=%s", failed, succeeded, failureJSON)
	}
}

func TestRunnerRecoversHandlerPanicThroughRetryPolicy(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_panic_retry", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_panic_retry", Payload: map[string]any{
			"job_id": "job_panic", "kind": "panic_once", "idempotency_key": "panic_once",
			"max_retries": 1, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	registry := NewRegistry()
	calls := 0
	if err := registry.Register("panic_once", func(
		context.Context, Job, ProgressReporter,
	) (map[string]any, error) {
		calls++
		if calls == 1 {
			panic("transient panic")
		}
		return map[string]any{"recovered": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := now
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "panic-worker",
		HeartbeatInterval: 10 * time.Second, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("panic run worked=%v err=%v", worked, err)
	}
	stored, err := GetJob(t.Context(), database, "job_panic")
	var failure string
	if scanErr := database.Read().QueryRowContext(t.Context(),
		"SELECT error_json FROM jobs WHERE job_id='job_panic'",
	).Scan(&failure); scanErr != nil {
		t.Fatal(scanErr)
	}
	if err != nil || stored.Status != "pending" || stored.Attempts != 1 ||
		!strings.Contains(failure, "transient panic") {
		t.Fatalf("after panic=%#v err=%v", stored, err)
	}
	clock = now.Add(time.Second)
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("retry worked=%v err=%v", worked, err)
	}
	stored, err = GetJob(t.Context(), database, "job_panic")
	if err != nil || stored.Status != "succeeded" || calls != 2 {
		t.Fatalf("after recovery=%#v calls=%d err=%v", stored, calls, err)
	}
}

func TestProgressReporterThrottlesAndForwardsOptionalDetail(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_progress_throttle", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_progress_throttle", Payload: map[string]any{
			"job_id": "job_progress", "kind": "understand", "idempotency_key": "progress",
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	job, err := Claim(t.Context(), database, "progress-worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim=%#v err=%v", job, err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: NewRegistry(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	report := runner.newProgressReporter()
	updates := []ProgressUpdate{
		{Progress: 0},
		{Progress: 0.005},
		{Progress: 0.009},
		{Progress: 0.01, CurrentAssetID: "asset_detail", Done: 1, Total: 5,
			Stage: "view_frames", Detail: "理解素材 2/5：采访.mp4 正在调用 VLM"},
		{Progress: 0.015},
		{Progress: 1},
	}
	for _, update := range updates {
		if err := report(t.Context(), *job, update); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE event_type='JobProgress'
		AND json_extract(payload_json,'$.payload.job_id')='job_progress'`,
	).Scan(&count); err != nil || count != 3 {
		t.Fatalf("progress events=%d want=3 err=%v", count, err)
	}
	var detail, stage, assetID string
	var done, total int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT
			json_extract(payload_json,'$.payload.detail'),
			json_extract(payload_json,'$.payload.stage'),
			json_extract(payload_json,'$.payload.current_asset_id'),
			json_extract(payload_json,'$.payload.done'),
			json_extract(payload_json,'$.payload.total')
		FROM event_log WHERE event_type='JobProgress'
		AND json_extract(payload_json,'$.payload.current_asset_id')='asset_detail'`,
	).Scan(&detail, &stage, &assetID, &done, &total); err != nil ||
		detail != "理解素材 2/5：采访.mp4 正在调用 VLM" || stage != "view_frames" ||
		assetID != "asset_detail" || done != 1 || total != 5 {
		t.Fatalf("detail=%q stage=%q asset=%q done=%d total=%d err=%v",
			detail, stage, assetID, done, total, err)
	}
}

func TestProgressReporterKeepsSameProgressWithNewDetail(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_progress_detail", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_progress_detail", Payload: map[string]any{
			"job_id": "job_progress_detail", "kind": "understand", "idempotency_key": "detail",
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	job, err := Claim(t.Context(), database, "progress-worker", now)
	if err != nil || job == nil {
		t.Fatalf("claim=%#v err=%v", job, err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: NewRegistry(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	report := runner.newProgressReporter()
	if err := report(t.Context(), *job, ProgressUpdate{Progress: 0.55, Detail: "抽取代表帧"}); err != nil {
		t.Fatal(err)
	}
	if err := report(t.Context(), *job, ProgressUpdate{Progress: 0.55, Detail: "调用 VLM"}); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT json_extract(payload_json,'$.payload.detail') FROM event_log
		WHERE event_type='JobProgress' ORDER BY event_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	details := []string{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if !reflect.DeepEqual(details, []string{"抽取代表帧", "调用 VLM"}) {
		t.Fatalf("details=%v", details)
	}
}

func TestRunnerPeriodicallyRecoversJobsThatBecomeStale(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_periodic_recovery", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_periodic_recovery", Payload: map[string]any{
			"job_id": "job_periodic", "kind": "noop", "idempotency_key": "periodic",
			"max_retries": 2, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	if job, err := Claim(t.Context(), database, "dead-worker", now); err != nil || job == nil {
		t.Fatalf("dead claim=%#v err=%v", job, err)
	}
	registry := NewRegistry()
	if err := registry.Register("noop", func(
		context.Context, Job, ProgressReporter,
	) (map[string]any, error) {
		return map[string]any{"recovered": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "replacement-worker",
		Concurrency: 1, PollInterval: 2 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond, HeartbeatTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := GetJob(t.Context(), database, "job_periodic")
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("periodically recovered job=%#v err=%v", stored, err)
	}
}

func TestRunnerRecoversAfterTransientTerminalEventWriteFailure(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_terminal_recovery", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_terminal_recovery", Payload: map[string]any{
			"job_id": "job_terminal", "kind": "noop", "idempotency_key": "terminal",
			"max_retries": 1, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	registry := NewRegistry()
	if err := registry.Register("noop", func(
		context.Context, Job, ProgressReporter,
	) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := now
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "terminal-worker",
		HeartbeatInterval: 10 * time.Second, HeartbeatTimeout: time.Minute,
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		CREATE TRIGGER fail_job_terminal
		BEFORE INSERT ON event_log WHEN NEW.event_type='JobSucceeded'
		BEGIN SELECT RAISE(ABORT,'transient terminal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err == nil || !worked ||
		!strings.Contains(err.Error(), "transient terminal failure") {
		t.Fatalf("first worked=%v err=%v", worked, err)
	}
	stored, err := GetJob(t.Context(), database, "job_terminal")
	if err != nil || stored.Status != "running" {
		t.Fatalf("after transient failure=%#v err=%v", stored, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), "DROP TRIGGER fail_job_terminal"); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Minute)
	if recovered, err := RecoverStale(t.Context(), database, clock, time.Minute); err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("second worked=%v err=%v", worked, err)
	}
	stored, err = GetJob(t.Context(), database, "job_terminal")
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("after terminal recovery=%#v err=%v", stored, err)
	}
}

func TestIngestHandlerProducesProbeThumbnailProxyAndProgress(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := testDatabase(t)
	source := filepath.Join(database.Paths.Temporary, "source.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=red:s=320x240:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_ingest", now)
	apply(t, database, []contracts.Event{
		{
			Type: "AssetImported",
			Payload: map[string]any{
				"asset_id": "asset_video", "job_id": "job_ingest", "storage_mode": "reference",
				"reference_path": source, "kind": "video", "source": "local", "filename": "source.mp4",
				"hash": "reference-hash", "size": 1, "ingest_status": "imported",
			},
		},
		{
			Type: "AssetLinked", DraftID: "draft_ingest",
			Payload: map[string]any{"asset_id": "asset_video"},
		},
		{
			Type: "JobEnqueued", DraftID: "draft_ingest",
			Payload: map[string]any{
				"job_id": "job_ingest", "kind": "ingest", "asset_id": "asset_video",
				"requested_by_draft_id": "draft_ingest", "idempotency_key": "asset_video",
				"next_run_at": now.Format(time.RFC3339Nano),
			},
		},
	}, contracts.ActorUser, now)
	registry := NewRegistry()
	if err := RegisterIngest(registry, database); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "ingest",
		HeartbeatInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), "asset_video")
	if err != nil || asset.IngestStatus != "ready" || asset.Probe == nil ||
		asset.ThumbnailObjectHash == nil || asset.ProxyObjectHash == nil {
		t.Fatalf("asset=%#v err=%v", asset, err)
	}
	var progress, terminal int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE event_type='JobProgress' AND draft_id='draft_ingest'`).Scan(&progress); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE event_type='JobSucceeded' AND draft_id='draft_ingest'`).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if progress < 3 || terminal != 1 {
		t.Fatalf("progress=%d terminal=%d", progress, terminal)
	}
}

func TestRenderWorkerCreatesPreviewWithRenderSnapshot(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := testDatabase(t)
	source := filepath.Join(database.Paths.Temporary, "render-source.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_render", now)
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_render", "job_id": "job_asset", "storage_mode": "reference",
			"reference_path": source, "kind": "video", "source": "local_path",
			"filename": "render-source.mp4", "hash": "hash", "size": 1, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_render", Payload: map[string]any{"asset_id": "asset_render"}},
	}, contracts.ActorUser, now)
	document, err := timeline.ComposeInitial("draft_render", 1, []timeline.Selection{{
		AssetID: "asset_render", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	documentMap, _ := timeline.ToMap(document)
	base := 0
	apply(t, database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: "draft_render", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1, "document_json": documentMap},
	}}, contracts.ActorAgent, now)
	base = 1
	apply(t, database, []contracts.Event{{
		Type: "TimelineValidated", DraftID: "draft_render", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 1, "validation_report": map[string]any{"valid": true}},
	}}, contracts.ActorAgent, now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_render",
		Payload: map[string]any{
			"job_id": "job_render", "kind": "render_preview", "requested_by_draft_id": "draft_render",
			"idempotency_key": "render:1", "job_payload": map[string]any{"timeline_version": 1},
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorAgent, now)
	document.Version = 2
	document.DurationFrames = 15
	document.Tracks[0].Clips[0].TimelineEndFrame = 15
	document.Tracks[0].Clips[0].SourceEndFrame = 15
	documentMap, _ = timeline.ToMap(document)
	if err := database.Read().QueryRowContext(t.Context(), "SELECT state_version FROM drafts WHERE draft_id='draft_render'").Scan(&base); err != nil {
		t.Fatal(err)
	}
	apply(t, database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: "draft_render", BaseVersion: &base,
		Payload: map[string]any{"timeline_version": 2, "document_json": documentMap},
	}}, contracts.ActorAgent, now)
	registry := NewRegistry()
	if err := RegisterRender(registry, database); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "render", HeartbeatInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	var previewID, hash string
	var timelineVersion int
	var width, height int
	var fps, duration float64
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT preview_id,object_hash,timeline_version,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE draft_id='draft_render'`).Scan(&previewID, &hash, &timelineVersion, &width, &height, &fps, &duration); err != nil {
		var status string
		var errorJSON any
		_ = database.Read().QueryRowContext(t.Context(), "SELECT status,error_json FROM jobs WHERE job_id='job_render'").Scan(&status, &errorJSON)
		t.Fatalf("preview query err=%v job status=%s error=%v", err, status, errorJSON)
	}
	if previewID == "" || len(hash) != 64 || timelineVersion != 1 || width != 960 || height != 540 || fps != 30 || duration != 1 {
		t.Fatalf("preview=%s hash=%s version=%d snapshot=%dx%d %.2f %.2f", previewID, hash, timelineVersion, width, height, fps, duration)
	}
	stored, err := GetJob(t.Context(), database, "job_render")
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("job=%#v err=%v", stored, err)
	}
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_render",
		Payload: map[string]any{
			"job_id": "job_render_final", "kind": "render_final", "requested_by_draft_id": "draft_render",
			"idempotency_key": "render-final:1:portrait", "job_payload": map[string]any{"timeline_version": int64(1), "orientation": "portrait"},
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}, contracts.ActorAgent, time.Now().UTC())
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("final worked=%v err=%v", worked, err)
	}
	var exportHash string
	var exportTimelineVersion int
	if err := database.Read().QueryRow("SELECT object_hash,timeline_version FROM exports WHERE draft_id='draft_render'").Scan(&exportHash, &exportTimelineVersion); err != nil {
		t.Fatal(err)
	}
	if exportTimelineVersion != 1 {
		t.Fatalf("export timeline_version=%d want=1", exportTimelineVersion)
	}
	exportPath, err := database.Paths.ObjectPath(exportHash)
	if err != nil {
		t.Fatal(err)
	}
	exportProbe, err := media.ProbeFile(t.Context(), exportPath)
	if err != nil || exportProbe.Width == nil || exportProbe.Height == nil || *exportProbe.Width != 1080 || *exportProbe.Height != 1920 {
		t.Fatalf("portrait export probe=%#v err=%v", exportProbe, err)
	}
}

func TestUnderstandHandlerCompletesSummariesAndInputShapes(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := testDatabase(t)
	source := filepath.Join(database.Paths.Temporary, "understand-worker.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		"color=c=yellow:s=160x120:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_understand_worker", now)
	for _, assetID := range []string{"asset_u1", "asset_u2"} {
		apply(t, database, []contracts.Event{{
			Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "import_" + assetID, "storage_mode": "reference",
				"reference_path": source, "kind": "video", "source": "local_path",
				"filename": assetID + ".mp4", "hash": assetID, "size": 1,
				"probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
			},
		}}, contracts.ActorUser, now)
	}
	registry := NewRegistry()
	if err := RegisterUnderstand(registry, database, nil); err != nil {
		t.Fatal(err)
	}
	handler, err := registry.Require("understand")
	if err != nil {
		t.Fatal(err)
	}
	progress := []float64{}
	progressUpdates := []ProgressUpdate{}
	result, err := handler(t.Context(), Job{
		ID: "understand_job", Payload: map[string]any{
			"asset_ids": []any{"asset_u1", 42, "asset_u2"}, "focus": "人物",
		},
	}, func(_ context.Context, _ Job, update ProgressUpdate) error {
		progress = append(progress, update.Progress)
		progressUpdates = append(progressUpdates, update)
		return nil
	})
	if err != nil || result["status"] != "completed" || len(progress) < 2 || progress[len(progress)-1] != 1 {
		t.Fatalf("result=%#v progress=%v err=%v", result, progress, err)
	}
	if !containsProgressDetail(progressUpdates, "asset_u1", "view_frames", "asset_u1.mp4") {
		t.Fatalf("missing asset progress detail: %#v", progressUpdates)
	}
	retryProgress := []float64{}
	result, err = handler(t.Context(), Job{
		ID: "understand_job", Payload: map[string]any{
			"asset_ids": []any{"asset_u1", "asset_u2"}, "focus": "人物",
		},
	}, func(_ context.Context, _ Job, update ProgressUpdate) error {
		retryProgress = append(retryProgress, update.Progress)
		return nil
	})
	if err != nil || result["status"] != "completed" || len(retryProgress) != 2 || retryProgress[1] != 1 {
		t.Fatalf("retry result=%#v progress=%v err=%v", result, retryProgress, err)
	}
	cacheResult, err := handler(t.Context(), Job{
		ID: "understand_cache_hit", Payload: map[string]any{
			"asset_ids": []string{"asset_u1"}, "focus": "人物",
		},
	}, func(context.Context, Job, ProgressUpdate) error { return nil })
	if err != nil || !reflect.DeepEqual(cacheResult["cache_hit_asset_ids"], []string{"asset_u1"}) ||
		len(cacheResult["analyzed_asset_ids"].([]string)) != 0 {
		t.Fatalf("cache result=%#v err=%v", cacheResult, err)
	}
	for _, assetID := range []string{"asset_u1", "asset_u2"} {
		asset, _ := storage.GetAsset(t.Context(), database.Read(), assetID)
		if asset.UnderstandingStatus != "ready" {
			t.Fatalf("%s status=%s", assetID, asset.UnderstandingStatus)
		}
		var summaries int
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM material_summaries WHERE asset_id=?", assetID,
		).Scan(&summaries); err != nil || summaries != 1 {
			t.Fatalf("%s summaries=%d err=%v", assetID, summaries, err)
		}
	}
	assetID := "asset_u1"
	if _, err := handler(t.Context(), Job{ID: "by_asset", AssetID: &assetID}, func(context.Context, Job, ProgressUpdate) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT version FROM material_summaries WHERE asset_id=? ORDER BY version`, assetID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	versions := []int{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("summary versions=%v", versions)
	}
	if _, err := handler(t.Context(), Job{ID: "missing", Payload: map[string]any{}}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil {
		t.Fatal("missing assets should fail")
	}
	if got := stringSlice([]string{"a", "b"}); len(got) != 2 {
		t.Fatalf("string slice=%v", got)
	}
	if got := stringSlice(1); got != nil {
		t.Fatalf("invalid string slice=%v", got)
	}
	for input, expected := range map[any]int{int(3): 3, int64(4): 4, float64(5.9): 5, "6": 0} {
		if got := understandInt(input); got != expected {
			t.Fatalf("understandInt(%#v)=%d want=%d", input, got, expected)
		}
	}
}

func TestUnderstandRetryAfterProgressFailureIsExactlyOnce(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	fontSource := filepath.Join(database.Paths.Temporary, "understand-retry.otf")
	if err := os.WriteFile(fontSource, []byte("font fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	assetIDs := []string{"asset-retry-1", "asset-retry-2"}
	for _, assetID := range assetIDs {
		apply(t, database, []contracts.Event{{
			Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "import-" + assetID,
				"storage_mode": "reference", "reference_path": fontSource,
				"kind": "font", "source": "local_path", "filename": assetID + ".otf",
				"hash": assetID, "size": 1, "ingest_status": "ready", "usable": true,
			},
		}}, contracts.ActorUser, now)
	}
	registry := NewRegistry()
	if err := RegisterUnderstand(registry, database, nil); err != nil {
		t.Fatal(err)
	}
	handler, err := registry.Require("understand")
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "understand-retry", Payload: map[string]any{
		"asset_ids": assetIDs, "focus": "字体可用性", "depth": "scan",
	}}
	reportErr := errors.New("进度写入失败")
	firstProgress := []float64{}
	firstResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, update ProgressUpdate) error {
		firstProgress = append(firstProgress, update.Progress)
		return reportErr
	})
	if !errors.Is(err, reportErr) || firstResult != nil || len(firstProgress) != 1 ||
		firstProgress[0] <= 0 || firstProgress[0] >= 0.5 {
		t.Fatalf("first result=%#v progress=%v err=%v", firstResult, firstProgress, err)
	}
	for _, assetID := range assetIDs {
		var count int
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM material_summaries WHERE asset_id=?", assetID,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if count != want {
			t.Fatalf("第一次 reporter 失败后 %s summaries=%d want=%d", assetID, count, want)
		}
	}

	retryProgress := []float64{}
	retryResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, update ProgressUpdate) error {
		retryProgress = append(retryProgress, update.Progress)
		return nil
	})
	if err != nil || retryResult["status"] != "completed" ||
		len(retryProgress) < 2 || retryProgress[len(retryProgress)-1] != 1 ||
		!reflect.DeepEqual(retryResult["asset_ids"], assetIDs) ||
		!reflect.DeepEqual(retryResult["analyzed_asset_ids"], assetIDs) ||
		!reflect.DeepEqual(retryResult["cache_hit_asset_ids"], []string{}) {
		t.Fatalf("retry result=%#v progress=%v err=%v", retryResult, retryProgress, err)
	}

	// 同一 job 再次执行必须重放相同有序结果，不能新增摘要或领域事件。
	replayProgress := []float64{}
	replayResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, update ProgressUpdate) error {
		replayProgress = append(replayProgress, update.Progress)
		return nil
	})
	if err != nil || !reflect.DeepEqual(replayProgress, []float64{0.5, 1.0}) ||
		!reflect.DeepEqual(replayResult["analyzed_asset_ids"], assetIDs) {
		t.Fatalf("replay result=%#v progress=%v err=%v", replayResult, replayProgress, err)
	}
	var summaries int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM material_summaries WHERE asset_id IN (?, ?)`, assetIDs[0], assetIDs[1],
	).Scan(&summaries); err != nil || summaries != 2 {
		t.Fatalf("summaries=%d err=%v", summaries, err)
	}
	for _, item := range []struct {
		eventType string
		want      int
	}{
		{eventType: "MaterialUnderstandingStarted", want: 2},
		{eventType: "MaterialUnderstandingCompleted", want: 2},
		{eventType: "MaterialUnderstandingFailed", want: 1},
	} {
		var count int
		if err := database.Read().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM event_log
			WHERE event_type=? AND json_extract(payload_json, '$.payload.job_id')=?`,
			item.eventType, job.ID,
		).Scan(&count); err != nil || count != item.want {
			t.Fatalf("%s count=%d want=%d err=%v", item.eventType, count, item.want, err)
		}
	}
}

func TestUnderstandRunnerRetriesKeepAssetRunningUntilFinalFailure(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	clock := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	const (
		draftID = "draft-understand-runner-retry"
		assetID = "asset-understand-runner-retry"
		jobID   = "job-understand-runner-retry"
	)
	createDraft(t, database, draftID, clock)
	missingSource := filepath.Join(database.Paths.Temporary, "missing-understand-retry.png")
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "import-" + assetID,
			"storage_mode": "reference", "reference_path": missingSource,
			"kind": "image", "source": "local_path", "filename": "missing.png",
			"hash": "missing-understand-retry", "size": 1,
			"ingest_status": "ready", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
		{Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "requested_by_draft_id": draftID,
			"idempotency_key": jobID, "job_payload": map[string]any{"asset_ids": []string{assetID}},
			"max_retries": 2, "next_run_at": clock.Format(time.RFC3339Nano),
		}},
	}, contracts.ActorAgent, clock)
	registry := NewRegistry()
	if err := RegisterUnderstand(registry, database, nil); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "understand-retry-worker",
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		worked, runErr := runner.RunOnce(t.Context())
		if runErr != nil || !worked {
			t.Fatalf("attempt=%d worked=%v err=%v", attempt, worked, runErr)
		}
		job, err := GetJob(t.Context(), database, jobID)
		if err != nil {
			t.Fatal(err)
		}
		asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 2 {
			if job.Status != "pending" || job.Attempts != attempt+1 ||
				asset.UnderstandingStatus != "running" || len(asset.Failure) != 0 {
				t.Fatalf("attempt=%d job=%#v asset_status=%s failure=%#v",
					attempt, job, asset.UnderstandingStatus, asset.Failure)
			}
			clock = clock.Add(time.Duration(attempt+2) * time.Second)
		} else if job.Status != "failed" || job.Attempts != 2 ||
			asset.UnderstandingStatus != "failed" || len(asset.Failure) == 0 {
			t.Fatalf("terminal job=%#v asset_status=%s failure=%#v",
				job, asset.UnderstandingStatus, asset.Failure)
		}
	}
	for _, eventType := range []string{
		"MaterialUnderstandingStarted", "MaterialUnderstandingFailed",
	} {
		rows, err := database.Read().QueryContext(t.Context(), `
			SELECT CAST(json_extract(payload_json, '$.payload.attempt') AS INTEGER)
			FROM event_log WHERE event_type=?
			AND json_extract(payload_json, '$.payload.job_id')=? ORDER BY event_id`, eventType, jobID)
		if err != nil {
			t.Fatal(err)
		}
		attempts := []int{}
		for rows.Next() {
			var attempt int
			if err := rows.Scan(&attempt); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			attempts = append(attempts, attempt)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(attempts, []int{0, 1, 2}) {
			t.Fatalf("%s attempts=%v want=[0 1 2]", eventType, attempts)
		}
	}
}

func TestRunnerLoopRegistryAndTerminalFailureBranches(t *testing.T) {
	t.Parallel()
	if _, err := NewRunner(RunnerConfig{}); err == nil {
		t.Fatal("nil runner config should fail")
	}
	database := testDatabase(t)
	if _, err := NewRunner(RunnerConfig{
		Database: database, Registry: NewRegistry(),
		HeartbeatInterval: time.Minute, HeartbeatTimeout: time.Minute,
	}); err == nil {
		t.Fatal("unsafe heartbeat interval should fail")
	}
	registry := NewRegistry()
	if err := registry.Register("", func(context.Context, Job, ProgressReporter) (map[string]any, error) { return nil, nil }); err == nil {
		t.Fatal("empty kind should fail")
	}
	if err := registry.Register("noop", nil); err == nil {
		t.Fatal("nil handler should fail")
	}
	handler := func(ctx context.Context, job Job, report ProgressReporter) (map[string]any, error) {
		if err := report(ctx, job, Progress(-1)); err != nil {
			return nil, err
		}
		if err := report(ctx, job, Progress(2)); err != nil {
			return nil, err
		}
		time.Sleep(3 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}
	if err := registry.Register("noop", handler); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("noop", handler); err == nil {
		t.Fatal("duplicate handler should fail")
	}
	if _, err := registry.Require("missing"); err == nil {
		t.Fatal("missing handler should fail")
	}
	if kinds := registry.Kinds(); len(kinds) != 1 || kinds[0] != "noop" {
		t.Fatalf("kinds=%v", kinds)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, Concurrency: 1,
		PollInterval: 2 * time.Millisecond, HeartbeatInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_noop", now)
	apply(t, database, []contracts.Event{{Type: "JobEnqueued", DraftID: "draft_noop", Payload: map[string]any{
		"job_id": "job_noop", "kind": "noop", "idempotency_key": "noop",
		"next_run_at": now.Format(time.RFC3339Nano),
	}}}, contracts.ActorSystem, now)
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("noop worked=%v err=%v", worked, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if job, err := Claim(t.Context(), database, "worker", time.Now()); err != nil || job != nil {
		t.Fatalf("empty claim=%#v err=%v", job, err)
	}
	missingWorker, missingStartedAt := "worker", time.Now().UTC().Format(time.RFC3339Nano)
	if ok, err := Heartbeat(t.Context(), database, Job{
		ID: "missing", WorkerID: &missingWorker, StartedAt: &missingStartedAt,
	}, time.Now()); err != nil || ok {
		t.Fatalf("missing heartbeat ok=%v err=%v", ok, err)
	}
	if ok, err := ScheduleRetry(t.Context(), database, Job{ID: "missing"}, time.Now(), nil); err != nil || ok {
		t.Fatalf("missing retry ok=%v err=%v", ok, err)
	}
	if RetryDelay(0) != time.Second {
		t.Fatal("retry delay minimum mismatch")
	}
	if value(nil) != "" {
		t.Fatal("nil pointer value mismatch")
	}
	if failureJSON(context.Canceled)["retryable"] != false {
		t.Fatal("cancel failure should not be retryable")
	}
	if _, err := GetJob(t.Context(), database, "missing"); err != storage.ErrNotFound {
		t.Fatalf("missing job err=%v", err)
	}
	partial := NewRegistry()
	if err := partial.Register("render_preview", handler); err != nil {
		t.Fatal(err)
	}
	if err := RegisterRender(partial, database); err == nil {
		t.Fatal("duplicate render_preview should fail")
	}
}

func TestRunnerFailsUnknownJobWithoutRetry(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_unknown_job", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_unknown_job", Payload: map[string]any{
			"job_id": "job_unknown", "kind": "unknown", "idempotency_key": "unknown",
			"next_run_at": now.Format(time.RFC3339Nano), "max_retries": 0,
		},
	}}, contracts.ActorSystem, now)
	runner, err := NewRunner(RunnerConfig{Database: database, Registry: NewRegistry(), WorkerID: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	job, err := GetJob(t.Context(), database, "job_unknown")
	if err != nil || job.Status != "failed" {
		t.Fatalf("job=%#v err=%v", job, err)
	}
}

func TestRunnerPreservesCancellationAgainstLateProgressAndSuccess(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_cancel_running", now)
	started := make(chan struct{})
	release := make(chan struct{})
	registry := NewRegistry()
	if err := registry.Register("blocking", func(
		ctx context.Context,
		job Job,
		report ProgressReporter,
	) (map[string]any, error) {
		close(started)
		<-release
		if err := report(ctx, job, Progress(0.5)); err != nil {
			return nil, err
		}
		return map[string]any{"late": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_cancel_running", Payload: map[string]any{
			"job_id": "job_cancel_running", "kind": "blocking",
			"requested_by_draft_id": "draft_cancel_running", "idempotency_key": "cancel-running",
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorUser, now)
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "cancel-running",
		HeartbeatInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	type runResult struct {
		worked bool
		err    error
	}
	result := make(chan runResult, 1)
	go func() {
		worked, runErr := runner.RunOnce(t.Context())
		result <- runResult{worked: worked, err: runErr}
	}()
	<-started
	cancelled, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobCancelled", DraftID: "draft_cancel_running", Payload: map[string]any{
			"job_id": "job_cancel_running", "kind": "blocking",
			"requested_by_draft_id": "draft_cancel_running",
		},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || cancelled.Status != reducer.StatusApplied {
		t.Fatalf("cancel result=%#v err=%v", cancelled, err)
	}
	close(release)
	select {
	case finished := <-result:
		if finished.err != nil || !finished.worked {
			t.Fatalf("runner result=%#v", finished)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop after cancellation")
	}
	job, err := GetJob(t.Context(), database, "job_cancel_running")
	if err != nil || job.Status != "cancelled" {
		t.Fatalf("job=%#v err=%v", job, err)
	}
	var lateEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE json_extract(payload_json, '$.job_id')='job_cancel_running'
		AND event_type IN ('JobProgress','JobSucceeded','JobFailed')`,
	).Scan(&lateEvents); err != nil || lateEvents != 0 {
		t.Fatalf("late events=%d err=%v", lateEvents, err)
	}
}

func TestCancelledJobClaimFencesAllWorkerResults(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	const (
		draftID = "draft_claim_fence"
		assetID = "asset_claim_fence"
		jobID   = "job_claim_fence"
	)
	createDraft(t, database, draftID, now)
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "import_claim_fence",
			"storage_mode": "reference", "reference_path": "/tmp/claim-fence.mp4",
			"kind": "video", "source": "local", "filename": "claim-fence.mp4",
			"hash": "claim-fence", "size": 1, "ingest_status": "ready", "usable": true,
		}},
		{Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "requested_by_draft_id": draftID,
			"idempotency_key": jobID, "next_run_at": now.Format(time.RFC3339Nano),
		}},
	}, contracts.ActorUser, now)
	job, err := Claim(t.Context(), database, "claim-fence-worker", now)
	if err != nil || job == nil || job.ID != jobID {
		t.Fatalf("claim=%#v err=%v", job, err)
	}
	if _, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobCancelled", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "reason": "user_cancelled",
			"requested_by_draft_id": draftID,
		},
	}}, reducer.Options{Actor: contracts.ActorUser}); err != nil {
		t.Fatal(err)
	}

	resultRows := reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
		ID: "summary_claim_fence", AssetID: assetID, Status: "ready",
		Summary: map[string]any{"description": "late"},
	}}}
	for _, events := range [][]contracts.Event{
		{{Type: "MaterialUnderstandingCompleted", Payload: map[string]any{
			"asset_id": assetID, "job_id": jobID, "attempt": 0,
			"summary_id": "summary_claim_fence",
		}}},
		{{Type: "PreviewRendered", DraftID: draftID, Payload: map[string]any{
			"artifact_id": "preview_claim_fence", "timeline_version": 1,
			"object_hash": "preview-claim-fence", "object_size": 1,
		}}},
		{{Type: "ExportCompleted", DraftID: draftID, Payload: map[string]any{
			"artifact_id": "export_claim_fence", "timeline_version": 1,
			"object_hash": "export-claim-fence", "object_size": 1,
		}}},
	} {
		rows := reducer.ResultRows{}
		if events[0].Type == "MaterialUnderstandingCompleted" {
			rows = resultRows
		}
		if _, err := reducer.Apply(t.Context(), database, events,
			claimedJobOptions(*job, reducer.Options{ResultRows: rows})); !errors.Is(err, reducer.ErrJobCancelled) {
			t.Fatalf("event=%s err=%v want ErrJobCancelled", events[0].Type, err)
		}
	}

	assertEmpty := func(query string, args ...any) {
		t.Helper()
		var count int
		if err := database.Read().QueryRowContext(t.Context(), query, args...).Scan(&count); err != nil || count != 0 {
			t.Fatalf("late result count=%d err=%v query=%s", count, err, query)
		}
	}
	assertEmpty("SELECT COUNT(*) FROM material_summaries WHERE summary_id='summary_claim_fence'")
	assertEmpty("SELECT COUNT(*) FROM previews WHERE preview_id='preview_claim_fence'")
	assertEmpty("SELECT COUNT(*) FROM exports WHERE export_id='export_claim_fence'")
	assertEmpty(`SELECT COUNT(*) FROM event_log WHERE event_type IN
		('MaterialUnderstandingCompleted','PreviewRendered','ExportCompleted')
		AND (json_extract(payload_json,'$.payload.job_id')=?
			OR json_extract(payload_json,'$.payload.artifact_id') IN
				('preview_claim_fence','export_claim_fence'))`, jobID)
}

func TestRunningJobCancellationStopsHandlerOnHeartbeat(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_heartbeat_cancel", now)
	started := make(chan struct{})
	stopped := make(chan struct{})
	registry := NewRegistry()
	if err := registry.Register("blocking", func(
		ctx context.Context,
		_ Job,
		_ ProgressReporter,
	) (map[string]any, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_heartbeat_cancel", Payload: map[string]any{
			"job_id": "job_heartbeat_cancel", "kind": "blocking",
			"requested_by_draft_id": "draft_heartbeat_cancel",
			"idempotency_key":       "heartbeat-cancel",
			"next_run_at":           now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorUser, now)
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "heartbeat-worker",
		HeartbeatInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, runErr := runner.RunOnce(t.Context())
		finished <- runErr
	}()
	<-started
	if _, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobCancelled", DraftID: "draft_heartbeat_cancel", Payload: map[string]any{
			"job_id": "job_heartbeat_cancel", "kind": "blocking", "reason": "turn_cancelled",
			"requested_by_draft_id": "draft_heartbeat_cancel",
		},
	}}, reducer.Options{Actor: contracts.ActorUser}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("job 取消后 heartbeat 没有停止 handler")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("cancelled runner err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled runner 没有结束")
	}
	job, err := GetJob(t.Context(), database, "job_heartbeat_cancel")
	if err != nil || job.Status != "cancelled" {
		t.Fatalf("job=%#v err=%v", job, err)
	}
}

func TestImageIngestShortcutAndMalformedJobPayload(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := testDatabase(t)
	image := filepath.Join(database.Paths.Temporary, "poster.png")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=purple:s=64x64", "-frames:v", "1", image); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_image", now)
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_image", "job_id": "job_image", "storage_mode": "reference",
			"reference_path": image, "kind": "image", "source": "local", "filename": "poster.png",
			"hash": "image", "size": 1, "ingest_status": "imported",
		}},
		{Type: "AssetLinked", DraftID: "draft_image", Payload: map[string]any{"asset_id": "asset_image"}},
		{Type: "JobEnqueued", DraftID: "draft_image", Payload: map[string]any{
			"job_id": "job_image", "kind": "ingest", "asset_id": "asset_image",
			"requested_by_draft_id": "draft_image", "idempotency_key": "image",
			"next_run_at": now.Format(time.RFC3339Nano),
		}},
	}, contracts.ActorUser, now)
	registry := NewRegistry()
	if err := RegisterIngest(registry, database); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Database: database, Registry: registry, WorkerID: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	asset, _ := storage.GetAsset(t.Context(), database.Read(), "asset_image")
	if asset.IngestStatus != "ready" || asset.ThumbnailObjectHash == nil || asset.ProxyObjectHash != nil {
		t.Fatalf("asset=%#v", asset)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO jobs(job_id,kind,status,idempotency_key,payload_json,attempts,max_retries,next_run_at,priority,created_at)
		VALUES('bad_payload','noop','pending','bad','not-json',0,0,?,100,?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := GetJob(t.Context(), database, "bad_payload"); err == nil {
		t.Fatal("malformed payload should fail")
	}
}

func TestFontIngestSkipsFFprobeAndBecomesReady(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	font := filepath.Join(database.Paths.Temporary, "font.otf")
	if err := os.WriteFile(font, []byte("not a media container"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createDraft(t, database, "draft_font", now)
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_font_ingest", "job_id": "job_font", "storage_mode": "reference",
			"reference_path": font, "kind": "font", "source": "local_path",
			"filename": "font.otf", "hash": "font", "size": 1, "ingest_status": "imported",
		}},
		{Type: "AssetLinked", DraftID: "draft_font", Payload: map[string]any{
			"asset_id": "asset_font_ingest",
		}},
		{Type: "JobEnqueued", DraftID: "draft_font", Payload: map[string]any{
			"job_id": "job_font", "kind": "ingest", "asset_id": "asset_font_ingest",
			"requested_by_draft_id": "draft_font", "idempotency_key": "font-ingest",
			"next_run_at": now.Format(time.RFC3339Nano),
		}},
	}, contracts.ActorUser, now)
	registry := NewRegistry()
	if err := RegisterIngest(registry, database); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{Database: database, Registry: registry, WorkerID: "font"})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), "asset_font_ingest")
	if err != nil || asset.IngestStatus != "ready" || !asset.Usable || asset.Probe == nil ||
		asset.ProxyObjectHash != nil || asset.ThumbnailObjectHash != nil {
		t.Fatalf("asset=%#v err=%v", asset, err)
	}
}

func TestWorkerHandlerFailureSemantics(t *testing.T) {
	database := testDatabase(t)
	now := time.Now().UTC()
	createDraft(t, database, "draft_handler_failures", now)
	missingSource := filepath.Join(database.Paths.Temporary, "missing.mp4")
	fontSource := filepath.Join(database.Paths.Temporary, "font.otf")
	invalidMedia := filepath.Join(database.Paths.Temporary, "invalid.mp4")
	if err := os.WriteFile(fontSource, []byte("font fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidMedia, []byte("not media"), 0o644); err != nil {
		t.Fatal(err)
	}
	apply(t, database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_missing_source", "job_id": "import_missing", "storage_mode": "reference",
			"reference_path": missingSource, "kind": "video", "source": "local_path",
			"filename": "missing.mp4", "hash": "missing", "size": 1, "ingest_status": "ready",
		}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_font", "job_id": "import_font", "storage_mode": "reference",
			"reference_path": fontSource, "kind": "font", "source": "local_path",
			"filename": "font.otf", "hash": "font", "size": 1,
			"probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
		}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_invalid_media", "job_id": "import_invalid", "storage_mode": "reference",
			"reference_path": invalidMedia, "kind": "video", "source": "local_path",
			"filename": "invalid.mp4", "hash": "invalid", "size": 1, "ingest_status": "imported",
		}},
	}, contracts.ActorUser, now)

	understandRegistry := NewRegistry()
	if err := RegisterUnderstand(understandRegistry, database, nil); err != nil {
		t.Fatal(err)
	}
	understand, err := understandRegistry.Require("understand")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := understand(t.Context(), Job{ID: "understand_missing", Payload: map[string]any{
		"asset_ids": []string{"asset_missing_source"},
	}}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil {
		t.Fatal("缺失源文件应触发理解失败")
	}
	var failed int
	if err := database.Read().QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='MaterialUnderstandingFailed'`).Scan(&failed); err != nil || failed != 1 {
		t.Fatalf("failed events=%d err=%v", failed, err)
	}
	reportErr := errors.New("report failed")
	if _, err := understand(t.Context(), Job{ID: "understand_report", Payload: map[string]any{
		"asset_ids": []string{"asset_font"},
	}}, func(context.Context, Job, ProgressUpdate) error { return reportErr }); !errors.Is(err, reportErr) {
		t.Fatalf("understand report err=%v", err)
	}
	duplicate := NewRegistry()
	if err := duplicate.Register("understand", func(context.Context, Job, ProgressReporter) (map[string]any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := RegisterUnderstand(duplicate, database, nil); err == nil {
		t.Fatal("重复 understand handler 应失败")
	}

	ingestRegistry := NewRegistry()
	if err := RegisterIngest(ingestRegistry, database); err != nil {
		t.Fatal(err)
	}
	ingest, err := ingestRegistry.Require("ingest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest(t.Context(), Job{ID: "missing_asset"}, func(context.Context, Job, ProgressUpdate) error { return nil }); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing ingest err=%v", err)
	}
	invalidAssetID := "asset_invalid_media"
	if _, err := ingest(t.Context(), Job{ID: "report_first", AssetID: &invalidAssetID}, func(context.Context, Job, ProgressUpdate) error { return reportErr }); !errors.Is(err, reportErr) {
		t.Fatalf("ingest first report err=%v", err)
	}
	if _, err := ingest(t.Context(), Job{ID: "probe_invalid", AssetID: &invalidAssetID}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil {
		t.Fatal("非法媒体应在 probe 阶段失败")
	}

	if _, err := renderHandler(database, false)(t.Context(), Job{}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil {
		t.Fatal("无 draft_id 的 render job 应失败")
	}
	draftID := "draft_handler_failures"
	if _, err := renderHandler(database, false)(t.Context(), Job{DraftID: &draftID, Payload: map[string]any{}}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil || !strings.Contains(err.Error(), "timeline_version") {
		t.Fatalf("缺少 timeline_version render err=%v", err)
	}
	for _, invalidVersion := range []any{0, -1, 1.5, "1"} {
		if _, err := renderHandler(database, false)(t.Context(), Job{DraftID: &draftID, Payload: map[string]any{"timeline_version": invalidVersion}}, func(context.Context, Job, ProgressUpdate) error { return nil }); err == nil {
			t.Fatalf("非法 timeline_version=%#v 应失败", invalidVersion)
		}
	}
	document := timeline.Empty(draftID, 1)
	documentMap, err := timeline.ToMap(document)
	if err != nil {
		t.Fatal(err)
	}
	base := 0
	apply(t, database, []contracts.Event{{Type: "TimelineVersionCreated", DraftID: draftID, BaseVersion: &base, Payload: map[string]any{
		"timeline_version": 1, "document_json": documentMap,
	}}}, contracts.ActorAgent, now)
	if _, err := renderHandler(database, false)(t.Context(), Job{DraftID: &draftID, Payload: map[string]any{"timeline_version": 1}}, func(context.Context, Job, ProgressUpdate) error { return reportErr }); !errors.Is(err, reportErr) {
		t.Fatalf("render report err=%v", err)
	}
}

func TestUnderstandingProgressMappingCoversEveryAnalyzerStage(t *testing.T) {
	for stage, want := range map[string]float64{
		"audio_probe": 0.1, "scene_detect": 0.15, "scene_verify": 0.35,
		"view_frames": 0.55, "transcribe": 0.8, "emit_summary": 0.95,
		"unknown": 0.5,
	} {
		if got := understandingStageProgress(stage); got != want {
			t.Fatalf("stage=%s progress=%v want=%v", stage, got, want)
		}
	}
	for _, test := range []struct {
		note, wantStage, wantMessage string
	}{
		{"view_frames：抽取代表帧", "view_frames", "抽取代表帧"},
		{"无阶段说明", "analyze", "无阶段说明"},
		{"scene_verify：", "scene_verify", "正在分析"},
	} {
		stage, message := understandingProgressDetail(test.note)
		if stage != test.wantStage || message != test.wantMessage {
			t.Fatalf("note=%q stage=%q message=%q", test.note, stage, message)
		}
	}
}

type errorScanner struct{ err error }

func (scanner errorScanner) Scan(...any) error { return scanner.err }

func TestWorkerDatabaseAndSerializationFailures(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := Claim(t.Context(), database, "closed", now); err == nil {
		t.Fatal("closed claim should fail")
	}
	workerID, startedAt := "worker", now.Format(time.RFC3339Nano)
	if _, err := Heartbeat(t.Context(), database, Job{
		ID: "job", WorkerID: &workerID, StartedAt: &startedAt,
	}, now); err == nil {
		t.Fatal("closed heartbeat should fail")
	}
	if _, err := RecoverStale(t.Context(), database, now, time.Minute); err == nil {
		t.Fatal("closed recovery should fail")
	}
	if _, err := ScheduleRetry(t.Context(), database, Job{ID: "job"}, now, nil); err == nil {
		t.Fatal("closed retry should fail")
	}
	if _, err := scanJob(errorScanner{err: errors.New("scan failed")}); err == nil {
		t.Fatal("scanner failure should propagate")
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: NewRegistry(), HeartbeatInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(t.Context()); err == nil {
		t.Fatal("closed runner should fail recovery")
	}
	if _, err := runner.RunOnce(t.Context()); err == nil {
		t.Fatal("closed RunOnce should fail claim")
	}
	if err := runner.newProgressReporter()(t.Context(), Job{ID: "job"}, Progress(0.5)); err == nil {
		t.Fatal("closed progress should fail")
	}
	if err := runner.emitTerminal(t.Context(), Job{ID: "job"}, "JobFailed", nil, nil); err == nil {
		t.Fatal("closed terminal should fail")
	}
	done := make(chan struct{})
	heartbeatContext, cancelHeartbeat := context.WithCancel(t.Context())
	defer cancelHeartbeat()
	runner.heartbeat(heartbeatContext, cancelHeartbeat, Job{ID: "job"}, done)
	<-done
	func() {
		defer func() {
			if recover() == nil {
				t.Error("不可 JSON 编码的 failure 应 panic")
			}
		}()
		_ = mustJSON(make(chan int))
	}()
}

func testDatabase(t *testing.T) *storage.DB {
	t.Helper()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func staleRecoveryTerminalFixture(
	t *testing.T,
	jobID string,
) (*storage.DB, time.Time, contracts.Event) {
	t.Helper()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	draftID := "draft_" + jobID
	createDraft(t, database, draftID, now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "idempotency_key": jobID,
			"max_retries": 0, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	startedAt := now.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	heartbeatAt := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status='running', worker_id='dead-worker', started_at=?, heartbeat_at=?
		WHERE job_id=?`, startedAt, heartbeatAt, jobID); err != nil {
		t.Fatal(err)
	}
	return database, now, contracts.Event{
		Type: "JobFailed", DraftID: draftID, Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"worker_id": "dead-worker", "started_at": startedAt, "heartbeat_at": heartbeatAt,
			"attempts": 1, "progress": 1.0, "error": map[string]any{
				"error_code": "stale_recovery_exhausted", "message": "stale",
			},
		},
	}
}

func containsProgressDetail(updates []ProgressUpdate, assetID, stage, filename string) bool {
	for _, update := range updates {
		if update.CurrentAssetID == assetID && update.Stage == stage &&
			strings.Contains(update.Detail, filename) {
			return true
		}
	}
	return false
}

func createDraft(t *testing.T, database *storage.DB, draftID string, now time.Time) {
	t.Helper()
	apply(t, database, []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID,
		Payload: map[string]any{"name": draftID},
	}}, contracts.ActorUser, now)
}

func apply(
	t *testing.T,
	database *storage.DB,
	events []contracts.Event,
	actor contracts.Actor,
	now time.Time,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: actor, CreatedAt: now})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("apply status=%s err=%v result=%#v", result.Status, err, result)
	}
}
