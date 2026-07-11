package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) { return 0, errors.New("download interrupted") }
func (failingReadCloser) Close() error             { return nil }

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
		{http.MethodGet, "/api/drafts/draft_db_error", nil},
		{http.MethodPatch, "/api/drafts/draft_db_error", map[string]any{"name": "x"}},
		{http.MethodDelete, "/api/drafts/draft_db_error", map[string]any{"confirm": true}},
		{http.MethodPost, "/api/drafts/draft_db_error/copy", map[string]any{}},
		{http.MethodGet, "/api/drafts/draft_db_error/costs", nil},
		{http.MethodPost, "/api/drafts/draft_db_error/materials/import-url", map[string]any{
			"url": "https://example.com/a.mp4",
		}},
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

func TestURLImportUsesBoundedExplicitClientAndEnqueuesIngest(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	server.urlClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("remote-media")),
			Request:    request,
		}, nil
	})}
	createDraftThroughAPI(t, handler, "draft_url")

	imported := httptest.NewRecorder()
	handler.ServeHTTP(imported, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_url/materials/import-url", map[string]any{
			"url": "https://example.com/clip.mp4", "asset_id": "asset_url", "max_bytes": 1024,
		}))
	if imported.Code != http.StatusOK {
		t.Fatalf("import=%d body=%s", imported.Code, imported.Body.String())
	}
	asset, err := server.database.Read().QueryContext(t.Context(), `
		SELECT source, storage_mode, object_hash FROM assets WHERE asset_id='asset_url'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = asset.Close() }()
	if !asset.Next() {
		t.Fatal("URL 素材未落库")
	}
	var source, mode, hash string
	if err := asset.Scan(&source, &mode, &hash); err != nil || source != "url" || mode != "copy" || len(hash) != 64 {
		t.Fatalf("source=%s mode=%s hash=%s err=%v", source, mode, hash, err)
	}
	customFilename := httptest.NewRecorder()
	handler.ServeHTTP(customFilename, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_url/materials/import-url", map[string]any{
			"url": "https://example.com/download", "filename": "voice.wav",
		}))
	if customFilename.Code != http.StatusOK || !strings.Contains(customFilename.Body.String(), `"asset_id":"asset_`) {
		t.Fatalf("custom filename=%d body=%s", customFilename.Code, customFilename.Body.String())
	}

	tooLarge := httptest.NewRecorder()
	handler.ServeHTTP(tooLarge, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_url/materials/import-url", map[string]any{
			"url": "https://example.com/large.mp4", "asset_id": "asset_large", "max_bytes": 1,
		}))
	if tooLarge.Code != http.StatusRequestEntityTooLarge || !strings.Contains(tooLarge.Body.String(), "url_too_large") {
		t.Fatalf("too large=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_url/materials/import-url", map[string]any{
			"url": "file:///tmp/clip.mp4",
		}))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "invalid_url") {
		t.Fatalf("invalid=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestURLImportSecurityAndFailureContracts(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_url_errors")
	request := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, path, body))
		return recorder
	}
	for _, item := range []struct {
		body   any
		status int
		reason string
	}{
		{map[string]any{"url": "https://user:pass@example.com/a.mp4"}, 400, "invalid_url"},
		{map[string]any{"url": "https://example.com/"}, 400, "missing_filename"},
		{map[string]any{"url": "https://example.com/file.txt"}, 400, "unsupported_material_type"},
		{map[string]any{"url": "https://example.com/a.mp4", "max_bytes": 0}, 400, "invalid_max_bytes"},
	} {
		recorder := request("/api/drafts/draft_url_errors/materials/import-url", item.body)
		if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.reason) {
			t.Fatalf("body=%#v status=%d response=%s", item.body, recorder.Code, recorder.Body.String())
		}
	}
	missing := request("/api/drafts/missing/materials/import-url", map[string]any{
		"url": "https://example.com/a.mp4",
	})
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "draft_not_found") {
		t.Fatalf("missing=%d body=%s", missing.Code, missing.Body.String())
	}
	invalidJSONRequest := apiRequest(t, http.MethodPost,
		"/api/drafts/draft_url_errors/materials/import-url", nil)
	invalidJSONRequest.Body = io.NopCloser(strings.NewReader("{"))
	invalidJSONRequest.ContentLength = 1
	invalidJSON := httptest.NewRecorder()
	handler.ServeHTTP(invalidJSON, invalidJSONRequest)
	if invalidJSON.Code != http.StatusBadRequest || !strings.Contains(invalidJSON.Body.String(), "invalid_json") {
		t.Fatalf("invalid json=%d body=%s", invalidJSON.Code, invalidJSON.Body.String())
	}

	server.urlClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	failed := request("/api/drafts/draft_url_errors/materials/import-url", map[string]any{
		"url": "https://example.com/a.mp4",
	})
	if failed.Code != http.StatusBadGateway || !strings.Contains(failed.Body.String(), "url_download_failed") {
		t.Fatalf("failed=%d body=%s", failed.Code, failed.Body.String())
	}
	server.urlClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("missing")), Request: request,
		}, nil
	})}
	badStatus := request("/api/drafts/draft_url_errors/materials/import-url", map[string]any{
		"url": "https://example.com/a.mp4",
	})
	if badStatus.Code != http.StatusBadGateway || !strings.Contains(badStatus.Body.String(), "url_download_status") {
		t.Fatalf("bad status=%d body=%s", badStatus.Code, badStatus.Body.String())
	}
	server.urlClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: 10,
			Body: io.NopCloser(strings.NewReader("0123456789")), Request: request,
		}, nil
	})}
	declaredLarge := request("/api/drafts/draft_url_errors/materials/import-url", map[string]any{
		"url": "https://example.com/a.mp4", "max_bytes": 1,
	})
	if declaredLarge.Code != http.StatusRequestEntityTooLarge || !strings.Contains(declaredLarge.Body.String(), "url_too_large") {
		t.Fatalf("declared large=%d body=%s", declaredLarge.Code, declaredLarge.Body.String())
	}
	server.urlClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: failingReadCloser{}, Request: request,
		}, nil
	})}
	interrupted := request("/api/drafts/draft_url_errors/materials/import-url", map[string]any{
		"url": "https://example.com/a.mp4",
	})
	if interrupted.Code != http.StatusBadGateway || !strings.Contains(interrupted.Body.String(), "url_download_failed") {
		t.Fatalf("interrupted=%d body=%s", interrupted.Code, interrupted.Body.String())
	}

	client := newURLImportClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialContext == nil || !transport.ForceAttemptHTTP2 {
		t.Fatalf("transport=%#v", client.Transport)
	}
	if _, err := transport.DialContext(t.Context(), "tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("URL importer must reject loopback")
	}
	for _, address := range []string{"0.0.0.0", "127.0.0.1", "10.0.0.1", "169.254.1.1", "224.0.0.1"} {
		if !unsafeImportAddress(net.ParseIP(address)) {
			t.Fatalf("unsafe address accepted: %s", address)
		}
	}
	redirect := client.CheckRedirect
	validRequest, _ := http.NewRequest(http.MethodGet, "https://example.com/a.mp4", nil)
	invalidRequest, _ := http.NewRequest(http.MethodGet, "file:///tmp/a.mp4", nil)
	if err := redirect(validRequest, nil); err != nil || redirect(invalidRequest, nil) == nil ||
		redirect(validRequest, make([]*http.Request, 5)) == nil {
		t.Fatal("redirect policy mismatch")
	}
}
