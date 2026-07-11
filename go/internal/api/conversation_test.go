package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestMessageQueueTurnStreamHistoryAndCancel(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 2)
	createDraftThroughAPI(t, handler, "draft_conversation")

	server.agent.Hub().Record("draft_conversation", map[string]any{"type": "turn_started", "turn_id": "snapshot"})
	server.agent.Hub().Record("draft_conversation", map[string]any{
		"type": "text_delta", "message_id": "snapshot_msg", "delta": "快照",
	})
	sse := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/api/drafts/draft_conversation/turn-stream?token="+testToken, nil)
	request.Host = "127.0.0.1:8000"
	handler.ServeHTTP(sse, request)
	if sse.Code != http.StatusOK || strings.Count(sse.Body.String(), "event: turn_stream") != 2 ||
		!strings.Contains(sse.Body.String(), `"turn_id":"snapshot"`) {
		t.Fatalf("SSE status=%d body=%s", sse.Code, sse.Body.String())
	}

	queued := httptest.NewRecorder()
	handler.ServeHTTP(queued, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_conversation/messages", map[string]any{
			"message_id": "user_message", "content": "把开头三秒删掉",
		}))
	if queued.Code != http.StatusAccepted || !strings.Contains(queued.Body.String(), `"status":"queued"`) {
		t.Fatalf("queued status=%d body=%s", queued.Code, queued.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_conversation")
	history := httptest.NewRecorder()
	handler.ServeHTTP(history, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_conversation/messages?limit=200", nil))
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "把开头三秒删掉") ||
		!strings.Contains(history.Body.String(), "未配置模型密钥") {
		t.Fatalf("history status=%d body=%s", history.Code, history.Body.String())
	}

	cancelIdle := httptest.NewRecorder()
	handler.ServeHTTP(cancelIdle, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_conversation/turn/cancel", nil))
	if cancelIdle.Code != http.StatusOK || !strings.Contains(cancelIdle.Body.String(), `"status":"idle"`) {
		t.Fatalf("cancel idle status=%d body=%s", cancelIdle.Code, cancelIdle.Body.String())
	}
}

func TestDecisionAnswerReplaysPendingToolCall(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_decision")
	ctx := tools.WithDraftID(t.Context(), "draft_decision")
	result, err := server.agent.ExecuteTool(ctx, "interaction.confirm_action", tools.ConfirmActionInput{
		Question: "确认读取素材？", ToolName: "asset.list_assets",
		Arguments: map[string]any{"only_usable": true},
	})
	if err != nil || result == nil {
		t.Fatalf("confirm result=%#v err=%v", result, err)
	}
	decision, err := storageCurrentDecision(t, server, "draft_decision")
	if err != nil || decision.PendingToolCall == nil {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	current := httptest.NewRecorder()
	handler.ServeHTTP(current, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_decision/decisions/current", nil))
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), "确认读取素材") {
		t.Fatalf("current status=%d body=%s", current.Code, current.Body.String())
	}
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, apiRequest(t, http.MethodPost,
		"/api/decisions/"+decision.ID+"/answer", map[string]any{
			"draft_id": "draft_decision",
			"answer":   map[string]any{"answered_via": "button", "option_id": "confirm"},
		}))
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"replays_enqueued":1`) {
		t.Fatalf("answer status=%d body=%s", answer.Code, answer.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_decision")
	stored, err := storageCurrentDecisionByID(t, server, decision.ID)
	if err != nil || stored.Status != "answered" || stored.ReplayedToolCallID == nil ||
		stored.PendingToolCallStatus == nil || *stored.PendingToolCallStatus != "replayed" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	var answered int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log WHERE event_type='DecisionAnswered' AND draft_id='draft_decision'`).Scan(&answered); err != nil || answered != 1 {
		t.Fatalf("answered=%d err=%v", answered, err)
	}
}

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
	cancelled := httptest.NewRecorder()
	handler.ServeHTTP(cancelled, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_cancel_api/turn/cancel", nil))
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"requested":true`) {
		t.Fatalf("cancel=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_cancel_api")
}

func TestTurnStreamLiveCancellationEncodingAndWriterFailures(t *testing.T) {
	server, handler := testServer(t, t.TempDir(), 1)
	createDraftThroughAPI(t, handler, "draft_turn_live")

	live := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
			live,
			httptest.NewRequest(http.MethodGet, "/api/drafts/draft_turn_live/turn-stream", nil),
			"draft_turn_live",
		)
	}()
	time.Sleep(20 * time.Millisecond)
	server.agent.Hub().Record("draft_turn_live", map[string]any{"type": "text_delta", "delta": "实时"})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("实时 turn-stream 未按 maxEvents 退出")
	}
	if !strings.Contains(live.Body.String(), `"delta":"实时"`) {
		t.Fatalf("live body=%s", live.Body.String())
	}

	createDraftThroughAPI(t, handler, "draft_invalid_stream")
	server.agent.Hub().Record("draft_invalid_stream", map[string]any{"type": "text_delta", "bad": make(chan int)})
	invalid := httptest.NewRecorder()
	server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
		invalid,
		httptest.NewRequest(http.MethodGet, "/api/drafts/draft_invalid_stream/turn-stream", nil),
		"draft_invalid_stream",
	)
	if strings.Contains(invalid.Body.String(), "event: turn_stream") {
		t.Fatalf("非法事件不应写入: %s", invalid.Body.String())
	}

	createDraftThroughAPI(t, handler, "draft_write_fail")
	server.agent.Hub().Record("draft_write_fail", map[string]any{"type": "text_delta"})
	for _, writer := range []*turnStreamErrorWriter{
		{header: http.Header{}, writeErr: errors.New("write failed")},
		{header: http.Header{}, flushErr: errors.New("flush failed")},
	} {
		server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
			writer,
			httptest.NewRequest(http.MethodGet, "/api/drafts/draft_write_fail/turn-stream", nil),
			"draft_write_fail",
		)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	createDraftThroughAPI(t, handler, "draft_cancelled_stream")
	server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/drafts/draft_cancelled_stream/turn-stream", nil).WithContext(ctx),
		"draft_cancelled_stream",
	)

	server.agent.Queue().Close()
	closed := httptest.NewRecorder()
	handler.ServeHTTP(closed, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_turn_live/messages", map[string]any{"content": "closed"}))
	if closed.Code != http.StatusServiceUnavailable || !strings.Contains(closed.Body.String(), "turn_queue_closed") {
		t.Fatalf("closed queue status=%d body=%s", closed.Code, closed.Body.String())
	}
}

type turnStreamErrorWriter struct {
	mu       sync.Mutex
	header   http.Header
	status   int
	writeErr error
	flushErr error
}

func (writer *turnStreamErrorWriter) Header() http.Header { return writer.header }

func (writer *turnStreamErrorWriter) WriteHeader(status int) { writer.status = status }

func (writer *turnStreamErrorWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return len(data), nil
}

func (writer *turnStreamErrorWriter) FlushError() error { return writer.flushErr }

func storageCurrentDecision(t *testing.T, server *Server, draftID string) (storage.Decision, error) {
	t.Helper()
	return storage.CurrentDecision(t.Context(), server.database.Read(), draftID)
}

func storageCurrentDecisionByID(t *testing.T, server *Server, decisionID string) (storage.Decision, error) {
	t.Helper()
	return storage.GetDecision(t.Context(), server.database.Read(), decisionID)
}
