//go:build e2e_scaffold

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCancelEndpointStopsActiveTurn(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_cancel_api")
	_, stream, unsubscribe := server.agent.Hub().Subscribe("draft_cancel_api")
	defer unsubscribe()
	queued := httptest.NewRecorder()
	handler.ServeHTTP(queued, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_cancel_api/messages", map[string]any{"content": "E2E_BLOCK_UNTIL_CANCEL"}))
	if queued.Code != http.StatusAccepted {
		t.Fatalf("queued=%d body=%s", queued.Code, queued.Body.String())
	}
	select {
	case <-stream:
	case <-time.After(time.Second):
		t.Fatal("turn 未启动")
	}
	busyClear := httptest.NewRecorder()
	handler.ServeHTTP(busyClear, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_cancel_api/conversation/clear", nil))
	if busyClear.Code != http.StatusConflict || !strings.Contains(busyClear.Body.String(), "turn_active") {
		t.Fatalf("busy clear=%d body=%s", busyClear.Code, busyClear.Body.String())
	}
	cancelled := httptest.NewRecorder()
	handler.ServeHTTP(cancelled, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_cancel_api/turn/cancel", nil))
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"requested":true`) {
		t.Fatalf("cancel=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_cancel_api")
}
