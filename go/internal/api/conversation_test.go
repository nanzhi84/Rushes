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

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
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

func TestDecisionAnswerRESTValidatesAnswerContent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		answer        map[string]any
		allowFreeText bool
		reason        string
	}{
		{"empty", map[string]any{"answered_via": "button"}, true, "decision_answer_empty"},
		{"unknown option", map[string]any{"answered_via": "button", "option_id": "missing"}, true, "decision_option_not_found"},
		{"free text disabled", map[string]any{"answered_via": "text", "free_text": "自定义"}, false, "decision_free_text_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, handler := testServer(t, t.TempDir(), 0)
			draftID := "draft_rest_" + strings.ReplaceAll(test.name, " ", "_")
			createDraftThroughAPI(t, handler, draftID)
			ctx := tools.WithDraftID(t.Context(), draftID)
			result, err := server.agent.ExecuteTool(ctx, "interaction.ask_user", tools.AskUserInput{
				Question: "选择方案？", AllowFreeText: &test.allowFreeText,
				Options: []tools.DecisionOptionInput{{OptionID: "known", Label: "已知选项"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			decisionID := result.(tools.ToolResult).Data["decision_id"].(string)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, apiRequest(t, http.MethodPost,
				"/api/decisions/"+decisionID+"/answer", map[string]any{
					"draft_id": draftID, "answer": test.answer,
				}))
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.reason) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			stored, err := storageCurrentDecisionByID(t, server, decisionID)
			if err != nil || stored.Status != "pending" {
				t.Fatalf("invalid REST answer changed decision: %#v err=%v", stored, err)
			}
		})
	}
}

func TestClearConversationHidesHistoryAndPreservesObjectiveState(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_clear_context")
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_keep", "job_id": "job_asset_keep", "kind": "video",
			"filename": "keep.mp4", "probe": map[string]any{"duration_sec": 1},
			"ingest_status": "ready", "understanding_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_clear_context", Payload: map[string]any{"asset_id": "asset_keep"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset setup=%#v err=%v", result, err)
	}
	ctx := tools.WithDraftID(t.Context(), "draft_clear_context")
	if _, err := server.agent.ExecuteTool(ctx, "timeline.compose_initial", tools.ComposeInitialInput{
		Clips: []tools.ComposeClip{{
			AssetID: "asset_keep", SourceStartFrame: 0, SourceEndFrame: 30, Role: "video",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	old := httptest.NewRecorder()
	handler.ServeHTTP(old, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_clear_context/messages", map[string]any{"content": "旧对话内容"}))
	if old.Code != http.StatusAccepted {
		t.Fatalf("old=%d body=%s", old.Code, old.Body.String())
	}
	server.agent.Queue().JoinDraft("draft_clear_context")
	waiting, err := server.agent.ExecuteTool(ctx, "interaction.ask_user", tools.AskUserInput{Question: "旧问题？"})
	if err != nil {
		t.Fatal(err)
	}
	decisionID := waiting.(tools.ToolResult).Data["decision_id"].(string)
	var rawBefore int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE draft_id='draft_clear_context'`).Scan(&rawBefore); err != nil {
		t.Fatal(err)
	}

	cleared := httptest.NewRecorder()
	handler.ServeHTTP(cleared, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_clear_context/conversation/clear", nil))
	if cleared.Code != http.StatusOK || !strings.Contains(cleared.Body.String(), `"status":"cleared"`) ||
		!strings.Contains(cleared.Body.String(), `"timeline"`) {
		t.Fatalf("clear=%d body=%s", cleared.Code, cleared.Body.String())
	}
	draft, err := storage.GetDraft(t.Context(), server.database.Read(), "draft_clear_context")
	if err != nil || draft.MessagesTailRef == nil || draft.TimelineCurrentVersion == nil ||
		*draft.TimelineCurrentVersion != 1 || !draft.TimelineValidated {
		t.Fatalf("draft=%#v err=%v", draft, err)
	}
	decision, err := storage.GetDecision(t.Context(), server.database.Read(), decisionID)
	if err != nil || decision.Status != "cancelled" {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	var linked, rawAfter int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM draft_asset_links WHERE draft_id='draft_clear_context'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM messages WHERE draft_id='draft_clear_context'`).Scan(&rawAfter); err != nil {
		t.Fatal(err)
	}
	if linked != 1 || rawAfter != rawBefore+1 {
		t.Fatalf("linked=%d rawBefore=%d rawAfter=%d", linked, rawBefore, rawAfter)
	}
	history := httptest.NewRecorder()
	handler.ServeHTTP(history, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_clear_context/messages?limit=200", nil))
	if history.Code != http.StatusOK || strings.Contains(history.Body.String(), "旧对话内容") ||
		!strings.Contains(history.Body.String(), "素材理解、时间线和预览均已保留") {
		t.Fatalf("history=%d body=%s", history.Code, history.Body.String())
	}
	current := httptest.NewRecorder()
	handler.ServeHTTP(current, apiRequest(t, http.MethodGet,
		"/api/drafts/draft_clear_context/decisions/current", nil))
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), `"decision":null`) {
		t.Fatalf("current=%d body=%s", current.Code, current.Body.String())
	}

	newMessage := httptest.NewRecorder()
	handler.ServeHTTP(newMessage, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_clear_context/messages", map[string]any{"content": "继续优化现有时间线"}))
	server.agent.Queue().JoinDraft("draft_clear_context")
	visible, err := storage.ListMessages(t.Context(), server.database.Read(), "draft_clear_context", 20)
	if err != nil || len(visible) != 3 || visible[0].Kind != "context_reset" ||
		visible[1].Content != "继续优化现有时间线" {
		t.Fatalf("visible=%#v err=%v", visible, err)
	}
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
