package agentexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func TestUnderstandWaitReturnsTerminalTimeoutCancellationAndErrors(t *testing.T) {
	t.Run("terminal_and_invalid", func(t *testing.T) {
		for _, test := range []struct {
			status     string
			wantStatus string
			wantError  string
		}{
			{status: "failed", wantStatus: "failed"},
			{status: "cancelled", wantStatus: "cancelled"},
			{status: "mystery", wantError: "状态无效"},
		} {
			t.Run(test.status, func(t *testing.T) {
				database, exec, request := understandWaitFixture(t, "draft_understand_terminal_"+test.status)
				exec.jobPollInterval = 0
				exec.jobWaitTimeout = 0
				jobID := "job_understand_terminal_" + test.status
				insertUnderstandJobState(t, database, request.Asset.ID, request.Asset.ID, jobID, test.status)
				result, err := exec.waitForUnderstandJob(
					t.Context(), request.Asset.ID, jobID, request,
				)
				if test.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), test.wantError) {
						t.Fatalf("err=%v want contains %q", err, test.wantError)
					}
					return
				}
				if err != nil || result.Status != test.wantStatus || result.JobID != jobID {
					t.Fatalf("result=%#v err=%v", result, err)
				}
			})
		}
	})

	t.Run("missing_job", func(t *testing.T) {
		_, exec, request := understandWaitFixture(t, "draft_understand_missing_job")
		if _, err := exec.waitForUnderstandJob(
			t.Context(), request.Asset.ID, "missing_job", request,
		); err == nil || !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("err=%v", err)
		}
		if _, err := exec.understandJobStatus(
			t.Context(), request.Asset.ID, "missing_job", request.Asset.ID,
		); err == nil || !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("status err=%v", err)
		}
	})

	t.Run("timer_timeout", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_timer")
		insertUnderstandJobState(t, database, request.Asset.ID, request.Asset.ID, "job_understand_timer", "running")
		exec.jobPollInterval = time.Millisecond
		exec.jobWaitTimeout = 5 * time.Millisecond
		result, err := exec.waitForUnderstandJob(
			t.Context(), request.Asset.ID, "job_understand_timer", request,
		)
		if err != nil || result.Status != string(rushestools.StatusTimeout) ||
			result.Data["error_code"] != string(rushestools.ErrCodeToolTimeout) ||
			result.Data["job_status"] != "running" ||
			result.Data["underlying_job_continues"] != true ||
			!strings.Contains(result.UsageNote, "job_status=running") {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("deadline_during_status_read", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_status_deadline")
		insertUnderstandJobState(
			t, database, request.Asset.ID, request.Asset.ID,
			"job_understand_status_deadline", "pending",
		)
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(context.DeadlineExceeded)
		result, err := exec.waitForUnderstandJob(
			ctx, request.Asset.ID, "job_understand_status_deadline", request,
		)
		if err != nil || result.Status != string(rushestools.StatusTimeout) ||
			result.Data["error_code"] != string(rushestools.ErrCodeToolTimeout) ||
			result.Data["job_status"] != "pending" ||
			result.Data["underlying_job_continues"] != true {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("turn_cancelled", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_cancel_wait")
		insertUnderstandJobState(t, database, request.Asset.ID, request.Asset.ID, "job_understand_cancel_wait", "pending")
		exec.jobPollInterval = time.Second
		exec.jobWaitTimeout = time.Second
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := exec.waitForUnderstandJob(
				ctx, request.Asset.ID, "job_understand_cancel_wait", request,
			)
			done <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("understand waiter did not observe turn cancellation")
		}
	})
}

func TestExistingUnderstandResultAndMaterialSummaryFallbacks(t *testing.T) {
	database, exec, request := understandWaitFixture(t, "draft_understand_existing_edges")
	if _, err := exec.existingUnderstandResult(
		t.Context(), request.Asset.ID,
		understandJobRef{ID: "job_succeeded_without_summary", Status: "succeeded"}, request,
	); err == nil || !strings.Contains(err.Error(), "缺少持久化摘要") {
		t.Fatalf("missing summary err=%v", err)
	}
	if _, err := exec.existingUnderstandResult(
		t.Context(), request.Asset.ID,
		understandJobRef{ID: "job_invalid", Status: "unexpected"}, request,
	); err == nil || !strings.Contains(err.Error(), "状态无效") {
		t.Fatalf("invalid status err=%v", err)
	}
	if _, err := exec.materialSummaryForUnderstandJob(
		t.Context(), "job_missing", request.Asset.ID, "",
	); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("empty fingerprint err=%v", err)
	}

	fingerprint := "fingerprint_fallback"
	focus := "fixture"
	model := "fixture"
	promptVersion := understanding.PromptVersion
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
			ID: "summary_noncanonical", AssetID: request.Asset.ID, Status: "ready",
			Focus: &focus, Model: &model, Fingerprint: &fingerprint, PromptVersion: &promptVersion,
			Summary: map[string]any{
				"asset_id": request.Asset.ID, "version": 2,
				"overall": "fingerprint fallback", "segments": []any{},
			},
		}}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("summary status=%s err=%v", result.Status, err)
	}
	summary, err := exec.materialSummaryForUnderstandJob(
		t.Context(), "job_without_canonical_summary", request.Asset.ID, fingerprint,
	)
	if err != nil || summary["overall"] != "fingerprint fallback" {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	if got := sampleUnderstandingSegments([]understanding.Segment{{Description: "a"}, {Description: "b"}, {Description: "c"}}, 1); len(got) != 1 || got[0].Description != "b" {
		t.Fatalf("sample=%#v", got)
	}
}

func TestEnqueueUnderstandHandlesDuplicateAndCommitFailure(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_enqueue_duplicate")
		const key = "understand:duplicate"
		insertUnderstandJobStateWithKey(
			t, database, request.Asset.ID, request.Asset.ID, "job_understand_duplicate", "failed", key,
		)
		result, err := exec.enqueueUnderstand(
			t.Context(), request.Asset.ID,
			rushestools.DetectShotsInput{AssetID: request.Asset.ID, Depth: "deep"}, request, key,
		)
		if err != nil || result.Status != "failed" || result.JobID != "job_understand_duplicate" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("commit_failure", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_enqueue_failure")
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER fail_understand_insert BEFORE INSERT ON jobs
			WHEN NEW.kind='understand'
			BEGIN SELECT RAISE(ABORT, 'fixture understand insert failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueueUnderstand(
			t.Context(), request.Asset.ID,
			rushestools.DetectShotsInput{AssetID: request.Asset.ID, Depth: "deep"},
			request, "understand:failure",
		); err == nil || !strings.Contains(err.Error(), "fixture understand insert failure") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestRunUnderstandInlineFailureBoundaries(t *testing.T) {
	t.Run("cancelled_before_start", func(t *testing.T) {
		_, exec, request := understandWaitFixture(t, "draft_understand_inline_cancelled")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := exec.runUnderstandInline(ctx, request.Asset.ID, request); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("cache_claim_without_summary", func(t *testing.T) {
		_, exec, request := understandWaitFixture(t, "draft_understand_inline_missing_cache")
		request.CacheHit = true
		if _, err := exec.runUnderstandInline(t.Context(), request.Asset.ID, request); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("start_commit_failure", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_inline_start_failure")
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER fail_understand_start BEFORE UPDATE OF understanding_status ON assets
			WHEN NEW.understanding_status='running'
			BEGIN SELECT RAISE(ABORT, 'fixture understand start failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.runUnderstandInline(t.Context(), request.Asset.ID, request); err == nil || !strings.Contains(err.Error(), "fixture understand start failure") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("analyzer_failure", func(t *testing.T) {
		_, exec, request := understandWaitFixture(t, "draft_understand_inline_analyzer_failure")
		if _, err := exec.runUnderstandInline(t.Context(), request.Asset.ID, request); err == nil {
			t.Fatal("missing media source should fail analysis")
		}
	})

	t.Run("completion_commit_failure", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_inline_complete_failure")
		if _, err := database.Write().ExecContext(t.Context(), `
			UPDATE assets SET kind='audio', reference_path=?, probe_json='{"duration_sec":1}'
			WHERE asset_id=?`, database.Paths.DB, request.Asset.ID,
		); err != nil {
			t.Fatal(err)
		}
		loaded, err := storage.GetAsset(t.Context(), database.Read(), request.Asset.ID)
		if err != nil {
			t.Fatal(err)
		}
		request.Asset = loaded
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER fail_understand_complete BEFORE UPDATE OF understanding_status ON assets
			WHEN NEW.understanding_status='ready'
			BEGIN SELECT RAISE(ABORT, 'fixture understand complete failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.runUnderstandInline(t.Context(), request.Asset.ID, request); err == nil || !strings.Contains(err.Error(), "fixture understand complete failure") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestUnderstandHelpersSurfaceClosedDatabaseAndAssetPagination(t *testing.T) {
	t.Run("asset_filter_and_page", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_asset_page")
		insertUnderstandAsset(t, database, request.Asset.ID, "asset_page_two", true, "/tmp/missing-two.mp4")
		insertUnderstandAsset(t, database, request.Asset.ID, "asset_page_unusable", false, "/tmp/missing-three.mp4")
		onlyUsable := true
		result, err := exec.ToolListAssets(t.Context(), request.Asset.ID, rushestools.AssetListInput{
			OnlyUsable: &onlyUsable, Limit: 1,
		})
		if err != nil || result.Total != 2 || len(result.Assets) != 1 || result.NextAfter == "" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("closed_database", func(t *testing.T) {
		database, exec, request := understandWaitFixture(t, "draft_understand_closed")
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := exec.findUnderstandJob(t.Context(), "missing"); err == nil {
			t.Fatal("find should surface closed database")
		}
		if _, err := exec.bestUnderstandingSummary(t.Context(), request.Asset); err == nil {
			t.Fatal("best summary should surface closed database")
		}
		if _, err := exec.understandJobStatus(
			t.Context(), request.Asset.ID, "missing", request.Asset.ID,
		); err == nil {
			t.Fatal("status should surface closed database")
		}
	})
}

func understandWaitFixture(
	t *testing.T,
	draftID string,
) (*storage.DB, *Executor, preparedDetectShotsRequest) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	insertUnderstandAsset(t, database, draftID, draftID, true, "/tmp/missing-understand.mp4")
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := storage.GetAsset(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	return database, exec, preparedDetectShotsRequest{
		Asset: asset,
		Options: understanding.AnalyzeOptions{
			Depth: "deep", MaxStepsPerAsset: 3,
		},
		Fingerprint: "fingerprint_" + draftID,
	}
}

func insertUnderstandAsset(
	t *testing.T,
	database *storage.DB,
	draftID, assetID string,
	usable bool,
	referencePath string,
) {
	t.Helper()
	usableValue := 0
	if usable {
		usableValue = 1
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(?, 'reference', ?, 'video', 'local_path', ?, ?, 1, '{}', 'ready', 'none', ?)`,
		assetID, referencePath, assetID+".mp4", "hash_"+assetID, usableValue,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at) VALUES(?,?,?)`,
		draftID, assetID, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
}

func insertUnderstandJobState(
	t *testing.T,
	database *storage.DB,
	draftID, assetID, jobID, status string,
) {
	t.Helper()
	insertUnderstandJobStateWithKey(t, database, draftID, assetID, jobID, status, jobID)
}

func insertUnderstandJobStateWithKey(
	t *testing.T,
	database *storage.DB,
	draftID, assetID, jobID, status, idempotencyKey string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "understand", "requested_by_draft_id": draftID,
			"asset_id": assetID, "idempotency_key": idempotencyKey,
			"job_payload": map[string]any{"asset_id": assetID},
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert understand job status=%s err=%v", result.Status, err)
	}
	if status != "pending" {
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE jobs SET status=? WHERE job_id=?", status, jobID,
		); err != nil {
			t.Fatal(err)
		}
	}
}
