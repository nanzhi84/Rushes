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
	// 手动 REST 补丁入口对结构非法输入必须 400 拒绝并回明确错误码：空 body / 未知 kind
	// 命中前置守卫，空 ops、含空 op 的 batch 命中 timelinePatchOperations 展开守卫，
	// 全部在 apply_patches 执行前返回 timeline_patch_invalid（既不是 404，也不进入校验路径）。
	for _, item := range []struct {
		path       string
		body       any
		wantReason string
	}{
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{}, "timeline_patch_invalid"},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "unknown"}}, "timeline_patch_invalid"},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "batch", "ops": []any{}}}, "timeline_patch_invalid"},
		{"/api/drafts/timeline_errors/timeline/patch", map[string]any{"op": map[string]any{"kind": "batch", "ops": []any{map[string]any{}}}}, "timeline_patch_invalid"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, apiRequest(t, http.MethodPost, item.path, item.body))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), item.wantReason) {
			t.Fatalf("path=%s status=%d body=%s want reason=%s", item.path, response.Code, response.Body.String(), item.wantReason)
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
