package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/nanzhi84/Rushes/go/internal/tools"
)

type cancellationBlockingModel struct {
	mu      sync.Mutex
	blocked bool
	started chan struct{}
	release chan struct{}
}

func TestConversationClearVersionConflictAlwaysMapsToTurnActive(t *testing.T) {
	server, _ := testServer(t, t.TempDir(), 0)
	recorder := httptest.NewRecorder()
	server.writeConversationClearReducerResult(recorder, reducer.Result{
		Status: reducer.StatusVersionConflict,
		Conflict: &reducer.VersionConflict{
			DraftID: "draft_idle", ActualStateVersion: 3, EventType: "ConversationContextCleared",
		},
	})
	if recorder.Code != http.StatusConflict ||
		!strings.Contains(recorder.Body.String(), `"reason":"turn_active"`) ||
		strings.Contains(recorder.Body.String(), `"reason":"version_conflict"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func (stub *cancellationBlockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *cancellationBlockingModel) waitFirstCall() {
	stub.mu.Lock()
	block := !stub.blocked
	stub.blocked = true
	stub.mu.Unlock()
	if block {
		close(stub.started)
		<-stub.release // 模拟不响应 context 取消的 provider。
	}
}

func (stub *cancellationBlockingModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.waitFirstCall()
	return schema.AssistantMessage("已完成", nil), nil
}

func (stub *cancellationBlockingModel) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.waitFirstCall()
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("已完成", nil)}), nil
}

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
		"/api/drafts/draft_conversation/turn-stream?token="+testToken+"&turn_stream_client_id=test-client", nil)
	request.Host = "127.0.0.1:8000"
	handler.ServeHTTP(sse, request)
	if sse.Code != http.StatusOK || strings.Count(sse.Body.String(), "event: turn_stream") != 2 ||
		!strings.Contains(sse.Body.String(), `"turn_id":"snapshot"`) {
		t.Fatalf("SSE status=%d body=%s", sse.Code, sse.Body.String())
	}
	missingClient := httptest.NewRecorder()
	missingClientRequest := httptest.NewRequest(http.MethodGet,
		"/api/drafts/draft_conversation/turn-stream?token="+testToken, nil)
	missingClientRequest.Host = "127.0.0.1:8000"
	handler.ServeHTTP(missingClient, missingClientRequest)
	if missingClient.Code != http.StatusBadRequest ||
		!strings.Contains(missingClient.Body.String(), "turn_stream_client_id is required") {
		t.Fatalf("missing client status=%d body=%s", missingClient.Code, missingClient.Body.String())
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

func TestCancelCurrentTurnCancelsAllWaitedJobsWithoutActiveAgentTurn(t *testing.T) {
	t.Parallel()
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_cancel_jobs")
	now := time.Now().UTC()
	events := make([]contracts.Event, 0, 4)
	for _, item := range []struct {
		id, kind string
	}{
		{"job_understand", "understand"},
		{"job_preview", "render_preview"},
		{"job_final", "render_final"},
		{"job_ingest", "ingest"},
	} {
		events = append(events, contracts.Event{
			Type: "JobEnqueued", DraftID: "draft_cancel_jobs", Payload: map[string]any{
				"job_id": item.id, "kind": item.kind, "idempotency_key": item.id,
				"requested_by_draft_id": "draft_cancel_jobs",
				"next_run_at":           now.Format(time.RFC3339Nano),
			},
		})
	}
	result, err := reducer.Apply(t.Context(), server.database, events, reducer.Options{
		Actor: contracts.ActorAgent, CreatedAt: now,
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("enqueue status=%s err=%v", result.Status, err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_cancel_jobs/turn/cancel", nil))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"requested":true`) ||
		!strings.Contains(response.Body.String(), `"status":"requested"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, item := range []struct {
		id, want string
	}{
		{"job_understand", "cancelled"},
		{"job_preview", "cancelled"},
		{"job_final", "cancelled"},
		{"job_ingest", "pending"},
	} {
		var status string
		if err := server.database.Read().QueryRowContext(t.Context(),
			"SELECT status FROM jobs WHERE job_id=?", item.id,
		).Scan(&status); err != nil || status != item.want {
			t.Fatalf("job=%s status=%s want=%s err=%v", item.id, status, item.want, err)
		}
	}
	var cancellationCount int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE event_type='JobCancelled'
		  AND json_extract(payload_json,'$.payload.reason')='turn_cancelled'`,
	).Scan(&cancellationCount); err != nil || cancellationCount != 3 {
		t.Fatalf("turn cancellations=%d err=%v", cancellationCount, err)
	}
}

func TestTurnCancellationCleanupSurvivesRequestCancellation(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, server.Handler(), "draft_cancel_disconnected")
	now := time.Now().UTC()
	result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_cancel_disconnected", Payload: map[string]any{
			"job_id": "job_disconnected", "kind": "understand",
			"idempotency_key":       "job_disconnected",
			"requested_by_draft_id": "draft_cancel_disconnected",
			"next_run_at":           now.Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, CreatedAt: now})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("enqueue status=%s err=%v", result.Status, err)
	}

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	cancelRequest()
	cleanupCtx, cancelCleanup := turnCancellationContext(requestCtx)
	defer cancelCleanup()
	boundary, err := server.turnCancellationJobBoundary(cleanupCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.cancelTurnJobs(cleanupCtx, "draft_cancel_disconnected", boundary); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM jobs WHERE job_id='job_disconnected'",
	).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestTurnCancellationBoundaryProtectsJobsCreatedAfterBarrier(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, server.Handler(), "draft_cancel_boundary")
	now := time.Now().UTC()
	enqueue := func(jobID string, createdAt time.Time) {
		t.Helper()
		result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: "draft_cancel_boundary", Payload: map[string]any{
				"job_id": jobID, "kind": "understand", "idempotency_key": jobID,
				"requested_by_draft_id": "draft_cancel_boundary",
				"next_run_at":           createdAt.Format(time.RFC3339Nano),
			},
		}}, reducer.Options{Actor: contracts.ActorAgent, CreatedAt: createdAt})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("job=%s status=%s err=%v", jobID, result.Status, err)
		}
	}
	enqueue("job_before_boundary", now)
	boundary, err := server.turnCancellationJobBoundary(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.suppressTurnJobObservations(
		t.Context(), "draft_cancel_boundary", boundary,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := server.cancelTurnJobs(t.Context(), "draft_cancel_boundary", boundary); err != nil {
		t.Fatal(err)
	}
	enqueue("job_after_boundary", now.Add(time.Second))
	if _, err := server.cancelTurnJobs(t.Context(), "draft_cancel_boundary", boundary); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct{ jobID, want string }{
		{"job_before_boundary", "cancelled"},
		{"job_after_boundary", "pending"},
	} {
		var status string
		if err := server.database.Read().QueryRowContext(t.Context(),
			"SELECT status FROM jobs WHERE job_id=?", test.jobID,
		).Scan(&status); err != nil || status != test.want {
			t.Fatalf("job=%s status=%s want=%s err=%v", test.jobID, status, test.want, err)
		}
	}
	var suppressedAfter int
	if err := server.database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_job_observation_suppressions WHERE job_id='job_after_boundary'`,
	).Scan(&suppressedAfter); err != nil || suppressedAfter != 0 {
		t.Fatalf("后续 job 不应被抑制: count=%d err=%v", suppressedAfter, err)
	}
	createDraftThroughAPI(t, server.Handler(), "draft_cancel_empty")
	if err := server.suppressTurnJobObservations(t.Context(), "draft_cancel_empty", boundary); err != nil {
		t.Fatalf("空集合 suppression 应静默成功: %v", err)
	}
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := server.suppressTurnJobObservations(cancelledCtx, "draft_cancel_boundary", boundary); err == nil {
		t.Fatal("已取消 context 的 suppression 查询应失败")
	}
	if _, err := server.cancelTurnJobs(cancelledCtx, "draft_cancel_boundary", boundary); err == nil {
		t.Fatal("已取消 context 的 job 查询应失败")
	}
}

func TestCancelCurrentTurnReturnsBoundedAndProtectsLaterJobs(t *testing.T) {
	database, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	blockingModel := &cancellationBlockingModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service, err := agent.NewService(t.Context(), database, blockingModel)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(blockingModel.release) })
		service.Close()
	})
	server, err := NewServer(Config{
		Database: database, Token: testToken, Port: 8000,
		FSRoots: []string{t.TempDir()}, Agent: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	createDraftThroughAPI(t, handler, "draft_bounded_cancel")

	queued := httptest.NewRecorder()
	handler.ServeHTTP(queued, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_bounded_cancel/messages", map[string]any{
			"message_id": "blocked_message", "content": "开始处理",
		}))
	if queued.Code != http.StatusAccepted {
		t.Fatalf("queue status=%d body=%s", queued.Code, queued.Body.String())
	}
	select {
	case <-blockingModel.started:
	case <-time.After(time.Second):
		t.Fatal("阻塞模型未启动")
	}
	for index := range 2 {
		queuedFollowUp := httptest.NewRecorder()
		handler.ServeHTTP(queuedFollowUp, apiRequest(t, http.MethodPost,
			"/api/drafts/draft_bounded_cancel/messages", map[string]any{
				"message_id": fmt.Sprintf("queued_cancelled_%d", index), "content": "也不要继续",
			}))
		if queuedFollowUp.Code != http.StatusAccepted {
			t.Fatalf("queue follow-up %d status=%d body=%s", index, queuedFollowUp.Code, queuedFollowUp.Body.String())
		}
	}
	now := time.Now().UTC()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_bounded_cancel", Payload: map[string]any{
			"job_id": "job_before_cancel", "kind": "understand",
			"idempotency_key": "job_before_cancel", "requested_by_draft_id": "draft_bounded_cancel",
			"next_run_at": now.Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, CreatedAt: now})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("enqueue status=%s err=%v", result.Status, err)
	}

	response := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(response, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_bounded_cancel/turn/cancel", nil))
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("取消端点等待失控: %s", elapsed)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"requested":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var markerRole, markerKind, markerContent string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT role,kind,content FROM messages
		WHERE draft_id='draft_bounded_cancel' ORDER BY rowid DESC LIMIT 1`,
	).Scan(&markerRole, &markerKind, &markerContent); err != nil ||
		markerRole != "system_observation" || markerKind != contracts.TurnCancelledObservationKind ||
		markerContent != contracts.TurnCancelledObservationContent(3) {
		t.Fatalf("取消持久标记 role=%q kind=%q content=%q err=%v", markerRole, markerKind, markerContent, err)
	}
	var status string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM jobs WHERE job_id='job_before_cancel'",
	).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if !service.Queue().EnqueueUserMessage("draft_bounded_cancel", "after_timeout", "继续") {
		t.Fatal("旧 runner 永久阻塞时，新执行代仍应可入队")
	}
	service.Queue().JoinDraft("draft_bounded_cancel")
	later := now.Add(time.Second)
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: "draft_bounded_cancel", Payload: map[string]any{
			"job_id": "job_after_cancel", "kind": "understand",
			"idempotency_key": "job_after_cancel", "requested_by_draft_id": "draft_bounded_cancel",
			"next_run_at": later.Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, CreatedAt: later})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("later enqueue status=%s err=%v", result.Status, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT status FROM jobs WHERE job_id='job_after_cancel'",
	).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("later status=%s err=%v", status, err)
	}
	closed := make(chan struct{})
	go func() {
		service.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("旧 runner 永久阻塞时 Service.Close 不应等待封存代")
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
				Question: "关键目标存在冲突，请选择方案。", DecisionType: "critical",
				AllowFreeText: &test.allowFreeText,
				Options:       []tools.DecisionOptionInput{{OptionID: "known", Label: "已知选项"}},
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
	if _, err := server.agent.ExecuteTool(ctx, "timeline.insert", tools.TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset_keep", "role": "video",
		"source_start_frame": 0, "source_end_frame": 30,
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
	waiting, err := server.agent.ExecuteTool(ctx, "interaction.ask_user", tools.AskUserInput{
		Question: "旧的关键问题？", DecisionType: "critical",
	})
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
			DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
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
		DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
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
			DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
		)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	createDraftThroughAPI(t, handler, "draft_cancelled_stream")
	server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/drafts/draft_cancelled_stream/turn-stream", nil).WithContext(ctx),
		"draft_cancelled_stream",
		DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
	)

	for _, clientID := range []string{"", strings.Repeat("x", 129), "含中文"} {
		invalidClient := httptest.NewRecorder()
		server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
			invalidClient,
			httptest.NewRequest(http.MethodGet, "/api/drafts/draft_turn_live/turn-stream", nil),
			"draft_turn_live",
			DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: clientID},
		)
		if invalidClient.Code != http.StatusBadRequest ||
			!strings.Contains(invalidClient.Body.String(), "invalid_turn_stream_client_id") {
			t.Fatalf("invalid client id length=%d status=%d body=%s", len(clientID), invalidClient.Code, invalidClient.Body.String())
		}
	}

	server.agent.Queue().Close()
	closed := httptest.NewRecorder()
	handler.ServeHTTP(closed, apiRequest(t, http.MethodPost,
		"/api/drafts/draft_turn_live/messages", map[string]any{"content": "closed"}))
	if closed.Code != http.StatusServiceUnavailable || !strings.Contains(closed.Body.String(), "turn_queue_closed") {
		t.Fatalf("closed queue status=%d body=%s", closed.Code, closed.Body.String())
	}
	messages, err := storage.ListMessages(t.Context(), server.database.Read(), "draft_turn_live", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "user" && message.Content == "closed" {
			t.Fatalf("queue preflight failure left an orphan user message: %#v", message)
		}
	}
}

func TestSlowTurnStreamSubscriberReceivesGapAndTerminalThenReconnects(t *testing.T) {
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_slow_stream")
	writer := &blockingTurnStreamWriter{
		header: http.Header{}, started: make(chan struct{}), release: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.DraftEventsApiDraftsDraftIdEventsGet(
			writer,
			httptest.NewRequest(http.MethodGet, "/api/drafts/draft_slow_stream/events", nil),
			"draft_slow_stream",
			DraftEventsApiDraftsDraftIdEventsGetParams{TurnStreamClientId: "test-client"},
		)
	}()
	server.agent.Hub().Record("draft_slow_stream", agent.StreamEvent{"type": agent.TurnStreamTurnStarted})
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not start writing the turn stream")
	}
	for index := range agent.DefaultSubscriberQueueLimit {
		server.agent.Hub().Record("draft_slow_stream", agent.StreamEvent{
			"type": agent.TurnStreamTextDelta, "message_id": "message", "delta": index,
		})
	}
	server.agent.Hub().Record("draft_slow_stream", agent.StreamEvent{
		"type": agent.TurnStreamMessageCompleted, "message_id": "message", "content": "done",
	})
	server.agent.Hub().Record("draft_slow_stream", agent.StreamEvent{
		"type": agent.TurnStreamTurnEnded, "outcome": "finished",
	})
	close(writer.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("slow turn stream connection did not close for EventSource reconnect")
	}
	body := writer.bodyString()
	if !strings.Contains(body, `"type":"stream_gap"`) {
		t.Fatalf("gap frame missing from first SSE body: %s", body)
	}

	server.sseMaxEvents = 2
	reconnected := httptest.NewRecorder()
	server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
		reconnected,
		httptest.NewRequest(http.MethodGet, "/api/drafts/draft_slow_stream/turn-stream", nil),
		"draft_slow_stream",
		DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
	)
	if !strings.Contains(reconnected.Body.String(), `"type":"stream_gap"`) ||
		!strings.Contains(reconnected.Body.String(), `"type":"turn_ended"`) {
		t.Fatalf("terminal recovery snapshot missing: %s", reconnected.Body.String())
	}
	if snapshot := server.agent.Hub().Snapshot("draft_slow_stream"); len(snapshot) != 0 {
		t.Fatalf("成功重放后未确认清理恢复快照: %#v", snapshot)
	}
}

func TestTurnStreamTerminalFlushFailureRetainsRecoverySnapshot(t *testing.T) {
	server, handler := testServer(t, t.TempDir(), 0)
	createDraftThroughAPI(t, handler, "draft_terminal_flush_failure")
	writer := &terminalFailureWriter{
		header: http.Header{}, started: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.DraftEventsApiDraftsDraftIdEventsGet(
			writer,
			httptest.NewRequest(http.MethodGet, "/api/drafts/draft_terminal_flush_failure/events", nil),
			"draft_terminal_flush_failure",
			DraftEventsApiDraftsDraftIdEventsGetParams{TurnStreamClientId: "test-client"},
		)
	}()
	server.agent.Hub().Record("draft_terminal_flush_failure", agent.StreamEvent{
		"type": agent.TurnStreamTurnStarted,
	})
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not flush turn_started")
	}
	server.agent.Hub().Record("draft_terminal_flush_failure", agent.StreamEvent{
		"type": agent.TurnStreamTurnEnded, "outcome": "finished",
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal write failure did not close SSE handler")
	}

	snapshot := server.agent.Hub().Snapshot("draft_terminal_flush_failure")
	if len(snapshot) != 2 || snapshot[0]["type"] != agent.TurnStreamGap ||
		snapshot[1]["type"] != agent.TurnStreamTurnEnded {
		t.Fatalf("terminal flush 失败后恢复快照=%#v", snapshot)
	}
	server.sseMaxEvents = 2
	reconnected := httptest.NewRecorder()
	server.DraftTurnStreamApiDraftsDraftIdTurnStreamGet(
		reconnected,
		httptest.NewRequest(http.MethodGet, "/api/drafts/draft_terminal_flush_failure/turn-stream", nil),
		"draft_terminal_flush_failure",
		DraftTurnStreamApiDraftsDraftIdTurnStreamGetParams{TurnStreamClientId: "test-client"},
	)
	if !strings.Contains(reconnected.Body.String(), `"type":"stream_gap"`) ||
		!strings.Contains(reconnected.Body.String(), `"type":"turn_ended"`) {
		t.Fatalf("terminal flush 失败后重连未恢复: %s", reconnected.Body.String())
	}
	if strings.Contains(reconnected.Body.String(), turnStreamRecoveryGenerationKeyForTest) {
		t.Fatalf("内部 recovery generation 泄漏到 SSE: %s", reconnected.Body.String())
	}
	if remaining := server.agent.Hub().Snapshot("draft_terminal_flush_failure"); len(remaining) != 0 {
		t.Fatalf("恢复快照成功 flush 后未清理: %#v", remaining)
	}
}

const turnStreamRecoveryGenerationKeyForTest = "_recovery_generation"

type turnStreamErrorWriter struct {
	mu       sync.Mutex
	header   http.Header
	status   int
	writeErr error
	flushErr error
}

type terminalFailureWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	started chan struct{}
	once    sync.Once
}

func (writer *terminalFailureWriter) Header() http.Header { return writer.header }

func (writer *terminalFailureWriter) WriteHeader(int) {}

func (writer *terminalFailureWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(`"type":"turn_ended"`)) {
		return 0, errors.New("terminal write failed")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if bytes.Contains(data, []byte(`"type":"turn_started"`)) {
		writer.once.Do(func() { close(writer.started) })
	}
	return writer.body.Write(data)
}

func (writer *terminalFailureWriter) FlushError() error { return nil }

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

type blockingTurnStreamWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockingTurnStreamWriter) Header() http.Header { return writer.header }

func (writer *blockingTurnStreamWriter) WriteHeader(int) {}

func (writer *blockingTurnStreamWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() {
		close(writer.started)
		<-writer.release
	})
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.body.Write(data)
}

func (writer *blockingTurnStreamWriter) FlushError() error { return nil }

func (writer *blockingTurnStreamWriter) bodyString() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.body.String()
}

func storageCurrentDecision(t *testing.T, server *Server, draftID string) (storage.Decision, error) {
	t.Helper()
	return storage.CurrentDecision(t.Context(), server.database.Read(), draftID)
}

func storageCurrentDecisionByID(t *testing.T, server *Server, decisionID string) (storage.Decision, error) {
	t.Helper()
	return storage.GetDecision(t.Context(), server.database.Read(), decisionID)
}
