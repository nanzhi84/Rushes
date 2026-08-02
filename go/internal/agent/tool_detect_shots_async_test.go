package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestDetectShotsSingleAssetAsyncRouting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		input     rushestools.DetectShotsInput
		wantDepth string
		wantForce bool
	}{
		{
			name: "deep",
			input: rushestools.DetectShotsInput{
				AssetID: "asset_deep", Focus: "  人物   动作  ", Depth: "deep", MaxStepsPerAsset: 11,
			},
			wantDepth: "scan",
		},
		{
			name: "force_refresh",
			input: rushestools.DetectShotsInput{
				AssetID: "asset_force", Depth: "scan", ForceRefresh: true,
			},
			wantDepth: "scan", wantForce: true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			draftID := "draft_detect_" + test.name
			database, service := setupUnderstandRoutingService(t, draftID, test.input.AssetID)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			result, err := executeDetectShots(service, ctx, draftID, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != string(rushestools.StatusTimeout) || result.JobID == "" || result.AssetID != test.input.AssetID {
				t.Fatalf("result=%#v", result)
			}
			job := readUnderstandRoutingJob(t, database, result.JobID)
			if job.Status != "pending" || job.RequestedByDraftID != draftID || job.MaxRetries != 2 {
				t.Fatalf("job=%#v", job)
			}
			if job.Payload["depth"] != test.wantDepth || job.Payload["force_refresh"] != test.wantForce {
				t.Fatalf("payload=%#v", job.Payload)
			}
			if !job.AssetID.Valid || job.AssetID.String != test.input.AssetID ||
				job.Payload["asset_id"] != test.input.AssetID {
				t.Fatalf("内部 job 不是单素材: %#v", job)
			}
			if fingerprint, _ := job.Payload["analysis_fingerprint"].(string); fingerprint == "" {
				t.Fatalf("job 缺少单素材 fingerprint: %#v", job.Payload)
			}
			if job.Payload["focus"] != "" {
				t.Fatalf("基础标注不得被一次查询 focus 污染: %#v", job.Payload)
			}
		})
	}
}

func TestDetectShotsSingleScanEnqueuesAndIsCancelable(t *testing.T) {
	t.Parallel()
	t.Run("enqueued", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_detect_enqueued"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_enqueued")
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		result, err := executeDetectShots(service, ctx, draftID, rushestools.DetectShotsInput{
			AssetID: "asset_enqueued", Depth: "scan", Focus: "主体",
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != string(rushestools.StatusTimeout) || result.JobID == "" {
			t.Fatalf("result=%#v", result)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
			t.Fatalf("single scan jobs=%d want=1", got)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_detect_inline_cancel"
		database, service := setupUnderstandRoutingService(t, draftID, "asset_inline_cancel")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := executeDetectShots(service, ctx, draftID, rushestools.DetectShotsInput{
			AssetID: "asset_inline_cancel", Depth: "scan",
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
			t.Fatalf("cancelled jobs=%d want=0", got)
		}
	})
}

func TestDetectShotsIdempotentEnqueue(t *testing.T) {
	t.Parallel()
	t.Run("concurrent", func(t *testing.T) {
		t.Parallel()
		const draftID = "draft_detect_concurrent"
		const assetID = "asset_concurrent"
		database, service := setupUnderstandRoutingService(t, draftID, assetID)
		input := rushestools.DetectShotsInput{AssetID: assetID, Focus: "并发", Depth: "deep"}
		const callers = 8
		start := make(chan struct{})
		results := make(chan rushestools.DetectShotsResult, callers)
		errs := make(chan error, callers)
		var wait sync.WaitGroup
		for range callers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				// race+cover instrumentation runs all internal packages concurrently;
				// leave enough time for the eight SQLite contenders to converge on one
				// durable job before the same-turn waiter reaches its terminal timeout.
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				result, err := executeDetectShots(service, ctx, draftID, input)
				if err != nil {
					errs <- err
					return
				}
				results <- result
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Errorf("concurrent enqueue: %v", err)
		}
		var jobID string
		for result := range results {
			if result.Status != string(rushestools.StatusTimeout) || result.JobID == "" {
				t.Errorf("result=%#v", result)
			}
			if jobID == "" {
				jobID = result.JobID
			} else if result.JobID != jobID {
				t.Errorf("jobID=%s want=%s", result.JobID, jobID)
			}
		}
		if got := countUnderstandRoutingJobs(t, database, draftID); got != 1 {
			t.Fatalf("concurrent jobs=%d want=1", got)
		}
	})
}

func TestDetectShotsSharesPendingContentJobAcrossDrafts(t *testing.T) {
	t.Parallel()
	database, service := setupUnderstandRoutingService(t, "draft_shared_a", "asset_shared_a")
	agenttest.CreateAgentDraft(t, database, "draft_shared_b")
	addUnderstandRoutingAsset(t, database, "draft_shared_b", "asset_shared_b")
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE assets SET hash='shared-content-hash'
		WHERE asset_id IN ('asset_shared_a','asset_shared_b')`); err != nil {
		t.Fatal(err)
	}

	var sharedJobID string
	for _, fixture := range []struct {
		draftID string
		assetID string
	}{{"draft_shared_a", "asset_shared_a"}, {"draft_shared_b", "asset_shared_b"}} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		result, err := executeDetectShots(service, ctx, fixture.draftID, rushestools.DetectShotsInput{
			AssetID: fixture.assetID,
		})
		cancel()
		if err != nil || result.Status != string(rushestools.StatusTimeout) || result.JobID == "" {
			t.Fatalf("draft=%s result=%#v err=%v", fixture.draftID, result, err)
		}
		if sharedJobID == "" {
			sharedJobID = result.JobID
		} else if result.JobID != sharedJobID {
			t.Fatalf("job=%s want shared job=%s", result.JobID, sharedJobID)
		}
	}
	var jobs int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM jobs WHERE kind='understand'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d want=1 err=%v", jobs, err)
	}
}

func TestDetectShotsRejectsForeignAssetBeforeSideEffect(t *testing.T) {
	t.Parallel()
	const draftID = "draft_detect_invalid_asset"
	database, service := setupUnderstandRoutingService(t, draftID, "asset_valid")
	agenttest.CreateAgentDraft(t, database, "draft_detect_owner")
	addUnderstandRoutingAsset(t, database, "draft_detect_owner", "asset_foreign")
	_, err := executeDetectShots(service, t.Context(), draftID, rushestools.DetectShotsInput{
		AssetID: "asset_foreign", Depth: "scan",
	})
	if err == nil || !strings.Contains(err.Error(), "不属于当前草稿") {
		t.Fatalf("err=%v", err)
	}
	if got := countUnderstandRoutingJobs(t, database, draftID); got != 0 {
		t.Fatalf("invalid request jobs=%d want=0", got)
	}
}

func TestDetectShotsRejectsNonVideoOrUnusableAssetBeforeSideEffect(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		kind    string
		usable  bool
		wantErr string
	}{
		{name: "audio", kind: "audio", usable: true, wantErr: "只接受可用 video 素材"},
		{name: "image", kind: "image", usable: true, wantErr: "只接受可用 video 素材"},
		{name: "unusable_video", kind: "video", usable: false, wantErr: "当前不可用"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			draftID := "draft_detect_reject_" + test.name
			assetID := "asset_reject_" + test.name
			database, service := setupUnderstandRoutingService(t, draftID, "asset_valid_"+test.name)
			addUnderstandRoutingAssetRecord(t, database, draftID, assetID, test.kind, test.usable)
			before := understandSideEffectCounts(t, database)
			_, err := executeDetectShots(service, t.Context(), draftID, rushestools.DetectShotsInput{
				AssetID: assetID, Depth: "scan",
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v want contains %q", err, test.wantErr)
			}
			if after := understandSideEffectCounts(t, database); after != before {
				t.Fatalf("无效目标产生业务写入: before=%v after=%v", before, after)
			}
		})
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
	path := filepath.Join(database.Paths.Temporary, assetID+".mp4")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=64x64:r=5:d=0.4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path,
	); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_import_" + assetID,
			"storage_mode": "reference", "reference_path": path,
			"kind": "video", "source": "local_path", "filename": assetID + ".mp4",
			"hash": "hash_" + assetID, "size": info.Size(), "ingest_status": "ready", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset=%s result=%#v err=%v", assetID, result, err)
	}
}

func addUnderstandRoutingAssetRecord(
	t *testing.T,
	database *storage.DB,
	draftID, assetID, kind string,
	usable bool,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": assetID, "job_id": "job_import_" + assetID,
			"storage_mode":   "reference",
			"reference_path": filepath.Join(database.Paths.Temporary, assetID+"."+kind),
			"kind":           kind, "source": "local_path", "filename": assetID + "." + kind,
			"hash": "hash_" + assetID, "size": 1, "ingest_status": "ready", "usable": usable,
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": assetID}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset=%s result=%#v err=%v", assetID, result, err)
	}
}

func understandSideEffectCounts(t *testing.T, database *storage.DB) [3]int {
	t.Helper()
	var counts [3]int
	for index, table := range []string{"jobs", "material_summaries", "event_log"} {
		if err := database.Read().QueryRowContext(
			t.Context(), "SELECT COUNT(*) FROM "+table,
		).Scan(&counts[index]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
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

func executeDetectShots(
	service *Service,
	ctx context.Context,
	draftID string,
	input rushestools.DetectShotsInput,
) (rushestools.DetectShotsResult, error) {
	raw, err := service.ExecuteTool(rushestools.WithDraftID(ctx, draftID), "media.detect_shots", input)
	if err != nil {
		return rushestools.DetectShotsResult{}, err
	}
	result, ok := raw.(rushestools.DetectShotsResult)
	if !ok {
		return rushestools.DetectShotsResult{}, fmt.Errorf("media.detect_shots 返回类型异常: %T", raw)
	}
	return result, nil
}
