package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
		replacement.WorkerID == nil || *replacement.WorkerID != "worker_b" {
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

func TestRunnerRetriesThenEmitsTerminalEvent(t *testing.T) {
	t.Parallel()
	database := testDatabase(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	createDraft(t, database, "draft_runner", now)
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_runner",
		Payload: map[string]any{
			"job_id": "job_retry", "kind": "unstable", "idempotency_key": "job_retry",
			"max_retries": 1, "next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, contracts.ActorSystem, now)
	registry := NewRegistry()
	calls := 0
	if err := registry.Register("unstable", func(
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
		HeartbeatInterval: time.Hour, Now: func() time.Time { return clock },
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
		HeartbeatInterval: time.Hour,
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
	registry := NewRegistry()
	if err := RegisterRender(registry, database); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(RunnerConfig{
		Database: database, Registry: registry, WorkerID: "render", HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	var previewID, hash string
	var width, height int
	var fps, duration float64
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT preview_id,object_hash,render_width,render_height,render_fps,expected_duration_sec
		FROM previews WHERE draft_id='draft_render'`).Scan(&previewID, &hash, &width, &height, &fps, &duration); err != nil {
		t.Fatal(err)
	}
	if previewID == "" || len(hash) != 64 || width != 960 || height != 540 || fps != 30 || duration != 1 {
		t.Fatalf("preview=%s hash=%s snapshot=%dx%d %.2f %.2f", previewID, hash, width, height, fps, duration)
	}
	stored, err := GetJob(t.Context(), database, "job_render")
	if err != nil || stored.Status != "succeeded" {
		t.Fatalf("job=%#v err=%v", stored, err)
	}
	apply(t, database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_render",
		Payload: map[string]any{
			"job_id": "job_render_final", "kind": "render_final", "requested_by_draft_id": "draft_render",
			"idempotency_key": "render-final:1", "job_payload": map[string]any{"timeline_version": int64(1)},
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}, contracts.ActorAgent, time.Now().UTC())
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("final worked=%v err=%v", worked, err)
	}
	var exports int
	if err := database.Read().QueryRow("SELECT COUNT(*) FROM exports WHERE draft_id='draft_render'").Scan(&exports); err != nil || exports != 1 {
		t.Fatalf("exports=%d err=%v", exports, err)
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
	result, err := handler(t.Context(), Job{
		ID: "understand_job", Payload: map[string]any{
			"asset_ids": []any{"asset_u1", 42, "asset_u2"}, "focus": "人物",
		},
	}, func(_ context.Context, _ Job, value float64) error {
		progress = append(progress, value)
		return nil
	})
	if err != nil || result["status"] != "completed" || len(progress) != 2 {
		t.Fatalf("result=%#v progress=%v err=%v", result, progress, err)
	}
	retryProgress := []float64{}
	result, err = handler(t.Context(), Job{
		ID: "understand_job", Payload: map[string]any{
			"asset_ids": []any{"asset_u1", "asset_u2"}, "focus": "人物",
		},
	}, func(_ context.Context, _ Job, value float64) error {
		retryProgress = append(retryProgress, value)
		return nil
	})
	if err != nil || result["status"] != "completed" || len(retryProgress) != 2 {
		t.Fatalf("retry result=%#v progress=%v err=%v", result, retryProgress, err)
	}
	cacheResult, err := handler(t.Context(), Job{
		ID: "understand_cache_hit", Payload: map[string]any{
			"asset_ids": []string{"asset_u1"}, "focus": "人物",
		},
	}, func(context.Context, Job, float64) error { return nil })
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
	if _, err := handler(t.Context(), Job{ID: "by_asset", AssetID: &assetID}, func(context.Context, Job, float64) error { return nil }); err != nil {
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
	if _, err := handler(t.Context(), Job{ID: "missing", Payload: map[string]any{}}, func(context.Context, Job, float64) error { return nil }); err == nil {
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
	firstResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, progress float64) error {
		firstProgress = append(firstProgress, progress)
		return reportErr
	})
	if !errors.Is(err, reportErr) || firstResult != nil || !reflect.DeepEqual(firstProgress, []float64{0.5}) {
		t.Fatalf("first result=%#v progress=%v err=%v", firstResult, firstProgress, err)
	}
	for index, assetID := range assetIDs {
		var count int
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM material_summaries WHERE asset_id=?", assetID,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if index == 0 {
			want = 1
		}
		if count != want {
			t.Fatalf("第一次 reporter 失败后 %s summaries=%d want=%d", assetID, count, want)
		}
	}

	retryProgress := []float64{}
	retryResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, progress float64) error {
		retryProgress = append(retryProgress, progress)
		return nil
	})
	if err != nil || retryResult["status"] != "completed" ||
		!reflect.DeepEqual(retryProgress, []float64{0.5, 1.0}) ||
		!reflect.DeepEqual(retryResult["asset_ids"], assetIDs) ||
		!reflect.DeepEqual(retryResult["analyzed_asset_ids"], assetIDs) ||
		!reflect.DeepEqual(retryResult["cache_hit_asset_ids"], []string{}) {
		t.Fatalf("retry result=%#v progress=%v err=%v", retryResult, retryProgress, err)
	}

	// 同一 job 再次执行必须重放相同有序结果，不能新增摘要或领域事件。
	replayProgress := []float64{}
	replayResult, err := handler(t.Context(), job, func(_ context.Context, _ Job, progress float64) error {
		replayProgress = append(replayProgress, progress)
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
		{eventType: "MaterialUnderstandingFailed", want: 0},
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
	registry := NewRegistry()
	if err := registry.Register("", func(context.Context, Job, ProgressReporter) (map[string]any, error) { return nil, nil }); err == nil {
		t.Fatal("empty kind should fail")
	}
	if err := registry.Register("noop", nil); err == nil {
		t.Fatal("nil handler should fail")
	}
	handler := func(ctx context.Context, job Job, report ProgressReporter) (map[string]any, error) {
		if err := report(ctx, job, -1); err != nil {
			return nil, err
		}
		if err := report(ctx, job, 2); err != nil {
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
		if err := report(ctx, job, 0.5); err != nil {
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
		HeartbeatInterval: time.Hour,
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
	}}, func(context.Context, Job, float64) error { return nil }); err == nil {
		t.Fatal("缺失源文件应触发理解失败")
	}
	var failed int
	if err := database.Read().QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='MaterialUnderstandingFailed'`).Scan(&failed); err != nil || failed != 1 {
		t.Fatalf("failed events=%d err=%v", failed, err)
	}
	reportErr := errors.New("report failed")
	if _, err := understand(t.Context(), Job{ID: "understand_report", Payload: map[string]any{
		"asset_ids": []string{"asset_font"},
	}}, func(context.Context, Job, float64) error { return reportErr }); !errors.Is(err, reportErr) {
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
	if _, err := ingest(t.Context(), Job{ID: "missing_asset"}, func(context.Context, Job, float64) error { return nil }); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing ingest err=%v", err)
	}
	invalidAssetID := "asset_invalid_media"
	if _, err := ingest(t.Context(), Job{ID: "report_first", AssetID: &invalidAssetID}, func(context.Context, Job, float64) error { return reportErr }); !errors.Is(err, reportErr) {
		t.Fatalf("ingest first report err=%v", err)
	}
	if _, err := ingest(t.Context(), Job{ID: "probe_invalid", AssetID: &invalidAssetID}, func(context.Context, Job, float64) error { return nil }); err == nil {
		t.Fatal("非法媒体应在 probe 阶段失败")
	}

	if _, err := renderHandler(database, false)(t.Context(), Job{}, func(context.Context, Job, float64) error { return nil }); err == nil {
		t.Fatal("无 draft_id 的 render job 应失败")
	}
	draftID := "draft_handler_failures"
	if _, err := renderHandler(database, false)(t.Context(), Job{DraftID: &draftID, Payload: map[string]any{}}, func(context.Context, Job, float64) error { return nil }); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("无时间线 render err=%v", err)
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
	if _, err := renderHandler(database, false)(t.Context(), Job{DraftID: &draftID, Payload: map[string]any{"timeline_version": 1}}, func(context.Context, Job, float64) error { return reportErr }); !errors.Is(err, reportErr) {
		t.Fatalf("render report err=%v", err)
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
	if err := runner.reportProgress(t.Context(), Job{ID: "job"}, 0.5); err == nil {
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
