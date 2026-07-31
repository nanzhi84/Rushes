package agentexec

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestUserExportDirectLifecycleAndRetryEdges(t *testing.T) {
	database, exec, document := userExportFixture(t, "draft_user_export_direct", true)

	if _, err := exec.CreateUserExport(t.Context(), document.DraftID, "  ", "portrait"); !errors.Is(err, ErrUserExportTimelineRequired) {
		t.Fatalf("empty timeline err=%v", err)
	}
	if _, err := exec.CreateUserExport(t.Context(), document.DraftID, document.TimelineID, "square"); err == nil {
		t.Fatal("invalid orientation should fail")
	}
	if _, err := exec.CreateUserExport(t.Context(), "missing_draft", "missing_draft:v1", "auto"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft err=%v", err)
	}
	if _, err := exec.CreateUserExport(t.Context(), document.DraftID, document.DraftID+":v99", "auto"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing timeline err=%v", err)
	}

	created, err := exec.CreateUserExport(
		t.Context(), document.DraftID, "  "+document.TimelineID+"  ", " PORTRAIT ",
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.JobID == "" || created.Status != "pending" || created.Orientation != "portrait" ||
		created.TimelineID != document.TimelineID || created.TimelineVersion != 1 || created.MaxRetries != 0 {
		t.Fatalf("created=%#v", created)
	}
	replayed, err := exec.CreateUserExport(
		t.Context(), document.DraftID, document.TimelineID, "portrait",
	)
	if err != nil || replayed.JobID != created.JobID {
		t.Fatalf("idempotent replay=%#v err=%v", replayed, err)
	}
	if _, err := exec.RetryUserExport(t.Context(), document.DraftID, "missing_job"); !errors.Is(err, ErrUserExportNotFound) {
		t.Fatalf("missing retry err=%v", err)
	}
	if _, err := exec.RetryUserExport(t.Context(), document.DraftID, created.JobID); !errors.Is(err, ErrUserExportNotRetryable) {
		t.Fatalf("pending retry err=%v", err)
	}

	failUserExportJob(t, database, document.DraftID, created.JobID)
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs
		SET progress=0.75, attempts=1, started_at='2026-07-31T00:00:00Z',
		    payload_json=json_remove(payload_json, '$.root_idempotency_key')
		WHERE job_id=?`, created.JobID); err != nil {
		t.Fatal(err)
	}
	failed, err := exec.UserExport(t.Context(), document.DraftID, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if !failed.Retryable || failed.Progress != 0.75 || failed.Attempts != 1 ||
		failed.StartedAt == "" || failed.FinishedAt == "" || failed.Failure == nil ||
		failed.Failure.Code != "render_failed" || strings.Contains(failed.Failure.Message, "/private/") {
		t.Fatalf("failed projection=%#v", failed)
	}

	versionTwo, composeErr := agenttest.ComposeTimeline(document.DraftID, 2, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 90, Role: "a_roll",
	}})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if _, err := seedTimelineVersion(exec, t.Context(), document.DraftID, versionTwo, "user_export_v2", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.CreateUserExport(t.Context(), document.DraftID, document.TimelineID, "landscape"); !errors.Is(err, ErrUserExportStaleTimeline) {
		t.Fatalf("stale create err=%v", err)
	}

	retried, err := exec.RetryUserExport(t.Context(), document.DraftID, created.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.JobID == created.JobID || retried.RetryOfJobID != created.JobID ||
		retried.TimelineID != document.TimelineID || retried.TimelineVersion != 1 {
		t.Fatalf("retried=%#v", retried)
	}
	retriedAgain, err := exec.RetryUserExport(t.Context(), document.DraftID, created.JobID)
	if err != nil || retriedAgain.JobID != retried.JobID {
		t.Fatalf("retry replay=%#v err=%v", retriedAgain, err)
	}
	listed, err := exec.ListUserExports(t.Context(), document.DraftID)
	if err != nil || len(listed) != 2 || listed[0].JobID != retried.JobID {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if _, err := exec.ListUserExports(t.Context(), "missing_draft"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft list err=%v", err)
	}
}

func TestUserExportRejectsInvalidTimelineAndMalformedPersistedTargets(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		_, exec, document := userExportFixture(t, "draft_user_export_invalid", false)
		_, err := exec.CreateUserExport(t.Context(), document.DraftID, document.TimelineID, "auto")
		var validationErr *UserExportValidationError
		if !errors.As(err, &validationErr) || validationErr.Error() == "" || validationErr.Report["valid"] != false {
			t.Fatalf("validation err=%#v", err)
		}
		_, err = exec.enqueueUserExport(
			t.Context(), document.DraftID, document, "auto", "root", "invalid", "", true,
		)
		if !errors.As(err, &validationErr) {
			t.Fatalf("direct enqueue validation err=%v", err)
		}
	})

	t.Run("persisted_target", func(t *testing.T) {
		database, exec, document := userExportFixture(t, "draft_user_export_bad_target", true)
		insertUserExportJob(t, database, document.DraftID, "job_missing_timeline", "failed", map[string]any{
			"request_origin": userExportOrigin, "timeline_id": document.DraftID + ":v99",
			"timeline_version": 99, "orientation": "auto",
		})
		if _, err := exec.RetryUserExport(t.Context(), document.DraftID, "job_missing_timeline"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("missing persisted timeline err=%v", err)
		}
		insertUserExportJob(t, database, document.DraftID, "job_version_mismatch", "failed", map[string]any{
			"request_origin": userExportOrigin, "timeline_id": document.TimelineID,
			"timeline_version": 2, "orientation": "auto",
		})
		if _, err := exec.RetryUserExport(t.Context(), document.DraftID, "job_version_mismatch"); err == nil || !strings.Contains(err.Error(), "版本不一致") {
			t.Fatalf("version mismatch err=%v", err)
		}
		insertUserExportJob(t, database, document.DraftID, "job_missing_fixed_target", "failed", map[string]any{
			"request_origin": userExportOrigin, "timeline_version": 1, "orientation": "auto",
		})
		if _, err := exec.UserExport(t.Context(), document.DraftID, "job_missing_fixed_target"); err == nil || !strings.Contains(err.Error(), "缺少固定时间线") {
			t.Fatalf("missing fixed target err=%v", err)
		}
		if _, err := exec.ListUserExports(t.Context(), document.DraftID); err == nil {
			t.Fatal("list should surface malformed persisted export")
		}
	})
}

func TestUserExportTransactionRechecksLeaseTargetAndVersion(t *testing.T) {
	for _, test := range []struct {
		name       string
		triggerSQL string
		want       error
		wantText   string
	}{
		{
			name: "lease_appears_inside_transaction",
			triggerSQL: `CREATE TRIGGER inject_export_lease AFTER INSERT ON jobs
				WHEN NEW.kind='render_final'
				BEGIN
					INSERT INTO agent_edit_leases(
						draft_id,turn_id,lease_token,acquired_at,heartbeat_at,expires_at
					) VALUES(
						NEW.draft_id,'turn_trigger','token_trigger',
						'2099-01-01T00:00:00.000000000Z','2099-01-01T00:00:00.000000000Z',
						'2099-01-01T00:01:00.000000000Z'
					);
				END`,
			want: storage.ErrTimelineLockedByAgent,
		},
		{
			name: "target_changes_inside_transaction",
			triggerSQL: `CREATE TRIGGER stale_export_target AFTER INSERT ON jobs
				WHEN NEW.kind='render_final'
				BEGIN
					UPDATE drafts SET timeline_current_version=timeline_current_version+1
					WHERE draft_id=NEW.draft_id;
				END`,
			want: ErrUserExportStaleTimeline,
		},
		{
			name: "persistent_version_conflict",
			triggerSQL: `CREATE TRIGGER conflict_export_state AFTER UPDATE OF timeline_validated ON drafts
				WHEN NEW.timeline_validated=1
				BEGIN
					UPDATE drafts SET state_version=state_version+1 WHERE draft_id=NEW.draft_id;
				END`,
			want: ErrUserExportStateConflict,
		},
		{
			name: "job_insert_failure",
			triggerSQL: `CREATE TRIGGER fail_export_insert BEFORE INSERT ON jobs
				WHEN NEW.kind='render_final'
				BEGIN SELECT RAISE(ABORT, 'fixture export insert failure'); END`,
			wantText: "fixture export insert failure",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, exec, document := userExportFixture(t, "draft_export_tx_"+test.name, true)
			if _, err := database.Write().ExecContext(t.Context(), test.triggerSQL); err != nil {
				t.Fatal(err)
			}
			_, err := exec.enqueueUserExport(
				t.Context(), document.DraftID, document, "auto", "root", "tx_"+test.name, "", true,
			)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			if test.wantText != "" && (err == nil || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("err=%v want contains %q", err, test.wantText)
			}
			var jobs int
			if queryErr := database.Read().QueryRowContext(t.Context(),
				"SELECT COUNT(*) FROM jobs WHERE kind='render_final'",
			).Scan(&jobs); queryErr != nil || jobs != 0 {
				t.Fatalf("rolled back jobs=%d err=%v", jobs, queryErr)
			}
		})
	}
}

func TestUserExportDatabaseFailuresRemainErrors(t *testing.T) {
	t.Run("missing_jobs_table", func(t *testing.T) {
		database, exec, document := userExportFixture(t, "draft_export_missing_jobs", true)
		if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE jobs"); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.CreateUserExport(t.Context(), document.DraftID, document.TimelineID, "auto"); err == nil {
			t.Fatal("create should surface job lookup failure")
		}
		if _, err := exec.ListUserExports(t.Context(), document.DraftID); err == nil {
			t.Fatal("list should surface job query failure")
		}
		if _, err := exec.enqueueUserExport(
			t.Context(), document.DraftID, document, "auto", "root", "missing_jobs", "", true,
		); err == nil {
			t.Fatal("enqueue should surface job lookup failure")
		}
	})

	t.Run("closed_database", func(t *testing.T) {
		database, exec, document := userExportFixture(t, "draft_export_closed_db", true)
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if err := exec.ensureUserExportUnlocked(t.Context(), document.DraftID, time.Now().UTC()); err == nil {
			t.Fatal("lease lookup should surface closed database")
		}
		if _, err := exec.loadUserExport(t.Context(), document.DraftID, "job_closed"); err == nil {
			t.Fatal("load should surface closed database")
		}
		if err := exec.validateUserExportTimeline(t.Context(), document.DraftID, document); err == nil {
			t.Fatal("validation should surface closed database")
		}
		if _, err := exec.enqueueUserExport(
			t.Context(), document.DraftID, document, "auto", "root", "closed", "", true,
		); err == nil {
			t.Fatal("enqueue should surface closed database")
		}
	})
}

func userExportFixture(
	t *testing.T,
	draftID string,
	valid bool,
) (*storage.DB, *Executor, timeline.Document) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !valid {
		document.DurationFrames = 0
	}
	if _, err := seedTimelineVersion(exec, t.Context(), draftID, document, "user_export_fixture", nil); err != nil {
		t.Fatal(err)
	}
	return database, exec, document
}

func insertUserExportJob(
	t *testing.T,
	database *storage.DB,
	draftID, jobID, status string,
	payload map[string]any,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": userExportJobKind, "requested_by_draft_id": draftID,
			"idempotency_key": jobID, "job_payload": payload,
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano), "max_retries": 0,
		},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert export job status=%s err=%v", result.Status, err)
	}
	if status == "pending" {
		return
	}
	eventType := map[string]string{
		"failed": "JobFailed", "cancelled": "JobCancelled", "succeeded": "JobSucceeded",
	}[status]
	terminalPayload := map[string]any{"job_id": jobID}
	if status == "failed" {
		terminalPayload["error"] = map[string]any{
			"error_code": "render_failed", "message": "fixture", "retryable": true,
		}
	}
	terminal, terminalErr := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: eventType, DraftID: draftID, Payload: terminalPayload,
	}}, reducer.Options{Actor: contracts.ActorJob})
	if terminalErr != nil || terminal.Status != reducer.StatusApplied {
		t.Fatalf("terminal export job status=%s err=%v", terminal.Status, terminalErr)
	}
}

func failUserExportJob(t *testing.T, database *storage.DB, draftID, jobID string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobFailed", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID,
			"error": map[string]any{
				"error_code": "render_failed",
				"message":    "/private/tmp/secret.mp4 failed",
				"retryable":  true,
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fail export status=%s err=%v", result.Status, err)
	}
}
