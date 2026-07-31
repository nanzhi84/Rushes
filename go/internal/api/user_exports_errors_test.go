package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

func TestUserExportEndpointsReturnStableErrorsAndDefaultOrientation(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	const draftID = "draft-user-export-errors"
	createDraftThroughAPI(t, handler, draftID)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		status int
		reason string
	}{
		{
			name: "missing draft list", method: http.MethodGet,
			path:   "/api/drafts/missing-user-export/exports",
			status: http.StatusNotFound, reason: "draft_or_timeline_not_found",
		},
		{
			name: "timeline id required", method: http.MethodPost,
			path: "/api/drafts/" + draftID + "/exports", body: map[string]any{},
			status: http.StatusBadRequest, reason: "timeline_id_required",
		},
		{
			name: "invalid orientation", method: http.MethodPost,
			path:   "/api/drafts/" + draftID + "/exports",
			body:   map[string]any{"timeline_id": draftID + ":v1", "orientation": "square"},
			status: http.StatusBadRequest, reason: "invalid_orientation",
		},
		{
			name: "missing draft create", method: http.MethodPost,
			path:   "/api/drafts/missing-user-export/exports",
			body:   map[string]any{"timeline_id": "missing-user-export:v1"},
			status: http.StatusNotFound, reason: "draft_or_timeline_not_found",
		},
		{
			name: "missing timeline create", method: http.MethodPost,
			path:   "/api/drafts/" + draftID + "/exports",
			body:   map[string]any{"timeline_id": draftID + ":missing"},
			status: http.StatusNotFound, reason: "draft_or_timeline_not_found",
		},
		{
			name: "missing retry", method: http.MethodPost,
			path:   "/api/drafts/" + draftID + "/exports/missing-job/retry",
			status: http.StatusNotFound, reason: "export_job_not_found",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, apiRequest(t, test.method, test.path, test.body))
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.reason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	empty := httptest.NewRecorder()
	handler.ServeHTTP(empty, apiRequest(t, http.MethodGet, "/api/drafts/"+draftID+"/exports", nil))
	if empty.Code != http.StatusOK || empty.Body.String() != "{\"exports\":[]}\n" {
		t.Fatalf("empty list status=%d body=%s", empty.Code, empty.Body.String())
	}

	invalidJSON := httptest.NewRequest(
		http.MethodPost, "/api/drafts/"+draftID+"/exports", bytes.NewBufferString("{"),
	)
	invalidJSON.Host = "127.0.0.1:8000"
	invalidJSON.Header.Set("Authorization", "Bearer "+testToken)
	invalidJSON.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalidJSON)
	if invalidResponse.Code != http.StatusBadRequest ||
		!strings.Contains(invalidResponse.Body.String(), "invalid_json") {
		t.Fatalf("invalid json status=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}

	seedUserExportTimeline(t, server, handler, "draft-user-export-default-orientation")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, apiRequest(
		t, http.MethodPost, "/api/drafts/draft-user-export-default-orientation/exports",
		map[string]any{"timeline_id": "draft-user-export-default-orientation:v1"},
	))
	if created.Code != http.StatusAccepted {
		t.Fatalf("default orientation status=%d body=%s", created.Code, created.Body.String())
	}
	var record UserExportRecord
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Orientation != UserExportRecordOrientation("auto") {
		t.Fatalf("default orientation record=%#v", record)
	}
}

func TestUserExportErrorWriterCoversEveryPublicFailureClass(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t, t.TempDir(), 0)
	for _, test := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"lease", storage.ErrTimelineLockedByAgent, http.StatusConflict, "agent_edit_lease_active"},
		{"stale", agentexec.ErrUserExportStaleTimeline, http.StatusConflict, "stale_timeline"},
		{"not retryable", agentexec.ErrUserExportNotRetryable, http.StatusConflict, "export_not_retryable"},
		{"state conflict", agentexec.ErrUserExportStateConflict, http.StatusConflict, "export_state_conflict"},
		{"job missing", agentexec.ErrUserExportNotFound, http.StatusNotFound, "export_job_not_found"},
		{"draft missing", storage.ErrNotFound, http.StatusNotFound, "draft_or_timeline_not_found"},
		{"timeline required", agentexec.ErrUserExportTimelineRequired, http.StatusBadRequest, "timeline_id_required"},
		{"invalid timeline", &agentexec.UserExportValidationError{
			Report: map[string]any{"valid": false},
		}, http.StatusUnprocessableEntity, "timeline_validation_failed"},
		{"orientation", errors.New("orientation 必须是 auto、portrait 或 landscape"), http.StatusBadRequest, "invalid_orientation"},
		{"internal", errors.New("unexpected user export failure"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.writeUserExportError(response, test.err)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.reason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestUserExportCancelledRetryFallbackIsIdempotentAndProjectsTerminalFields(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	const draftID = "draft-user-export-cancelled-retry"
	seedUserExportTimeline(t, server, handler, draftID)
	createdResponse := requestUserExport(t, handler, draftID, draftID+":v1", "landscape")
	var created UserExportRecord
	if createdResponse.Code != http.StatusAccepted ||
		json.Unmarshal(createdResponse.Body.Bytes(), &created) != nil {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}

	notRetryable := httptest.NewRecorder()
	handler.ServeHTTP(notRetryable, apiRequest(
		t, http.MethodPost, "/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil,
	))
	if notRetryable.Code != http.StatusConflict ||
		!strings.Contains(notRetryable.Body.String(), "export_not_retryable") {
		t.Fatalf("pending retry status=%d body=%s", notRetryable.Code, notRetryable.Body.String())
	}

	if _, err := server.database.Write().ExecContext(t.Context(), `
		UPDATE jobs
		SET status='cancelled',
			payload_json=json_remove(payload_json, '$.root_idempotency_key'),
			error_json='{"error_code":"cancelled","message":"用户取消 /private/tmp/export.mp4"}',
			progress=-0.25,attempts=2,
			started_at='2026-07-31T12:00:00Z',finished_at='2026-07-31T12:00:01Z'
		WHERE job_id=?`, created.JobId,
	); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(
		t, http.MethodPost, "/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil,
	))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("cancelled retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	var retried UserExportRecord
	if err := json.Unmarshal(retry.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.JobId == created.JobId || retried.RetryOfJobId == nil ||
		*retried.RetryOfJobId != created.JobId || retried.TimelineId != draftID+":v1" {
		t.Fatalf("retried=%#v", retried)
	}
	replayed := httptest.NewRecorder()
	handler.ServeHTTP(replayed, apiRequest(
		t, http.MethodPost, "/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil,
	))
	var replayedRecord UserExportRecord
	if replayed.Code != http.StatusAccepted || json.Unmarshal(replayed.Body.Bytes(), &replayedRecord) != nil ||
		replayedRecord.JobId != retried.JobId {
		t.Fatalf("replayed retry status=%d body=%s", replayed.Code, replayed.Body.String())
	}

	if _, err := server.database.Write().ExecContext(t.Context(), `
		UPDATE jobs SET status='succeeded',progress=1.75,
			result_json='{"artifact_id":"export-terminal","profile":"landscape","private_path":"/tmp/private"}',
			started_at='2026-07-31T12:00:02Z',finished_at='2026-07-31T12:00:03Z'
		WHERE job_id=?`, retried.JobId,
	); err != nil {
		t.Fatal(err)
	}
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, apiRequest(t, http.MethodGet, "/api/drafts/"+draftID+"/exports", nil))
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "/tmp/private") {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var payload UserExportsResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Exports) != 2 || payload.Exports[0].JobId != retried.JobId ||
		payload.Exports[0].Progress != 1 || payload.Exports[0].ExportId == nil ||
		*payload.Exports[0].ExportId != "export-terminal" || payload.Exports[0].Profile == nil ||
		*payload.Exports[0].Profile != "landscape" || payload.Exports[0].Retryable {
		t.Fatalf("terminal export list=%#v", payload.Exports)
	}
	if payload.Exports[1].Progress != 0 || !payload.Exports[1].Retryable ||
		payload.Exports[1].Error == nil || payload.Exports[1].Error.ErrorCode != "cancelled" ||
		strings.Contains(payload.Exports[1].Error.Message, "/private/tmp") {
		t.Fatalf("cancelled export projection=%#v", payload.Exports[1])
	}
}

func TestUserExportRejectsStructurallyInvalidTimeline(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	const draftID = "draft-user-export-invalid-timeline"
	createDraftThroughAPI(t, handler, draftID)
	draft, err := storage.GetDraft(t.Context(), server.database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	baseVersion := draft.StateVersion
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": draftID + ":v1", "timeline_version": 1,
			"patch_id": "invalid-export-timeline", "edit_origin": "manual",
			"document_json": map[string]any{
				"timeline_id": draftID + ":v1", "draft_id": draftID,
				"version": 1, "fps": 0, "duration_frames": 0, "tracks": []any{},
			},
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &baseVersion,
		TimelineWriteAdmission: &reducer.TimelineWriteAdmission{Origin: "manual"},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("invalid timeline seed result=%#v err=%v", result, err)
	}
	response := requestUserExport(t, handler, draftID, draftID+":v1", "portrait")
	if response.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(response.Body.String(), "timeline_validation_failed") {
		t.Fatalf("invalid timeline export status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUserExportMalformedPersistedJobsFailClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		payloadJSON string
		resultJSON  string
		errorJSON   string
		wantStatus  int
	}{
		{
			name: "malformed payload", payloadJSON: `{`,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:        "missing fixed timeline",
			payloadJSON: `{"request_origin":"user","timeline_id":"","timeline_version":0,"orientation":"auto"}`,
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:        "malformed optional result and error are bounded",
			payloadJSON: `{"request_origin":"user","timeline_id":"draft-malformed-job:v1","timeline_version":1,"orientation":"auto"}`,
			resultJSON:  `{`, errorJSON: `{`, wantStatus: http.StatusOK,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, handler := testServer(t, t.TempDir(), 0)
			const draftID = "draft-malformed-job"
			createDraftThroughAPI(t, handler, draftID)
			var resultValue, errorValue any
			if test.resultJSON != "" {
				resultValue = test.resultJSON
			}
			if test.errorJSON != "" {
				errorValue = test.errorJSON
			}
			if _, err := server.database.Write().ExecContext(t.Context(), `
				INSERT INTO jobs(
					job_id,kind,status,draft_id,requested_by_draft_id,idempotency_key,payload_json,
					result_json,error_json,next_run_at,created_at
				) VALUES('malformed-user-export','render_final','failed',?,?,?, ?,?,?,?,?)`,
				draftID, draftID, "malformed-key", test.payloadJSON, resultValue, errorValue,
				"2026-07-31T12:00:00Z", "2026-07-31T12:00:00Z",
			); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, apiRequest(
				t, http.MethodGet, "/api/drafts/"+draftID+"/exports", nil,
			))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.wantStatus == http.StatusOK &&
				(strings.Contains(response.Body.String(), "artifact_id") ||
					strings.Contains(response.Body.String(), "error_code")) {
				t.Fatalf("malformed optional fields leaked: %s", response.Body.String())
			}
		})
	}
}

func TestUserExportRetryRejectsPinnedVersionMismatchAndDeletedTimeline(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		mutateSQL  string
		wantStatus int
		wantReason string
	}{
		{
			name: "version mismatch",
			mutateSQL: `UPDATE jobs SET status='cancelled',
				payload_json=json_set(payload_json, '$.timeline_version', 2) WHERE job_id=?`,
			wantStatus: http.StatusInternalServerError, wantReason: "internal_error",
		},
		{
			name: "deleted timeline",
			mutateSQL: `UPDATE jobs SET status='cancelled' WHERE job_id=?;
				DELETE FROM timeline_versions WHERE draft_id='draft-user-export-retry-corrupt'`,
			wantStatus: http.StatusNotFound, wantReason: "draft_or_timeline_not_found",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, handler := testServer(t, t.TempDir(), 0)
			const draftID = "draft-user-export-retry-corrupt"
			seedUserExportTimeline(t, server, handler, draftID)
			createdResponse := requestUserExport(t, handler, draftID, draftID+":v1", "portrait")
			var created UserExportRecord
			if createdResponse.Code != http.StatusAccepted ||
				json.Unmarshal(createdResponse.Body.Bytes(), &created) != nil {
				t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
			}
			if _, err := server.database.Write().ExecContext(t.Context(), test.mutateSQL, created.JobId); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, apiRequest(
				t, http.MethodPost,
				"/api/drafts/"+draftID+"/exports/"+created.JobId+"/retry", nil,
			))
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantReason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
