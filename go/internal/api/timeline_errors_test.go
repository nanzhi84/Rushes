package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTimelineMutationEndpointsRejectMissingAndInvalidInputs(t *testing.T) {
	t.Parallel()
	_, handler := testServer(t, t.TempDir(), 0)

	for _, item := range []struct {
		path string
		body map[string]any
	}{
		{"/api/drafts/missing/timeline/patch", map[string]any{"op": map[string]any{"kind": "split_clip"}}},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, apiRequest(t, http.MethodPost, item.path, item.body))
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "draft_not_found") {
			t.Fatalf("path=%s status=%d body=%s", item.path, response.Code, response.Body.String())
		}
	}

	createDraftThroughAPI(t, handler, "timeline_errors")
	for _, item := range []struct {
		path string
		body any
	}{
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{}},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "unknown"}}},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "batch", "ops": []any{}}}},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "batch", "ops": []any{map[string]any{}}}}},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, apiRequest(t, http.MethodPost, item.path, item.body))
		if response.Code != http.StatusBadRequest && response.Code != http.StatusNotFound {
			t.Fatalf("path=%s status=%d body=%s", item.path, response.Code, response.Body.String())
		}
	}

	for _, path := range []string{"/api/drafts/timeline_errors/timeline/patch"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{"))
		request.Host = "127.0.0.1:8000"
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
