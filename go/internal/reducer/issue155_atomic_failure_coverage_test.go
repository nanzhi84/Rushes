package reducer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const issue155CreatedAt = "2026-07-31T12:00:00.000000000Z"

func issue155ApplyState(t *testing.T, database *storage.DB) (*applyState, *sql.Tx) {
	t.Helper()
	tx, err := database.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return &applyState{
		tx: tx, createdAt: issue155CreatedAt,
		originalVersions: map[string]int{}, touched: map[string]struct{}{},
	}, tx
}

func requireIssue155ReducerError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || (contains != "" && !strings.Contains(err.Error(), contains)) {
		t.Fatalf("err=%v want substring %q", err, contains)
	}
}

func seedIssue155TimelineRow(t *testing.T, database *storage.DB, draftID string) {
	t.Helper()
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO timeline_versions(
			timeline_id,draft_id,version,document_json,created_at
		) VALUES(?,?,1,'{"version":1}',?);
		UPDATE drafts SET timeline_current_version=1 WHERE draft_id=?`,
		draftID+":v1", draftID, issue155CreatedAt, draftID,
	); err != nil {
		t.Fatal(err)
	}
}

func seedIssue155JobRow(t *testing.T, database *storage.DB, draftID, jobID string) {
	t.Helper()
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO jobs(
			job_id,kind,status,draft_id,requested_by_draft_id,idempotency_key,
			payload_json,next_run_at,created_at
		) VALUES(?,'render_final','running',?,?,?,'{}',?,?)`,
		jobID, draftID, draftID, jobID, issue155CreatedAt, issue155CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestIssue155TimelineReducerRejectsMalformedAndFailedAtomicWrites(t *testing.T) {
	t.Parallel()
	t.Run("missing version", func(t *testing.T) {
		database := openTestDB(t)
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineCreated(t.Context(), state, contracts.Event{
			Type: "TimelineVersionCreated", DraftID: "draft-missing-version",
		}), "缺少 timeline_version")
	})
	t.Run("missing draft query", func(t *testing.T) {
		database := openTestDB(t)
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineCreated(t.Context(), state, contracts.Event{
			Type: "TimelineVersionCreated", DraftID: "draft-does-not-exist",
			Payload: map[string]any{"timeline_version": 1},
		}), "no rows")
	})
	t.Run("timeline insert failure", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-timeline-insert-failure"
		createDraft(t, database, draftID)
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_reject_timeline_insert
			BEFORE INSERT ON timeline_versions
			BEGIN SELECT RAISE(ABORT, 'timeline insert rejected'); END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineCreated(t.Context(), state, contracts.Event{
			Type: "TimelineVersionCreated", DraftID: draftID,
			Payload: map[string]any{"timeline_version": 1},
		}), "timeline insert rejected")
	})
	t.Run("semantic operations require patch id", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-timeline-missing-patch"
		createDraft(t, database, draftID)
		state, tx := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineCreated(t.Context(), state, contracts.Event{
			Type: "TimelineVersionCreated", DraftID: draftID, Actor: contracts.ActorAgent,
			Payload: map[string]any{
				"timeline_version": 1,
				"edit_operations":  []any{map[string]any{"kind": "delete_clip"}},
			},
		}), "编辑日志缺少 patch_id")
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		var versions int
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?", draftID,
		).Scan(&versions); err != nil || versions != 0 {
			t.Fatalf("failed semantic write leaked versions=%d err=%v", versions, err)
		}
	})
	t.Run("edit history insert failure", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-history-insert-failure"
		createDraft(t, database, draftID)
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_reject_history_insert
			BEFORE INSERT ON timeline_edit_batches
			BEGIN SELECT RAISE(ABORT, 'history insert rejected'); END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineCreated(t.Context(), state, contracts.Event{
			Type: "TimelineVersionCreated", DraftID: draftID, Actor: contracts.ActorAgent,
			Payload: map[string]any{
				"timeline_version": 1, "patch_id": "patch-rejected",
				"edit_operations": []any{map[string]any{"kind": "delete_clip"}},
			},
		}), "history insert rejected")
	})

	refs := timelineEditAffectedRefs([]any{[]map[string]any{{
		"timeline_clip_id": "clip-a",
		"nested":           map[string]any{"asset_id": "asset-a"},
	}, {
		"track_id": "visual_base", "parent_block_id": "block-a",
	}}})
	if strings.Join(refs, ",") !=
		"asset_id:asset-a,parent_block_id:block-a,timeline_clip_id:clip-a,track_id:visual_base" {
		t.Fatalf("affected refs=%v", refs)
	}
}

func TestIssue155TimelineValidationPropagatesUpdateAndMissingDraftFailures(t *testing.T) {
	t.Parallel()
	t.Run("validation update failure", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-validation-update-failure"
		createDraft(t, database, draftID)
		seedIssue155TimelineRow(t, database, draftID)
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_reject_timeline_validation
			BEFORE UPDATE OF validation_report_json ON timeline_versions
			BEGIN SELECT RAISE(ABORT, 'timeline validation rejected'); END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineValidation(t.Context(), state, contracts.Event{
			Type: "TimelineValidated", DraftID: draftID,
			Payload: map[string]any{"timeline_version": 1},
		}), "timeline validation rejected")
	})
	t.Run("validation missing draft", func(t *testing.T) {
		database := openTestDB(t)
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyTimelineValidation(t.Context(), state, contracts.Event{
			Type: "TimelineValidationFailed", DraftID: "draft-validation-missing",
			Payload: map[string]any{"timeline_version": 1},
		}), "草稿不存在")
	})
}

func TestIssue155PreviewAndExportArtifactWritesFailAtomically(t *testing.T) {
	t.Parallel()
	for _, artifact := range []struct {
		name      string
		table     string
		apply     func(contextEvent contracts.Event, state *applyState) error
		eventType string
	}{
		{
			name: "preview", table: "previews", eventType: "PreviewRendered",
			apply: func(event contracts.Event, state *applyState) error {
				return applyPreviewRendered(t.Context(), state, event)
			},
		},
		{
			name: "export", table: "exports", eventType: "ExportCompleted",
			apply: func(event contracts.Event, state *applyState) error {
				return applyExportCompleted(t.Context(), state, event)
			},
		},
	} {
		artifact := artifact
		t.Run(artifact.name+" object failure", func(t *testing.T) {
			database := openTestDB(t)
			const draftID = "draft-artifact-object-failure"
			createDraft(t, database, draftID)
			if _, err := database.Write().ExecContext(t.Context(), `
				CREATE TRIGGER issue155_reject_object_insert
				BEFORE INSERT ON objects
				BEGIN SELECT RAISE(ABORT, 'object insert rejected'); END`,
			); err != nil {
				t.Fatal(err)
			}
			state, _ := issue155ApplyState(t, database)
			requireIssue155ReducerError(t, artifact.apply(contracts.Event{
				Type: artifact.eventType, DraftID: draftID,
				Payload: map[string]any{
					"artifact_id": artifact.name + "-object-failure",
					"object_hash": strings.Repeat("a", 64), "timeline_version": 1,
				},
			}, state), "object insert rejected")
		})
		t.Run(artifact.name+" row failure", func(t *testing.T) {
			database := openTestDB(t)
			const draftID = "draft-artifact-row-failure"
			createDraft(t, database, draftID)
			trigger := "CREATE TRIGGER issue155_reject_artifact_insert BEFORE INSERT ON " +
				artifact.table + " BEGIN SELECT RAISE(ABORT, 'artifact insert rejected'); END"
			if _, err := database.Write().ExecContext(t.Context(), trigger); err != nil {
				t.Fatal(err)
			}
			state, tx := issue155ApplyState(t, database)
			requireIssue155ReducerError(t, artifact.apply(contracts.Event{
				Type: artifact.eventType, DraftID: draftID,
				Payload: map[string]any{
					"artifact_id": artifact.name + "-row-failure",
					"object_hash": strings.Repeat("b", 64), "timeline_version": 1,
				},
			}, state), "artifact insert rejected")
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var objects int
			if err := database.Read().QueryRowContext(t.Context(),
				"SELECT COUNT(*) FROM objects WHERE hash=?", strings.Repeat("b", 64),
			).Scan(&objects); err != nil || objects != 0 {
				t.Fatalf("failed artifact leaked object=%d err=%v", objects, err)
			}
		})
		t.Run(artifact.name+" missing draft after insert", func(t *testing.T) {
			database := openTestDB(t)
			const draftID = "draft-artifact-touch-failure"
			createDraft(t, database, draftID)
			trigger := "CREATE TRIGGER issue155_delete_draft_after_artifact AFTER INSERT ON " +
				artifact.table + " BEGIN DELETE FROM drafts WHERE draft_id=NEW.draft_id; END"
			if _, err := database.Write().ExecContext(t.Context(), trigger); err != nil {
				t.Fatal(err)
			}
			state, _ := issue155ApplyState(t, database)
			requireIssue155ReducerError(t, artifact.apply(contracts.Event{
				Type: artifact.eventType, DraftID: draftID,
				Payload: map[string]any{
					"artifact_id": artifact.name + "-touch-failure",
					"object_hash": strings.Repeat("c", 64), "timeline_version": 1,
				},
			}, state), "草稿不存在")
		})
	}
}

func TestIssue155JobTerminalAndRunningProjectionPropagateSQLiteFailures(t *testing.T) {
	t.Parallel()
	t.Run("job terminal update failure", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-job-update-failure"
		createDraft(t, database, draftID)
		seedIssue155JobRow(t, database, draftID, "job-update-failure")
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_reject_job_update BEFORE UPDATE ON jobs
			BEGIN SELECT RAISE(ABORT, 'job update rejected'); END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyJob(t.Context(), state, contracts.Event{
			Type: "JobSucceeded", DraftID: draftID,
			Payload: map[string]any{"job_id": "job-update-failure"},
		}), "job update rejected")
	})
	t.Run("job disappears before artifact reconciliation", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-job-reconcile-failure"
		createDraft(t, database, draftID)
		seedIssue155JobRow(t, database, draftID, "job-reconcile-failure")
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_delete_job_after_update AFTER UPDATE ON jobs
			BEGIN DELETE FROM jobs WHERE job_id=NEW.job_id; END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, applyJob(t.Context(), state, contracts.Event{
			Type: "JobSucceeded", DraftID: draftID,
			Payload: map[string]any{"job_id": "job-reconcile-failure"},
		}), "no rows")
	})
	t.Run("running projection missing draft", func(t *testing.T) {
		database := openTestDB(t)
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, updateRunningJobs(
			t.Context(), state, "draft-running-missing", "job", "running",
			contracts.Event{Payload: map[string]any{"kind": "render_final"}},
		), "草稿不存在")
	})
	t.Run("running projection query failure", func(t *testing.T) {
		database := openTestDB(t)
		state, _ := issue155ApplyState(t, database)
		state.originalVersions["draft-running-query-missing"] = 0
		requireIssue155ReducerError(t, updateRunningJobs(
			t.Context(), state, "draft-running-query-missing", "job", "running",
			contracts.Event{Payload: map[string]any{"kind": "render_final"}},
		), "no rows")
	})
	t.Run("running projection update failure", func(t *testing.T) {
		database := openTestDB(t)
		const draftID = "draft-running-update-failure"
		createDraft(t, database, draftID)
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER issue155_reject_running_jobs_update
			BEFORE UPDATE OF running_jobs_json ON drafts
			BEGIN SELECT RAISE(ABORT, 'running jobs update rejected'); END`,
		); err != nil {
			t.Fatal(err)
		}
		state, _ := issue155ApplyState(t, database)
		requireIssue155ReducerError(t, updateRunningJobs(
			t.Context(), state, draftID, "job", "pending",
			contracts.Event{Payload: map[string]any{"kind": "render_final", "progress": 0}},
		), "running jobs update rejected")
	})
}
