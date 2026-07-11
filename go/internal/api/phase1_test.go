package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

const testToken = "test-token"

func testServer(t *testing.T, root string, maxEvents int) (*Server, http.Handler) {
	t.Helper()
	database, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	server, err := NewServer(Config{
		Database: database, Token: testToken, Port: 8000, FSRoots: []string{root},
		SSEMaxEvents: maxEvents,
		Picker: func(_ context.Context, mode string) ([]string, bool) {
			return []string{"/tmp/" + mode}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server, server.Handler()
}

func apiRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "127.0.0.1:8000"
	request.Header.Set("Authorization", "Bearer "+testToken)
	if body != nil || isMutation(method) {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestSecurityBaseline(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 0)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		status int
		reason string
	}{
		{"missing token", func(r *http.Request) { r.Header.Del("Authorization") }, 401, "missing_token"},
		{"bad token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer no") }, 401, "bad_token"},
		{"host", func(r *http.Request) { r.Host = "localhost:8000" }, 403, "host_mismatch"},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "http://localhost:8000") }, 403, "origin_mismatch"},
		{"content type", func(r *http.Request) { r.Header.Del("Content-Type") }, 415, "bad_content_type"},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			request := apiRequest(t, http.MethodPost, "/api/drafts", map[string]any{})
			item.mutate(request)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.reason) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDraftVerticalSliceAndWorkspaceSSE(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 1)
	create := apiRequest(t, http.MethodPost, "/api/drafts", map[string]any{
		"draft_id": "draft-api", "name": "Go 草稿", "goal": "剪 30 秒",
	})
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	for _, path := range []string{"/api/drafts", "/api/drafts/draft-api"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Go 草稿") {
			t.Fatalf("GET %s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}

	sse := httptest.NewRequest(http.MethodGet, "/api/events?token="+testToken, nil)
	sse.Host = "127.0.0.1:8000"
	sseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(sseRecorder, sse)
	body := sseRecorder.Body.String()
	if sseRecorder.Code != http.StatusOK ||
		!strings.Contains(body, "event: DraftCreated") ||
		!strings.Contains(body, `"draft_id":"draft-api"`) {
		t.Fatalf("SSE status=%d body=%s", sseRecorder.Code, body)
	}
}

func TestFSRootsListEscapeAndPicker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, handler := testServer(t, root, 0)

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, apiRequest(t, http.MethodGet, "/api/fs/list?path="+url.QueryEscape(root), nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "clip.mp4") || strings.Contains(list.Body.String(), "ignore.txt") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	escape := httptest.NewRecorder()
	handler.ServeHTTP(escape, apiRequest(t, http.MethodGet, "/api/fs/list?path=%2F", nil))
	if escape.Code != http.StatusForbidden || !strings.Contains(escape.Body.String(), "path_escape") {
		t.Fatalf("escape status=%d body=%s", escape.Code, escape.Body.String())
	}
	pick := httptest.NewRecorder()
	handler.ServeHTTP(pick, apiRequest(t, http.MethodPost, "/api/fs/pick", map[string]any{"mode": "folder"}))
	if pick.Code != http.StatusOK || !strings.Contains(pick.Body.String(), "/tmp/folder") {
		t.Fatalf("pick status=%d body=%s", pick.Code, pick.Body.String())
	}
}
