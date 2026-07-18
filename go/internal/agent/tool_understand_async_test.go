package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestToolUnderstandRoutesAsyncRequestsToPendingJobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		assetIDs  []string
		input     rushestools.UnderstandInput
		wantDepth string
		wantForce bool
		wantSteps int
	}{
		{
			name: "multi_asset_miss", assetIDs: []string{"asset_multi_a", "asset_multi_b"},
			input: rushestools.UnderstandInput{
				AssetIDs: []string{"asset_multi_a", "asset_multi_b"}, Focus: "  人物   动作  ",
				Depth: "scan", MaxStepsPerAsset: 7,
			},
			wantDepth: "scan", wantSteps: 7,
		},
		{
			name: "single_deep_miss", assetIDs: []string{"asset_deep"},
			input: rushestools.UnderstandInput{
				AssetIDs: []string{"asset_deep"}, Focus: "镜头语义", Depth: "deep", MaxStepsPerAsset: 11,
			},
			wantDepth: "deep", wantSteps: 11,
		},
		{
			name: "single_force_refresh_miss", assetIDs: []string{"asset_force"},
			input: rushestools.UnderstandInput{
				AssetIDs: []string{"asset_force"}, Depth: "scan", ForceRefresh: true,
			},
			wantDepth: "scan", wantForce: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			draftID := "draft_" + test.name
			database, service := setupUnderstandRoutingService(t, draftID, test.assetIDs...)
			result, err := executeUnderstand(service, t.Context(), draftID, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "queued" || result.JobID == "" || !reflect.DeepEqual(result.AssetIDs, test.input.AssetIDs) {
				t.Fatalf("result=%#v", result)
			}
			if !strings.Contains(result.UsageNote, "自动续跑") || !strings.Contains(result.UsageNote, "轮询") {
				t.Fatalf("queued usage note=%q", result.UsageNote)
			}

			job := readUnderstandRoutingJob(t, database, result.JobID)
			if job.Status != "pending" || job.RequestedByDraftID != draftID {
				t.Fatalf("job=%#v", job)
			}
			if job.AssetID.Valid {
				t.Fatalf("understand 聚合任务不应绑定单个 asset_id: %#v", job.AssetID)
			}
			if job.MaxRetries != 2 {
				t.Fatalf("max_retries=%d want=2", job.MaxRetries)
			}
			if job.Payload["depth"] != test.wantDepth || job.Payload["force_refresh"] != test.wantForce {
				t.Fatalf("payload=%#v", job.Payload)
			}
			if got := intFromJSONNumber(t, job.Payload["max_steps_per_asset"]); got != test.wantSteps {
				t.Fatalf("max_steps_per_asset=%d want=%d payload=%#v", got, test.wantSteps, job.Payload)
			}
			if test.name == "multi_asset_miss" && job.Payload["focus"] != "人物 动作" {
				t.Fatalf("focus 未规范化: %#v", job.Payload)
			}
			if got := stringsFromJSONSlice(t, job.Payload["asset_ids"]); !reflect.DeepEqual(got, test.input.AssetIDs) {
				t.Fatalf("payload asset_ids=%v want=%v", got, test.input.AssetIDs)
			}
		})
	}
}

func TestToolUnderstandSingleScanStaysInlineAndCancelable(t *testing.T) {
	t.Parallel()
	t.Run("completed_inline_without_job", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_inline"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_inline")
		result, err := executeUnderstand(service, t.Context(), draftID, rushestools.UnderstandInput{
			AssetIDs: []string{"asset_inline"}, Depth: "scan", Focus: "主体",
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" || len(result.Summaries) != 1 ||
			!reflect.DeepEqual(result.AnalyzedAssetIDs, []string{"asset_inline"}) {
			t.Fatalf("result=%#v", result)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
			t.Fatalf("single scan jobs=%d want=0", got)
		}
		started, completed := countUnderstandingLifecycleEvents(t, database, "asset_inline")
		if started != 1 || completed != 1 {
			t.Fatalf("started=%d completed=%d", started, completed)
		}
	})

	t.Run("cancelled_context_never_falls_back_to_job", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_inline_cancel"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_inline_cancel")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := executeUnderstand(service, ctx, draftID, rushestools.UnderstandInput{
			AssetIDs: []string{"asset_inline_cancel"}, Depth: "scan",
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
			t.Fatalf("cancelled single scan jobs=%d want=0", got)
		}
		started, completed := countUnderstandingLifecycleEvents(t, database, "asset_inline_cancel")
		if started != 0 || completed != 0 {
			t.Fatalf("cancelled lifecycle started=%d completed=%d", started, completed)
		}
	})
}

func TestToolUnderstandCacheRouting(t *testing.T) {
	t.Parallel()
	t.Run("all_cached_multi_asset_completes_without_job", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_all_cached"
		assetIDs := []string{"asset_cached_a", "asset_cached_b"}
		database, service := setupUnderstandRoutingService(t, draftID, assetIDs...)
		input := rushestools.UnderstandInput{AssetIDs: assetIDs, Focus: "人物 动作", Depth: "scan"}
		for _, assetID := range assetIDs {
			cacheUnderstandRoutingSummary(t, database, assetID, input)
		}

		result, err := executeUnderstand(service, t.Context(), draftID, input)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" || result.JobID != "" || len(result.Summaries) != 2 ||
			!reflect.DeepEqual(result.CacheHitAssetIDs, assetIDs) || len(result.AnalyzedAssetIDs) != 0 {
			t.Fatalf("result=%#v", result)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
			t.Fatalf("all cache jobs=%d want=0", got)
		}
	})

	t.Run("partial_cache_queues_whole_batch_and_reports_hits", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_partial_cached"
		assetIDs := []string{"asset_partial_hit", "asset_partial_miss"}
		database, service := setupUnderstandRoutingService(t, draftID, assetIDs...)
		input := rushestools.UnderstandInput{AssetIDs: assetIDs, Focus: "动作", Depth: "scan"}
		cacheUnderstandRoutingSummary(t, database, assetIDs[0], input)

		result, err := executeUnderstand(service, t.Context(), draftID, input)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "queued" || result.JobID == "" ||
			!reflect.DeepEqual(result.CacheHitAssetIDs, []string{assetIDs[0]}) || len(result.Summaries) != 0 {
			t.Fatalf("result=%#v", result)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
			t.Fatalf("partial cache jobs=%d want=1", got)
		}
		job := readUnderstandRoutingJob(t, database, result.JobID)
		if got := stringsFromJSONSlice(t, job.Payload["asset_ids"]); !reflect.DeepEqual(got, assetIDs) {
			t.Fatalf("queued batch=%v want=%v", got, assetIDs)
		}
	})
}

func TestToolUnderstandIdempotencySerialAndConcurrent(t *testing.T) {
	t.Parallel()
	t.Run("serial", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_serial_idempotency"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_serial_a", "asset_serial_b")
		input := rushestools.UnderstandInput{
			AssetIDs: []string{"asset_serial_a", "asset_serial_b"}, Focus: "相同参数", Depth: "scan",
		}
		first, err := executeUnderstand(service, t.Context(), draftID, input)
		if err != nil {
			t.Fatal(err)
		}
		second, err := executeUnderstand(service, t.Context(), draftID, input)
		if err != nil {
			t.Fatal(err)
		}
		if first.JobID == "" || second.JobID != first.JobID || countUnderstandRoutingJobs(t, database, draftID) != 1 {
			t.Fatalf("first=%#v second=%#v", first, second)
		}
	})

	t.Run("concurrent_first_enqueue", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_understand_concurrent_idempotency"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_concurrent_a", "asset_concurrent_b")
		input := rushestools.UnderstandInput{
			AssetIDs: []string{"asset_concurrent_a", "asset_concurrent_b"}, Focus: "并发参数", Depth: "scan",
		}
		const callers = 8
		start := make(chan struct{})
		results := make(chan rushestools.UnderstandResult, callers)
		errorsFound := make(chan error, callers)
		var wait sync.WaitGroup
		for range callers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := executeUnderstand(service, t.Context(), draftID, input)
				if err != nil {
					errorsFound <- err
					return
				}
				results <- result
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errorsFound)
		for err := range errorsFound {
			t.Errorf("concurrent enqueue: %v", err)
		}
		var jobID string
		count := 0
		for result := range results {
			count++
			if result.Status != "queued" || result.JobID == "" {
				t.Errorf("result=%#v", result)
			}
			if jobID == "" {
				jobID = result.JobID
			} else if result.JobID != jobID {
				t.Errorf("jobID=%s want=%s", result.JobID, jobID)
			}
		}
		if count != callers {
			t.Fatalf("successful callers=%d want=%d", count, callers)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
			t.Fatalf("concurrent jobs=%d want=1", got)
		}
	})
}

func TestToolUnderstandRejectsForeignAssetBeforeAnySideEffect(t *testing.T) {
	t.Parallel()
	const draftID = "draft_understand_invalid_asset"
	database, service := setupUnderstandRoutingService(t, draftID, "asset_valid_first")
	agenttest.CreateAgentDraft(t, database, "draft_understand_asset_owner")
	addUnderstandRoutingAsset(t, database, "draft_understand_asset_owner", "asset_foreign")

	_, err := executeUnderstand(service, t.Context(), draftID, rushestools.UnderstandInput{
		AssetIDs: []string{"asset_valid_first", "asset_foreign"}, Depth: "scan",
	})
	if err == nil || !strings.Contains(err.Error(), "不属于当前草稿") {
		t.Fatalf("err=%v", err)
	}
	if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
		t.Fatalf("invalid request jobs=%d want=0", got)
	}
	var summaries int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM material_summaries WHERE asset_id IN ('asset_valid_first','asset_foreign')`).Scan(&summaries); err != nil {
		t.Fatal(err)
	}
	if summaries != 0 {
		t.Fatalf("invalid request summaries=%d want=0", summaries)
	}
	for _, assetID := range []string{"asset_valid_first", "asset_foreign"} {
		started, completed := countUnderstandingLifecycleEvents(t, database, assetID)
		if started != 0 || completed != 0 {
			t.Fatalf("asset=%s started=%d completed=%d", assetID, started, completed)
		}
	}
}

func TestToolUnderstandForceRefreshReusesTerminalJobAndPersistedSummary(t *testing.T) {
	t.Parallel()
	const draftID = "draft_understand_force_terminal"
	database, service := setupUnderstandRoutingService(t, draftID, "asset_force_terminal")
	input := rushestools.UnderstandInput{
		AssetIDs: []string{"asset_force_terminal"}, Depth: "scan", ForceRefresh: true,
	}
	first, err := executeUnderstand(service, t.Context(), draftID, input)
	if err != nil {
		t.Fatal(err)
	}
	cacheUnderstandRoutingSummary(t, database, "asset_force_terminal", input)
	terminal, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": first.JobID, "kind": "understand", "requested_by_draft_id": draftID,
			"result": map[string]any{"status": "completed"},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || terminal.Status != reducer.StatusApplied {
		t.Fatalf("terminal=%#v err=%v", terminal, err)
	}

	const callers = 6
	start := make(chan struct{})
	jobIDs := make(chan string, callers)
	errorsFound := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, callErr := executeUnderstand(service, t.Context(), draftID, input)
			if callErr != nil {
				errorsFound <- callErr
				return
			}
			jobIDs <- result.JobID
		}()
	}
	close(start)
	wait.Wait()
	close(jobIDs)
	close(errorsFound)
	for callErr := range errorsFound {
		t.Errorf("force terminal reuse: %v", callErr)
	}
	for jobID := range jobIDs {
		if jobID != first.JobID {
			t.Errorf("terminal jobID=%q want=%q", jobID, first.JobID)
		}
	}
	serial, err := executeUnderstand(service, t.Context(), draftID, input)
	if err != nil || serial.JobID != first.JobID || serial.Status != "completed" || len(serial.Summaries) != 1 {
		t.Fatalf("serial=%#v err=%v", serial, err)
	}
	if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
		t.Fatalf("force jobs=%d want=1", got)
	}
	newGenerationInput := input
	newGenerationInput.RefreshNonce = "explicit-user-rerun-2"
	newGeneration, err := executeUnderstand(service, t.Context(), draftID, newGenerationInput)
	if err != nil || newGeneration.Status != "queued" || newGeneration.JobID == "" ||
		newGeneration.JobID == first.JobID {
		t.Fatalf("new generation=%#v err=%v", newGeneration, err)
	}
	repeatedGeneration, err := executeUnderstand(service, t.Context(), draftID, newGenerationInput)
	if err != nil || repeatedGeneration.JobID != newGeneration.JobID {
		t.Fatalf("repeated generation=%#v err=%v", repeatedGeneration, err)
	}
	if got := countUnderstandRoutingJobs(t, database, draftID); got != 2 {
		t.Fatalf("force generations=%d want=2", got)
	}
}

func TestToolUnderstandReusesFailedAndCancelledTerminalJobs(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			draftID := "draft_understand_terminal_" + status
			assetID := "asset_understand_terminal_" + status
			database, service := setupUnderstandRoutingService(t, draftID, assetID)
			input := rushestools.UnderstandInput{AssetIDs: []string{assetID}, Depth: "deep"}
			first, err := executeUnderstand(service, t.Context(), draftID, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Write().ExecContext(t.Context(),
				"UPDATE jobs SET status=? WHERE job_id=?", status, first.JobID,
			); err != nil {
				t.Fatal(err)
			}
			repeated, err := executeUnderstand(service, t.Context(), draftID, input)
			if err != nil || repeated.JobID != first.JobID || repeated.Status != status {
				t.Fatalf("repeated=%#v err=%v", repeated, err)
			}
			if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
				t.Fatalf("terminal jobs=%d want=1", got)
			}
			retryInput := input
			retryInput.ForceRefresh = true
			retryInput.RefreshNonce = "explicit-recovery-2"
			retry, err := executeUnderstand(service, t.Context(), draftID, retryInput)
			if err != nil || retry.Status != "queued" || retry.JobID == "" || retry.JobID == first.JobID {
				t.Fatalf("retry=%#v err=%v", retry, err)
			}
		})
	}
}

func TestToolUnderstandReadyCacheSupersedesFailedOrCancelledJob(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			draftID := "draft_understand_terminal_cache_" + status
			assetID := "asset_understand_terminal_cache_" + status
			database, service := setupUnderstandRoutingService(t, draftID, assetID)
			input := rushestools.UnderstandInput{AssetIDs: []string{assetID}, Depth: "deep"}
			first, err := executeUnderstand(service, t.Context(), draftID, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Write().ExecContext(t.Context(),
				"UPDATE jobs SET status=? WHERE job_id=?", status, first.JobID,
			); err != nil {
				t.Fatal(err)
			}
			// 素材摘要是全局缓存；它可能由另一个草稿或任务在历史 job 终态后写入。
			cacheUnderstandRoutingSummary(t, database, assetID, input)
			repeated, err := executeUnderstand(service, t.Context(), draftID, input)
			if err != nil || repeated.Status != "completed" || repeated.JobID != "" ||
				len(repeated.Summaries) != 1 || !reflect.DeepEqual(repeated.CacheHitAssetIDs, []string{assetID}) {
				t.Fatalf("repeated=%#v err=%v", repeated, err)
			}
			if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
				t.Fatalf("cache supersede jobs=%d want=1", got)
			}
		})
	}
}

func TestToolUnderstandAssetOrderIsPartOfIdempotencyKey(t *testing.T) {
	t.Parallel()
	const draftID = "draft_understand_ordered_key"
	database, service := setupUnderstandRoutingService(t, draftID, "asset_order_a", "asset_order_b")
	first, err := executeUnderstand(service, t.Context(), draftID, rushestools.UnderstandInput{
		AssetIDs: []string{"asset_order_a", "asset_order_b"}, Depth: "scan",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := executeUnderstand(service, t.Context(), draftID, rushestools.UnderstandInput{
		AssetIDs: []string{"asset_order_b", "asset_order_a"}, Depth: "scan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID == "" || second.JobID == "" || first.JobID == second.JobID {
		t.Fatalf("ordered jobs first=%#v second=%#v", first, second)
	}
	if got := countUnderstandRoutingJobs(t, database, draftID); got != 2 {
		t.Fatalf("ordered jobs=%d want=2", got)
	}
}

type understandRoutingJob struct {
	Status             string
	RequestedByDraftID string
	AssetID            sql.NullString
	MaxRetries         int
	Payload            map[string]any
}

func setupUnderstandRoutingService(
	t *testing.T,
	draftID string,
	assetIDs ...string,
) (*storage.DB, *Service) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	for _, assetID := range assetIDs {
		addUnderstandRoutingAsset(t, database, draftID, assetID)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return database, service
}

func addUnderstandRoutingAsset(t *testing.T, database *storage.DB, draftID, assetID string) {
	t.Helper()
	path := filepath.Join(database.Paths.Temporary, assetID+".otf")
	content := []byte("font fixture " + assetID)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_import_" + assetID,
			"storage_mode": "reference", "reference_path": path,
			"kind": "font", "source": "local_path", "filename": assetID + ".otf",
			"hash": "hash_" + assetID, "size": len(content), "ingest_status": "ready", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset=%s result=%#v err=%v", assetID, result, err)
	}
}

func cacheUnderstandRoutingSummary(
	t *testing.T,
	database *storage.DB,
	assetID string,
	input rushestools.UnderstandInput,
) {
	t.Helper()
	asset, err := storage.GetAsset(t.Context(), database.Read(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	options := understanding.NormalizeAnalyzeOptions(asset, understanding.AnalyzeOptions{
		Focus: input.Focus, Depth: input.Depth, MaxStepsPerAsset: input.MaxStepsPerAsset,
	})
	fingerprint := understanding.AnalysisFingerprint(asset, options)
	focus := options.Focus
	model := "fixture"
	promptVersion := understanding.PromptVersion
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
			ID: "summary_cached_" + assetID, AssetID: assetID, Status: "ready",
			Focus: &focus, Model: &model, Fingerprint: &fingerprint, PromptVersion: &promptVersion,
			Summary: map[string]any{
				"asset_id": assetID, "version": 2, "focus": focus,
				"semantic_role": "visual", "overall": "缓存摘要 " + assetID,
				"segments": []map[string]any{{
					"start_s": 0, "end_s": 1, "source_start_frame": 0, "source_end_frame": 30,
					"description": "缓存证据 " + assetID, "tags": []string{"font"}, "quality": "usable",
				}},
				"generated_at": "2026-07-15T00:00:00Z", "model": model,
				"analysis_method": "fixture", "analysis_depth": options.Depth, "analysis_steps": 1,
			},
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("cache asset=%s result=%#v err=%v", assetID, result, err)
	}
}

func readUnderstandRoutingJob(t *testing.T, database *storage.DB, jobID string) understandRoutingJob {
	t.Helper()
	var job understandRoutingJob
	var raw string
	err := database.Read().QueryRowContext(t.Context(), `
		SELECT status, requested_by_draft_id, asset_id, max_retries, payload_json
		FROM jobs WHERE job_id=? AND kind='understand'`, jobID,
	).Scan(&job.Status, &job.RequestedByDraftID, &job.AssetID, &job.MaxRetries, &raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &job.Payload); err != nil {
		t.Fatal(err)
	}
	return job
}

func countUnderstandRoutingJobs(t *testing.T, database *storage.DB, draftID string) int {
	t.Helper()
	var count int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE kind='understand' AND requested_by_draft_id=?`, draftID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countUnderstandingLifecycleEvents(t *testing.T, database *storage.DB, assetID string) (int, int) {
	t.Helper()
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT event_type, payload_json FROM event_log
		WHERE event_type IN ('MaterialUnderstandingStarted','MaterialUnderstandingCompleted')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	started, completed := 0, 0
	for rows.Next() {
		var eventType, raw string
		if err := rows.Scan(&eventType, &raw); err != nil {
			t.Fatal(err)
		}
		var event contracts.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if event.Payload["asset_id"] != assetID {
			continue
		}
		if eventType == "MaterialUnderstandingStarted" {
			started++
		} else {
			completed++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return started, completed
}

func stringsFromJSONSlice(t *testing.T, value any) []string {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("value=%#v 不是 JSON 数组", value)
	}
	result := make([]string, 0, len(values))
	for _, item := range values {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("item=%#v 不是字符串", item)
		}
		result = append(result, text)
	}
	return result
}

func intFromJSONNumber(t *testing.T, value any) int {
	t.Helper()
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		t.Fatalf("value=%#v 不是 JSON 数字", value)
		return 0
	}
}

// executeUnderstand 把 understand.materials 经引擎装饰器 ExecuteTool 路由到领域执行器，
// 并把返回类型断言收敛在一处。类型不符折进 error 返回而非 t.Fatal，以兼容并发 goroutine 调用点。
func executeUnderstand(
	service *Service,
	ctx context.Context,
	draftID string,
	input rushestools.UnderstandInput,
) (rushestools.UnderstandResult, error) {
	raw, err := service.ExecuteTool(rushestools.WithDraftID(ctx, draftID), "understand.materials", input)
	if err != nil {
		return rushestools.UnderstandResult{}, err
	}
	result, ok := raw.(rushestools.UnderstandResult)
	if !ok {
		return rushestools.UnderstandResult{}, fmt.Errorf("understand.materials 返回类型异常: %T", raw)
	}
	return result, nil
}
