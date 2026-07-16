package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdvertisedLifecycleCostDeleteAndCancelRoutesDoNotFallBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	clip := filepath.Join(root, "clip.mp4")
	if err := os.WriteFile(clip, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, handler := testServer(t, root, 0)
	fullyConfigured := httptest.NewRecorder()
	handler.ServeHTTP(fullyConfigured, apiRequest(t, http.MethodPost, "/api/drafts", map[string]any{
		"draft_id": "draft_configured",
		"name":     "完整配置草稿",
		"brief":    map[string]any{"audience": "developer"},
		"goal":     "验证可选字段",
		"defaults": map[string]any{"fps": 30},
	}))
	if fullyConfigured.Code != http.StatusOK || !strings.Contains(fullyConfigured.Body.String(), "验证可选字段") {
		t.Fatalf("configured draft=%d body=%s", fullyConfigured.Code, fullyConfigured.Body.String())
	}
	createDraftThroughAPI(t, handler, "draft_routes")

	rename := httptest.NewRecorder()
	handler.ServeHTTP(rename, apiRequest(t, http.MethodPatch, "/api/drafts/draft_routes", map[string]any{
		"name": "改名后的草稿",
	}))
	if rename.Code != http.StatusOK || !strings.Contains(rename.Body.String(), "改名后的草稿") {
		t.Fatalf("rename=%d body=%s", rename.Code, rename.Body.String())
	}
	costs := httptest.NewRecorder()
	handler.ServeHTTP(costs, apiRequest(t, http.MethodGet, "/api/drafts/draft_routes/costs", nil))
	if costs.Code != http.StatusOK || !strings.Contains(costs.Body.String(), `"total_cost_estimate":0`) {
		t.Fatalf("costs=%d body=%s", costs.Code, costs.Body.String())
	}

	imported := httptest.NewRecorder()
	handler.ServeHTTP(imported, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_routes/materials/import-local", map[string]any{"path": clip}))
	if imported.Code != http.StatusOK {
		t.Fatalf("import=%d body=%s", imported.Code, imported.Body.String())
	}
	var mutation MaterialMutationResponse
	if err := json.Unmarshal(imported.Body.Bytes(), &mutation); err != nil ||
		mutation.AssetId == nil || mutation.JobId == nil {
		t.Fatalf("mutation=%#v err=%v", mutation, err)
	}

	copyResponse := httptest.NewRecorder()
	handler.ServeHTTP(copyResponse, apiRequest(t, http.MethodPost, "/api/drafts/draft_routes/copy", map[string]any{
		"draft_id": "draft_routes_copy", "name": "副本",
	}))
	if copyResponse.Code != http.StatusCreated || !strings.Contains(copyResponse.Body.String(), "副本") {
		t.Fatalf("copy=%d body=%s", copyResponse.Code, copyResponse.Body.String())
	}
	var copiedLinks int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM draft_asset_links WHERE draft_id='draft_routes_copy'`,
	).Scan(&copiedLinks); err != nil || copiedLinks != 1 {
		t.Fatalf("copied links=%d err=%v", copiedLinks, err)
	}
	autoCopy := httptest.NewRecorder()
	handler.ServeHTTP(autoCopy, apiRequest(t, http.MethodPost, "/api/drafts/draft_routes/copy", map[string]any{}))
	if autoCopy.Code != http.StatusCreated || !strings.Contains(autoCopy.Body.String(), "改名后的草稿 Copy") {
		t.Fatalf("auto copy=%d body=%s", autoCopy.Code, autoCopy.Body.String())
	}

	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, apiRequest(t, http.MethodDelete,
		"/api/drafts/draft_routes/materials/"+*mutation.AssetId, nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete material=%d body=%s", deleted.Code, deleted.Body.String())
	}

	cancelled := httptest.NewRecorder()
	handler.ServeHTTP(cancelled, apiRequest(t, http.MethodPost, "/api/jobs/"+*mutation.JobId+"/cancel",
		map[string]any{"reason": "用户取消"}))
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	var cancelReason string
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT json_extract(payload_json,'$.payload.reason') FROM event_log
		WHERE event_type='JobCancelled' AND json_extract(payload_json,'$.payload.job_id')=?`,
		*mutation.JobId).Scan(&cancelReason); err != nil || cancelReason != "user_cancelled" {
		t.Fatalf("cancel reason=%q err=%v", cancelReason, err)
	}
	idempotentCancel := httptest.NewRecorder()
	handler.ServeHTTP(idempotentCancel, apiRequest(t, http.MethodPost, "/api/jobs/"+*mutation.JobId+"/cancel", nil))
	if idempotentCancel.Code != http.StatusOK || !strings.Contains(idempotentCancel.Body.String(), `"event_ids":[]`) {
		t.Fatalf("idempotent cancel=%d body=%s", idempotentCancel.Code, idempotentCancel.Body.String())
	}
	var jobStatus string
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM jobs WHERE job_id=?", *mutation.JobId).Scan(&jobStatus); err != nil || jobStatus != "cancelled" {
		t.Fatalf("job status=%s err=%v", jobStatus, err)
	}
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO jobs(
			job_id, kind, status, draft_id, idempotency_key, payload_json, next_run_at, created_at
		) VALUES(
			'job_finished', 'render', 'succeeded', 'draft_routes', 'finished', '{}',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		), (
			'job_invalid_json', 'render', 'pending', 'draft_routes', 'invalid-json', '{}',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}
	finishedCancel := httptest.NewRecorder()
	handler.ServeHTTP(finishedCancel, apiRequest(t, http.MethodPost, "/api/jobs/job_finished/cancel", nil))
	if finishedCancel.Code != http.StatusConflict || !strings.Contains(finishedCancel.Body.String(), "job_not_cancellable") {
		t.Fatalf("finished cancel=%d body=%s", finishedCancel.Code, finishedCancel.Body.String())
	}
	invalidCancelRequest := apiRequest(t, http.MethodPost, "/api/jobs/job_invalid_json/cancel", nil)
	invalidCancelRequest.Body = io.NopCloser(strings.NewReader("{"))
	invalidCancelRequest.ContentLength = 1
	invalidCancel := httptest.NewRecorder()
	handler.ServeHTTP(invalidCancel, invalidCancelRequest)
	if invalidCancel.Code != http.StatusBadRequest || !strings.Contains(invalidCancel.Body.String(), "invalid_json") {
		t.Fatalf("invalid cancel=%d body=%s", invalidCancel.Code, invalidCancel.Body.String())
	}

	unconfirmed := httptest.NewRecorder()
	handler.ServeHTTP(unconfirmed, apiRequest(t, http.MethodDelete, "/api/drafts/draft_routes", map[string]any{}))
	if unconfirmed.Code != http.StatusConflict || !strings.Contains(unconfirmed.Body.String(), "confirmation_required") {
		t.Fatalf("unconfirmed=%d body=%s", unconfirmed.Code, unconfirmed.Body.String())
	}
	trashed := httptest.NewRecorder()
	handler.ServeHTTP(trashed, apiRequest(t, http.MethodDelete, "/api/drafts/draft_routes", map[string]any{
		"confirm": true,
	}))
	if trashed.Code != http.StatusOK || !strings.Contains(trashed.Body.String(), `"status":"trashed"`) {
		t.Fatalf("trashed=%d body=%s", trashed.Code, trashed.Body.String())
	}
}

func TestAdvertisedRoutesReturnInternalErrorWhenDatabaseIsUnavailable(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	if err := server.database.Close(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/drafts", map[string]any{}},
		{http.MethodGet, "/api/drafts", nil},
		{http.MethodDelete, "/api/drafts", map[string]any{"draft_ids": []string{"draft_db_error"}, "confirm": true}},
		{http.MethodGet, "/api/drafts/draft_db_error", nil},
		{http.MethodPatch, "/api/drafts/draft_db_error", map[string]any{"name": "x"}},
		{http.MethodDelete, "/api/drafts/draft_db_error", map[string]any{"confirm": true}},
		{http.MethodPost, "/api/drafts/draft_db_error/copy", map[string]any{}},
		{http.MethodGet, "/api/drafts/draft_db_error/costs", nil},
		{http.MethodDelete, "/api/drafts/draft_db_error/materials/asset_db_error", nil},
		{http.MethodPost, "/api/jobs/job_db_error/cancel", map[string]any{}},
	}
	for _, item := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, item.method, item.path, item.body))
		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal_error") {
			t.Fatalf("%s %s status=%d body=%s", item.method, item.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestReadRoutesReturnInternalErrorForCorruptDatabaseSchema(t *testing.T) {
	t.Parallel()
	t.Run("draft material counts", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		createDraftThroughAPI(t, handler, "draft_corrupt_links")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE draft_asset_links"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet, "/api/drafts", nil))
		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal_error") {
			t.Fatalf("list drafts=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
	t.Run("decisions", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		createDraftThroughAPI(t, handler, "draft_corrupt_decisions")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE decisions"); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{
			"/api/drafts/draft_corrupt_decisions/decisions/current",
			"/api/drafts/draft_corrupt_decisions/decisions/pending",
		} {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet, path, nil))
			if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal_error") {
				t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
			}
		}
	})
	t.Run("messages", func(t *testing.T) {
		server, handler := testServer(t, t.TempDir(), 0)
		createDraftThroughAPI(t, handler, "draft_corrupt_messages")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE messages"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet,
			"/api/drafts/draft_corrupt_messages/messages", nil))
		if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "internal_error") {
			t.Fatalf("list messages=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestAdvertisedRouteErrorContracts(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_errors")
	cases := []struct {
		method string
		path   string
		body   any
		status int
		reason string
	}{
		{http.MethodPatch, "/api/drafts/missing", map[string]any{"name": "x"}, 404, "draft_not_found"},
		{http.MethodPatch, "/api/drafts/draft_errors", map[string]any{"name": "   "}, 400, "invalid_name"},
		{http.MethodDelete, "/api/drafts/missing", map[string]any{"confirm": true}, 404, "draft_not_found"},
		{http.MethodPost, "/api/drafts/missing/copy", map[string]any{}, 404, "draft_not_found"},
		{http.MethodPost, "/api/drafts/draft_errors/copy", map[string]any{"draft_id": "draft_errors"}, 409, "draft_already_exists"},
		{http.MethodGet, "/api/drafts/missing/costs", nil, 404, "draft_not_found"},
		{http.MethodDelete, "/api/drafts/draft_errors/materials/missing", nil, 404, "asset_not_linked"},
		{http.MethodPost, "/api/jobs/missing/cancel", map[string]any{}, 404, "job_not_found"},
	}
	for _, item := range cases {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, item.method, item.path, item.body))
		if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.reason) {
			t.Fatalf("%s %s status=%d body=%s", item.method, item.path, recorder.Code, recorder.Body.String())
		}
	}
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodPatch, "/api/drafts/draft_errors"},
		{http.MethodDelete, "/api/drafts/draft_errors"},
		{http.MethodPost, "/api/drafts/draft_errors/copy"},
	} {
		request := apiRequest(t, item.method, item.path, nil)
		request.Body = io.NopCloser(strings.NewReader("{"))
		request.ContentLength = 1
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_json") {
			t.Fatalf("invalid %s %s status=%d body=%s", item.method, item.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestURLMaterialImportRouteIsRemoved(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 0)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
		"/api/drafts/any/materials/import-url", map[string]any{"url": "https://example.com/a.mp4"}))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("removed URL import route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
