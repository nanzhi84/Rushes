package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type rewindBlockingModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (stub *rewindBlockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *rewindBlockingModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.once.Do(func() { close(stub.started) })
	<-stub.release
	return schema.AssistantMessage("迟到回复", nil), nil
}

func (stub *rewindBlockingModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestRewindAPIListsDiffsAndRestoresBothWithCancellation(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	draftID := "draft-rewind-api"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIRewindMessage(t, server.database, draftID, "api-user-1", "保留第一版")
	createAPIRewindTimeline(t, server.database, draftID, 1, "api-clip-1", 30)
	insertAPIRewindMessage(t, server.database, draftID, "api-user-2", "制作第二版")
	createAPIRewindTimeline(t, server.database, draftID, 2, "api-clip-2", 90)

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, apiRequest(t, http.MethodGet,
		"/api/drafts/"+draftID+"/rewind/checkpoints", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var listed RewindCheckpointsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || len(listed.Checkpoints) != 4 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	target := apiCheckpointForVersion(t, listed.Checkpoints, 1)
	latest := apiCheckpointForVersion(t, listed.Checkpoints, 2)
	oldest := listed.Checkpoints[len(listed.Checkpoints)-1]
	if latest.ClipCountDelta != 0 || latest.DurationFramesDelta != 60 || target.AnchorMessageId == nil ||
		*target.AnchorMessageId != "api-user-1" {
		t.Fatalf("checkpoint diffs latest=%#v target=%#v", latest, target)
	}
	if oldest.ClipCountDelta != 0 || oldest.DurationFramesDelta != 0 || oldest.TrackCountDelta != 0 {
		t.Fatalf("oldest checkpoint must not fabricate a predecessor diff: %#v", oldest)
	}

	draft, _ := storage.GetDraft(t.Context(), server.database.Read(), draftID)
	base := draft.StateVersion
	if result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": "rewind-pending-decision", "scope_type": "draft", "type": "generic",
			"question": "继续？", "options": []any{}, "allow_free_text": true, "blocking": true,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("decision result=%#v err=%v", result, err)
	}
	draft, _ = storage.GetDraft(t.Context(), server.database.Read(), draftID)
	base = draft.StateVersion
	if result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": "rewind-pending-job", "kind": "render_preview",
			"requested_by_draft_id": draftID, "next_run_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("job result=%#v err=%v", result, err)
	}

	restore := httptest.NewRecorder()
	handler.ServeHTTP(restore, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id": target.CheckpointId, "idempotency_key": "restore-both-1", "mode": "both",
		}))
	if restore.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restore.Code, restore.Body.String())
	}
	var restored RewindRestoreResponse
	if err := json.Unmarshal(restore.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.TimelineVersion == nil || *restored.TimelineVersion != 3 ||
		restored.CancelledJobs != 1 || restored.CancelledDecisions != 1 ||
		restored.RewoundMessageCount < 1 || len(restored.EventIds) != 2 {
		t.Fatalf("restored=%#v", restored)
	}
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id": target.CheckpointId, "idempotency_key": "restore-both-1", "mode": "both",
		}))
	if retry.Code != http.StatusOK || retry.Body.String() != restore.Body.String() {
		t.Fatalf("retry status=%d body=%s first=%s", retry.Code, retry.Body.String(), restore.Body.String())
	}
	storedCheckpoint, err := storage.GetRewindCheckpoint(
		t.Context(), server.database.Read(), draftID, target.CheckpointId,
	)
	if err != nil {
		t.Fatal(err)
	}
	directRetry, err := server.applyRewind(
		t.Context(), draftID, "both", "restore-both-1", storedCheckpoint, nil,
	)
	if err != nil || directRetry.TimelineVersion == nil || *directRetry.TimelineVersion != 3 ||
		len(directRetry.EventIds) != len(restored.EventIds) {
		t.Fatalf("direct retry=%#v err=%v", directRetry, err)
	}
	if _, err := server.applyRewind(
		t.Context(), draftID, "conversation", "restore-both-1", storedCheckpoint, nil,
	); !errors.Is(err, reducer.ErrRewindRestoreDuplicate) {
		t.Fatalf("direct key reuse err=%v", err)
	}
	reused := httptest.NewRecorder()
	handler.ServeHTTP(reused, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id": target.CheckpointId, "idempotency_key": "restore-both-1", "mode": "conversation",
		}))
	if reused.Code != http.StatusConflict || !strings.Contains(reused.Body.String(), "rewind_idempotency_key_reused") {
		t.Fatalf("reused status=%d body=%s", reused.Code, reused.Body.String())
	}
	var cancelledEvents, restoredEvents int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE draft_id=? AND event_type='JobCancelled'`, draftID,
	).Scan(&cancelledEvents); err != nil || cancelledEvents != 1 {
		t.Fatalf("cancel events=%d err=%v", cancelledEvents, err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE draft_id=? AND event_type='TimelineVersionRestored'`, draftID,
	).Scan(&restoredEvents); err != nil || restoredEvents != 1 {
		t.Fatalf("restore events=%d err=%v", restoredEvents, err)
	}
	var parent, jobStatus, decisionStatus string
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT CAST(parent_version AS TEXT) FROM timeline_versions
		WHERE draft_id=? AND version=3`, draftID).Scan(&parent); err != nil || parent != "1" {
		t.Fatalf("parent=%q err=%v", parent, err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT status FROM jobs WHERE job_id='rewind-pending-job'`).Scan(&jobStatus); err != nil || jobStatus != "cancelled" {
		t.Fatalf("job status=%q err=%v", jobStatus, err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT status FROM decisions WHERE decision_id='rewind-pending-decision'`).Scan(&decisionStatus); err != nil || decisionStatus != "cancelled" {
		t.Fatalf("decision status=%q err=%v", decisionStatus, err)
	}
	messages := httptest.NewRecorder()
	handler.ServeHTTP(messages, apiRequest(t, http.MethodGet,
		"/api/drafts/"+draftID+"/messages?limit=200", nil))
	var messageResponse MessagesResponse
	if err := json.Unmarshal(messages.Body.Bytes(), &messageResponse); err != nil ||
		messageResponse.RewoundMessageCount == 0 || len(messageResponse.Messages) != 2 ||
		messageResponse.Messages[1].Kind != "rewind" {
		t.Fatalf("messages=%#v err=%v", messageResponse, err)
	}
}

func TestRewindAPIRestoresTimelineAndConversationIndependently(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	for _, mode := range []string{"timeline", "conversation"} {
		t.Run(mode, func(t *testing.T) {
			draftID := "draft-rewind-" + mode
			createDraftThroughAPI(t, handler, draftID)
			insertAPIRewindMessage(t, server.database, draftID, mode+"-user-1", "保留第一版")
			createAPIRewindTimeline(t, server.database, draftID, 1, mode+"-clip-1", 30)
			insertAPIRewindMessage(t, server.database, draftID, mode+"-user-2", "制作第二版")
			createAPIRewindTimeline(t, server.database, draftID, 2, mode+"-clip-2", 60)

			list := httptest.NewRecorder()
			handler.ServeHTTP(list, apiRequest(t, http.MethodGet,
				"/api/drafts/"+draftID+"/rewind/checkpoints", nil))
			var checkpoints RewindCheckpointsResponse
			if err := json.Unmarshal(list.Body.Bytes(), &checkpoints); list.Code != http.StatusOK || err != nil {
				t.Fatalf("list status=%d body=%s err=%v", list.Code, list.Body.String(), err)
			}
			target := apiCheckpointForVersion(t, checkpoints.Checkpoints, 1)

			restore := httptest.NewRecorder()
			handler.ServeHTTP(restore, apiRequest(t, http.MethodPost,
				"/api/drafts/"+draftID+"/rewind", map[string]any{
					"checkpoint_id": target.CheckpointId, "idempotency_key": "restore-" + mode, "mode": mode,
				}))
			if restore.Code != http.StatusOK {
				t.Fatalf("restore status=%d body=%s", restore.Code, restore.Body.String())
			}
			var restored RewindRestoreResponse
			if err := json.Unmarshal(restore.Body.Bytes(), &restored); err != nil {
				t.Fatal(err)
			}
			draft, err := storage.GetDraft(t.Context(), server.database.Read(), draftID)
			if err != nil || draft.TimelineCurrentVersion == nil {
				t.Fatalf("draft=%#v err=%v", draft, err)
			}
			messages, err := storage.ListMessages(t.Context(), server.database.Read(), draftID, 20)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "timeline" {
				if *draft.TimelineCurrentVersion != 3 || restored.RewoundMessageCount != 0 ||
					len(messages) != 3 || messages[1].ID != mode+"-user-2" {
					t.Fatalf("timeline restore draft=%#v response=%#v messages=%#v", draft, restored, messages)
				}
				return
			}
			if *draft.TimelineCurrentVersion != 2 || restored.RewoundMessageCount == 0 ||
				len(messages) != 2 || messages[0].ID != mode+"-user-1" || messages[1].Kind != "rewind" {
				t.Fatalf("conversation restore draft=%#v response=%#v messages=%#v", draft, restored, messages)
			}
		})
	}
}

func TestRewindAPIRejectsInvalidTargetsAndMissingDrafts(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	draftID := "draft-rewind-errors"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIRewindMessage(t, server.database, draftID, "message-without-timeline", "还没有时间线")
	rows, err := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%#v err=%v", rows, err)
	}
	tests := []struct {
		name   string
		path   string
		body   any
		status int
		reason string
	}{
		{"invalid mode", "/api/drafts/" + draftID + "/rewind", map[string]any{"checkpoint_id": rows[0].ID, "idempotency_key": "invalid-mode", "mode": "bad"}, 400, "rewind_request_invalid"},
		{"missing checkpoint", "/api/drafts/" + draftID + "/rewind", map[string]any{"checkpoint_id": "missing", "idempotency_key": "missing-checkpoint", "mode": "conversation"}, 404, "rewind_checkpoint_not_found"},
		{"timeline unavailable", "/api/drafts/" + draftID + "/rewind", map[string]any{"checkpoint_id": rows[0].ID, "idempotency_key": "no-timeline", "mode": "timeline"}, 400, "rewind_checkpoint_has_no_timeline"},
		{"missing draft restore", "/api/drafts/missing/rewind", map[string]any{"checkpoint_id": "x", "idempotency_key": "missing-draft", "mode": "both"}, 404, "draft_not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, test.path, test.body))
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.reason) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	missingList := httptest.NewRecorder()
	handler.ServeHTTP(missingList, apiRequest(t, http.MethodGet,
		"/api/drafts/missing/rewind/checkpoints", nil))
	if missingList.Code != http.StatusNotFound || !strings.Contains(missingList.Body.String(), "draft_not_found") {
		t.Fatalf("missing list status=%d body=%s", missingList.Code, missingList.Body.String())
	}
	conversation := httptest.NewRecorder()
	handler.ServeHTTP(conversation, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id": rows[0].ID, "idempotency_key": "conversation-only", "mode": "conversation",
		}))
	if conversation.Code != http.StatusOK || !strings.Contains(conversation.Body.String(), `"timeline_version":null`) {
		t.Fatalf("conversation status=%d body=%s", conversation.Code, conversation.Body.String())
	}
}

func TestRewindAPIReturnsStableErrorsForUnavailableRuntimeState(t *testing.T) {
	t.Parallel()
	t.Run("list storage closed", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		if err := server.database.Close(); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet,
			"/api/drafts/draft-closed/rewind/checkpoints", nil))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("restore storage closed", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-restore-closed"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "closed-anchor", "锚点")
		if err := server.database.Close(); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": "rewind:message:closed-anchor", "idempotency_key": "closed", "mode": "conversation",
			}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("checkpoint table unavailable", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		createDraftThroughAPI(t, handler, "draft-rewind-table-error")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE rewind_checkpoints"); err != nil {
			t.Fatal(err)
		}
		list := httptest.NewRecorder()
		handler.ServeHTTP(list, apiRequest(t, http.MethodGet,
			"/api/drafts/draft-rewind-table-error/rewind/checkpoints", nil))
		if list.Code != http.StatusInternalServerError {
			t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
		}
		restore := httptest.NewRecorder()
		handler.ServeHTTP(restore, apiRequest(t, http.MethodPost,
			"/api/drafts/draft-rewind-table-error/rewind", map[string]any{
				"checkpoint_id": "missing", "idempotency_key": "table-missing", "mode": "conversation",
			}))
		if restore.Code != http.StatusInternalServerError {
			t.Fatalf("restore status=%d body=%s", restore.Code, restore.Body.String())
		}
	})

	t.Run("restore request table unavailable", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-request-table-error"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "request-table-anchor", "锚点")
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE rewind_restore_requests"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": rows[0].ID, "idempotency_key": "request-table-missing", "mode": "conversation",
			}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("turn queue closed", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-queue-closed"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "queue-anchor", "锚点")
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		server.agent.Queue().Close()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": rows[0].ID, "idempotency_key": "queue-closed", "mode": "conversation",
			}))
		if recorder.Code != http.StatusServiceUnavailable ||
			!strings.Contains(recorder.Body.String(), "turn_queue_closed") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("jobs table unavailable", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-jobs-error"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "jobs-anchor", "锚点")
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE jobs"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": rows[0].ID, "idempotency_key": "jobs-error", "mode": "conversation",
			}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupt timeline snapshot", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-corrupt-api"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "corrupt-anchor", "锚点")
		createAPIRewindTimeline(t, server.database, draftID, 1, "corrupt-clip", 30)
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		target := storageCheckpointForVersion(t, rows, 1)
		if _, err := server.database.Write().ExecContext(t.Context(), `
			UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1`, draftID,
		); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": target.ID, "idempotency_key": "corrupt", "mode": "timeline",
			}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("job state changes during atomic restore", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-job-race"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "race-anchor", "锚点")
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		draft, _ := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: draftID,
			Payload: map[string]any{
				"job_id": "rewind-racing-job", "kind": "render_preview",
				"requested_by_draft_id": draftID,
				"next_run_at":           time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			},
		}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("job result=%#v err=%v", result, err)
		}
		if _, err := server.database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER change_rewind_job_state
			AFTER INSERT ON rewind_restore_requests
			BEGIN
				UPDATE jobs SET status='succeeded' WHERE job_id='rewind-racing-job';
			END`); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": rows[0].ID, "idempotency_key": "job-race", "mode": "conversation",
			}))
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "rewind_job_state_changed") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var status string
		if err := server.database.Read().QueryRowContext(t.Context(), `
			SELECT status FROM jobs WHERE job_id='rewind-racing-job'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "pending" {
			t.Fatalf("failed rewind must roll back concurrent state simulation, status=%q", status)
		}
	})

	t.Run("draft version changes during atomic restore", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		draftID := "draft-rewind-version-race"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIRewindMessage(t, server.database, draftID, "version-race-anchor", "锚点")
		rows, _ := storage.ListRewindCheckpoints(t.Context(), server.database.Read(), draftID, 50)
		before, err := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER change_rewind_draft_version
			AFTER INSERT ON rewind_restore_requests
			BEGIN
				UPDATE drafts SET state_version=state_version+1 WHERE draft_id='draft-rewind-version-race';
			END`); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id": rows[0].ID, "idempotency_key": "version-race", "mode": "conversation",
			}))
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), string(reducer.StatusVersionConflict)) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		draft, err := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		if err != nil {
			t.Fatal(err)
		}
		if draft.StateVersion != before.StateVersion {
			t.Fatalf("failed rewind must roll back version-race simulation, version=%d", draft.StateVersion)
		}
	})

	failure := &rewindReducerResultError{result: reducer.Result{Status: reducer.StatusVersionConflict}}
	if !strings.Contains(failure.Error(), string(reducer.StatusVersionConflict)) {
		t.Fatalf("error=%q", failure.Error())
	}
	server, _ := testServer(t, t.TempDir(), 0)
	if _, err := server.applyRewind(t.Context(), "missing-draft", "conversation", "missing", storage.RewindCheckpoint{}, nil); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft err=%v", err)
	}
}

func TestRewindAPIDoesNotCommitWhenTurnCancellationTimesOut(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	blocking := &rewindBlockingModel{started: make(chan struct{}), release: make(chan struct{})}
	agentService, err := agent.NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agentService.Close)
	server, err := NewServer(Config{
		Database: database, Agent: agentService, Token: testToken, Port: 8000,
		FSRoots: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	draftID := "draft-rewind-timeout"
	createDraftThroughAPI(t, server.Handler(), draftID)
	insertAPIRewindMessage(t, database, draftID, "timeout-anchor", "锚点")
	if !agentService.Queue().EnqueueUserMessage(draftID, "timeout-turn", "阻塞") {
		t.Fatal("turn enqueue failed")
	}
	<-blocking.started

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id":   "rewind:message:timeout-anchor",
			"idempotency_key": "timeout-request", "mode": "conversation",
		}))
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), "rewind_cancellation_timeout") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	retryWhileDraining := httptest.NewRecorder()
	server.Handler().ServeHTTP(retryWhileDraining, apiRequest(t, http.MethodPost,
		"/api/drafts/"+draftID+"/rewind", map[string]any{
			"checkpoint_id":   "rewind:message:timeout-anchor",
			"idempotency_key": "timeout-request", "mode": "conversation",
		}))
	if retryWhileDraining.Code != http.StatusConflict ||
		!strings.Contains(retryWhileDraining.Body.String(), "rewind_cancellation_timeout") {
		t.Fatalf("draining retry status=%d body=%s", retryWhileDraining.Code, retryWhileDraining.Body.String())
	}
	var restores, requests int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionRestored'`,
		draftID).Scan(&restores); err != nil || restores != 0 {
		t.Fatalf("restore events=%d err=%v", restores, err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM rewind_restore_requests WHERE draft_id=?`, draftID,
	).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("restore requests=%d err=%v", requests, err)
	}
	close(blocking.release)
	agentService.Queue().JoinDraft(draftID)
	deadline := time.Now().Add(time.Second)
	for {
		retry := httptest.NewRecorder()
		server.Handler().ServeHTTP(retry, apiRequest(t, http.MethodPost,
			"/api/drafts/"+draftID+"/rewind", map[string]any{
				"checkpoint_id":   "rewind:message:timeout-anchor",
				"idempotency_key": "timeout-request", "mode": "conversation",
			}))
		if retry.Code == http.StatusOK {
			break
		}
		if retry.Code != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("drained retry status=%d body=%s", retry.Code, retry.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	var lateMessages int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE draft_id=? AND content='迟到回复'`, draftID,
	).Scan(&lateMessages); err != nil || lateMessages != 0 {
		t.Fatalf("late messages=%d err=%v", lateMessages, err)
	}
}

func insertAPIRewindMessage(
	t *testing.T,
	database *storage.DB,
	draftID string,
	messageID string,
	content string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("message result=%#v err=%v", result, err)
	}
}

func createAPIRewindTimeline(
	t *testing.T,
	database *storage.DB,
	draftID string,
	version int,
	clipID string,
	duration int,
) {
	t.Helper()
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": fmt.Sprintf("%s:v%d", draftID, version), "timeline_version": version,
			"patch_id": fmt.Sprintf("api-patch-%d", version),
			"document_json": map[string]any{
				"timeline_id": fmt.Sprintf("%s:v%d", draftID, version), "draft_id": draftID,
				"version": version, "fps": 30, "duration_frames": duration,
				"tracks": []any{map[string]any{
					"track_id": "visual_base", "track_type": "video",
					"clips": []any{map[string]any{"timeline_clip_id": clipID}},
				}},
			},
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("timeline result=%#v err=%v", result, err)
	}
}

func apiCheckpointForVersion(
	t *testing.T,
	checkpoints []RewindCheckpoint,
	version int,
) RewindCheckpoint {
	t.Helper()
	for _, checkpoint := range checkpoints {
		if checkpoint.TimelineVersion != nil && *checkpoint.TimelineVersion == version &&
			checkpoint.TriggerKind == RewindCheckpointTriggerKindTimelineWrite {
			return checkpoint
		}
	}
	t.Fatalf("missing checkpoint v%d: %#v", version, checkpoints)
	return RewindCheckpoint{}
}

func storageCheckpointForVersion(
	t *testing.T,
	checkpoints []storage.RewindCheckpoint,
	version int,
) storage.RewindCheckpoint {
	t.Helper()
	for _, checkpoint := range checkpoints {
		if checkpoint.TimelineVersion != nil && *checkpoint.TimelineVersion == version &&
			checkpoint.TriggerKind == "timeline_write" {
			return checkpoint
		}
	}
	t.Fatalf("missing storage checkpoint v%d: %#v", version, checkpoints)
	return storage.RewindCheckpoint{}
}
