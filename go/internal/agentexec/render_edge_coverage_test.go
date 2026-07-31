package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestPreviewWaitReturnsEveryPersistedTerminalShape(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		resultJSON any
		errorJSON  any
		wantStatus string
		wantError  string
	}{
		{
			name: "failed_with_bounded_error", status: "failed",
			errorJSON: map[string]any{
				"error_code": "render_failed", "message": "/private/tmp/secret.mp4 failed", "retryable": true,
			},
			wantStatus: string(rushestools.StatusFailed),
		},
		{name: "cancelled", status: "cancelled", wantStatus: string(rushestools.StatusCancelled)},
		{name: "succeeded_without_result", status: "succeeded", wantError: "缺少有效 result_json"},
		{
			name: "succeeded_with_wrong_version", status: "succeeded",
			resultJSON: map[string]any{"artifact_id": "preview_wrong", "timeline_version": 2},
			wantError:  "结果与请求不一致",
		},
		{name: "unknown_status", status: "mystery", wantError: "状态无效"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, exec, _, document := previewRenderFixture(t, "draft_preview_terminal_"+test.name)
			exec.jobPollInterval = 0
			exec.jobWaitTimeout = 0
			jobID := "job_preview_terminal_" + test.name
			insertPreviewJobState(t, database, document.DraftID, jobID, test.status, test.resultJSON, test.errorJSON)

			result, err := exec.waitForPreviewJob(
				t.Context(), document.DraftID, jobID, document.TimelineID, document.Version, "auto",
			)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("err=%v want contains %q", err, test.wantError)
				}
				return
			}
			if err != nil || result.Status != test.wantStatus || result.Data["job_status"] != test.status {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if test.status == "failed" {
				failure, _ := result.Data["error"].(map[string]any)
				if failure["error_code"] != "render_failed" ||
					strings.Contains(InterfaceString(failure["message"]), "/private/") {
					t.Fatalf("failure=%#v", failure)
				}
			}
		})
	}
}

func TestPreviewWaitSurfacesMissingJobAndDeadline(t *testing.T) {
	database, exec, _, document := previewRenderFixture(t, "draft_preview_wait_edges")
	if _, err := exec.waitForPreviewJob(
		t.Context(), document.DraftID, "missing_job", document.TimelineID, 1, "auto",
	); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing job err=%v", err)
	}
	if _, err := exec.previewJobState(t.Context(), "other_draft", "missing_job"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing state err=%v", err)
	}

	insertPreviewJobState(t, database, document.DraftID, "job_preview_deadline", "pending", nil, nil)
	exec.jobPollInterval = time.Second
	exec.jobWaitTimeout = time.Second
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	result, err := exec.waitForPreviewJob(
		ctx, document.DraftID, "job_preview_deadline", document.TimelineID, 1, "portrait",
	)
	if err != nil || result.Status != string(rushestools.StatusTimeout) ||
		result.Data["underlying_job_continues"] != true {
		t.Fatalf("deadline result=%#v err=%v", result, err)
	}

	for _, value := range []any{nil, 0, -1, 1.5, math.Inf(1), float64(math.MaxInt) * 2} {
		if got, ok := positiveIntValue(value); ok || got != 0 {
			t.Fatalf("positiveIntValue(%v)=(%d,%v)", value, got, ok)
		}
	}
}

func TestPreviewEnqueueStorageAndCommitFailures(t *testing.T) {
	t.Run("draft_missing", func(t *testing.T) {
		database := agenttest.AgentTestDatabase(t)
		exec, err := newTestExecutor(t.Context(), database, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(t.Context(), "missing_draft", "auto", "missing:v1"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("timeline_missing", func(t *testing.T) {
		database := agenttest.AgentTestDatabase(t)
		const draftID = "draft_preview_no_timeline"
		agenttest.CreateAgentDraft(t, database, draftID)
		exec, err := newTestExecutor(t.Context(), database, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(t.Context(), draftID, "auto", draftID+":v1"); err == nil || !strings.Contains(err.Error(), "没有时间线") {
			t.Fatalf("err=%v", err)
		}
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE drafts SET timeline_current_version=99 WHERE draft_id=?", draftID,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(t.Context(), draftID, "auto", draftID+":v99"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("missing current snapshot err=%v", err)
		}
	})

	t.Run("revalidates_existing_job", func(t *testing.T) {
		database, exec, _, document := previewRenderFixture(t, "draft_preview_revalidate")
		first, err := exec.enqueuePreviewRender(t.Context(), document.DraftID, "auto", document.TimelineID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(),
			"UPDATE drafts SET timeline_validated=0 WHERE draft_id=?", document.DraftID,
		); err != nil {
			t.Fatal(err)
		}
		replayed, err := exec.enqueuePreviewRender(t.Context(), document.DraftID, "auto", document.TimelineID)
		if err != nil || replayed.Data["job_id"] != first.Data["job_id"] {
			t.Fatalf("replayed=%#v err=%v", replayed, err)
		}
		var validated bool
		if err := database.Read().QueryRowContext(t.Context(),
			"SELECT timeline_validated FROM drafts WHERE draft_id=?", document.DraftID,
		).Scan(&validated); err != nil || !validated {
			t.Fatalf("validated=%v err=%v", validated, err)
		}
	})

	t.Run("invalid_validation_commit", func(t *testing.T) {
		database, exec, _, document := previewRenderFixture(t, "draft_preview_invalid_commit")
		document.DurationFrames = 0
		if _, err := seedTimelineVersion(exec, t.Context(), document.DraftID,
			timeline.Document{
				TimelineID: document.DraftID + ":v2", DraftID: document.DraftID,
				Version: 2, FPS: document.FPS, DurationFrames: 0, Tracks: document.Tracks,
			}, "invalid_preview_v2", nil,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER fail_invalid_preview_validation BEFORE UPDATE OF timeline_validated ON drafts
			WHEN NEW.timeline_validated=0
			BEGIN SELECT RAISE(ABORT, 'fixture validation commit failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(
			t.Context(), document.DraftID, "auto", document.DraftID+":v2",
		); err == nil || !strings.Contains(err.Error(), "fixture validation commit failure") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("persistent_version_conflict", func(t *testing.T) {
		database, exec, _, document := previewRenderFixture(t, "draft_preview_commit_conflict")
		if _, err := database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER conflict_preview_state AFTER UPDATE OF timeline_validated ON drafts
			WHEN NEW.timeline_validated=1
			BEGIN UPDATE drafts SET state_version=state_version+1 WHERE draft_id=NEW.draft_id; END`); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(t.Context(), document.DraftID, "auto", document.TimelineID); err == nil || !strings.Contains(err.Error(), "version_conflict") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("jobs_table_missing", func(t *testing.T) {
		database, exec, _, document := previewRenderFixture(t, "draft_preview_jobs_missing")
		if _, err := database.Write().ExecContext(t.Context(), "DROP TABLE jobs"); err != nil {
			t.Fatal(err)
		}
		if _, err := exec.enqueuePreviewRender(t.Context(), document.DraftID, "auto", document.TimelineID); err == nil {
			t.Fatal("job lookup should fail")
		}
	})
}

func TestPreviewInspectionFrameContextCoversMissingAndExplicitAudio(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_preview_context_edges"
	agenttest.CreateAgentDraft(t, database, draftID)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES(
			'asset_context_edge','reference','/tmp/context-edge.mp4','video','local_path',
			'context-edge.mp4','context-edge',1,'{}','ready','ready',1
		);
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES(
			'transcript_context_edge','asset_context_edge','fixture',0,
			'[{"utterance_id":"u1","source_start_frame":0,"source_end_frame":20,"text":"边界台词"}]','[]'
		)`); err != nil {
		t.Fatal(err)
	}
	document := timeline.Empty(draftID, 1)
	document.DurationFrames = 30
	for index := range document.Tracks {
		if document.Tracks[index].TrackID == "original_audio" {
			document.Tracks[index].Clips = []timeline.Clip{
				{TrackID: "original_audio", TimelineStartFrame: 0, TimelineEndFrame: 30},
				{TrackID: "original_audio", AssetID: "missing_context_asset", TimelineStartFrame: 0, TimelineEndFrame: 30},
				{TrackID: "original_audio", AssetID: "missing_context_asset", TimelineStartFrame: 0, TimelineEndFrame: 30},
				{
					TrackID: "original_audio", AssetID: "asset_context_edge",
					TimelineStartFrame: 0, TimelineEndFrame: 30,
					SourceStartFrame: 0, SourceEndFrame: 20, PlaybackRate: 0,
				},
				{
					TrackID: "original_audio", AssetID: "asset_context_edge",
					TimelineStartFrame: 0, TimelineEndFrame: 30,
					SourceStartFrame: 10, SourceEndFrame: 10, PlaybackRate: 1,
				},
			}
		}
	}
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	contexts, err := exec.PreviewInspectionFrameContext(t.Context(), document, []int{1})
	if err != nil || !strings.Contains(contexts[1], "边界台词") {
		t.Fatalf("contexts=%#v err=%v", contexts, err)
	}

	closedDatabase, closedExec, _, closedDocument := previewRenderFixture(t, "draft_preview_context_closed")
	for index := range closedDocument.Tracks {
		if closedDocument.Tracks[index].TrackID == "original_audio" {
			closedDocument.Tracks[index].Clips = []timeline.Clip{{
				TrackID: "original_audio", AssetID: "fixture",
				TimelineStartFrame: 0, TimelineEndFrame: 30, SourceEndFrame: 30,
			}}
		}
	}
	if err := closedDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closedExec.PreviewInspectionFrameContext(t.Context(), closedDocument, []int{1}); err == nil {
		t.Fatal("transcript lookup should surface closed database")
	}
}

func TestInspectPreviewCheckSurfacesPersistedArtifactFailures(t *testing.T) {
	for _, test := range []struct {
		name            string
		hash            string
		timelineVersion int
	}{
		{name: "invalid_object_hash", hash: "short", timelineVersion: 1},
		{name: "missing_timeline", hash: strings.Repeat("a", 64), timelineVersion: 99},
		{name: "missing_object_file", hash: strings.Repeat("b", 64), timelineVersion: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, exec, _, document := previewRenderFixture(t, "draft_preview_inspect_"+test.name)
			insertPreviewArtifactRecord(
				t, database, document.DraftID, "preview_"+test.name, test.hash, test.timelineVersion,
			)
			if _, err := exec.inspectPreviewCheck(
				t.Context(), document.DraftID, "preview_"+test.name, "decode",
			); err == nil {
				t.Fatal("persisted artifact failure should surface")
			}
		})
	}
}

func previewRenderFixture(
	t *testing.T,
	draftID string,
) (*storage.DB, *Executor, context.Context, timeline.Document) {
	t.Helper()
	database := agenttest.AgentTestDatabase(t)
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
	if _, err := seedTimelineVersion(exec, t.Context(), draftID, document, "preview_render_fixture", nil); err != nil {
		t.Fatal(err)
	}
	return database, exec, rushestools.WithDraftID(t.Context(), draftID), document
}

func insertPreviewJobState(
	t *testing.T,
	database *storage.DB,
	draftID, jobID, status string,
	resultJSON, errorJSON any,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"idempotency_key": jobID, "job_payload": map[string]any{"timeline_version": 1},
			"next_run_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("insert preview job status=%s err=%v", result.Status, err)
	}
	if status == "pending" {
		return
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status=?, result_json=json(?), error_json=json(?) WHERE job_id=?`,
		status, nullableFixtureJSON(resultJSON), nullableFixtureJSON(errorJSON), jobID,
	); err != nil {
		t.Fatal(err)
	}
}

func nullableFixtureJSON(value any) any {
	if value == nil {
		return nil
	}
	return mustFixtureJSON(value)
}

func mustFixtureJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func insertPreviewArtifactRecord(
	t *testing.T,
	database *storage.DB,
	draftID, previewID, hash string,
	timelineVersion int,
) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(),
		"INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?,?,0,?)",
		hash, "objects/fixture", now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO previews(
			preview_id,draft_id,timeline_version,object_hash,quality_json,
			render_width,render_height,render_fps,expected_duration_sec,created_at
		) VALUES(?,?,?,?, '{}',64,64,30,2,?)`, previewID, draftID, timelineVersion, hash, now,
	); err != nil {
		t.Fatal(err)
	}
}
