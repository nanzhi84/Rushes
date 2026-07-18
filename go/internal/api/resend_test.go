package api

import (
	"context"
	"encoding/json"
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
)

// rewindBlockingModel 的 Generate 忽略 ctx 取消，用来模拟“卡住不响应取消”的在途回合，
// 触发排空屏障的 500ms 超时路径。
type rewindBlockingModel struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (stub *rewindBlockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *rewindBlockingModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	stub.once.Do(func() { close(stub.started) })
	<-stub.release
	return schema.AssistantMessage("迟到回复", nil), nil
}

func (stub *rewindBlockingModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

// resendBlockedModel 的 Generate 阻塞但尊重 ctx，用于 resend 成功路径：新回合入队后
// 停在模型处，断言期间不会异步落库；清理时可 unblock 或由 ctx 取消收尾，不会挂起。
type resendBlockedModel struct {
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (stub *resendBlockedModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *resendBlockedModel) unblock() { stub.releaseOnce.Do(func() { close(stub.release) }) }

func (stub *resendBlockedModel) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	stub.startOnce.Do(func() { close(stub.started) })
	select {
	case <-stub.release:
		return schema.AssistantMessage("已完成", nil), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (stub *resendBlockedModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func resendTestServer(t *testing.T) (*Server, http.Handler, *resendBlockedModel) {
	t.Helper()
	database, err := storage.Open(t.Context(), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	blocking := &resendBlockedModel{started: make(chan struct{}), release: make(chan struct{})}
	agentService, err := agent.NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{
		Database: database, Agent: agentService, Token: testToken, Port: 8000,
		FSRoots: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	t.Cleanup(func() { blocking.unblock(); agentService.Close() })
	t.Cleanup(server.Close)
	return server, server.Handler(), blocking
}

func resendPath(draftID, messageID string) string {
	return "/api/drafts/" + draftID + "/messages/" + messageID + "/resend"
}

func TestResendRewindsBranchEnqueuesTurnAndIsIdempotent(t *testing.T) {
	t.Parallel()
	server, handler, blocking := resendTestServer(t)
	draftID := "draft-resend-api"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIResendMessage(t, server.database, draftID, "api-user-1", "保留第一版")
	createAPIResendTimeline(t, server.database, draftID, 1, "api-clip-1", 30)
	insertAPIResendMessage(t, server.database, draftID, "api-user-2", "制作第二版")
	createAPIResendTimeline(t, server.database, draftID, 2, "api-clip-2", 90)
	insertAPIResendMessage(t, server.database, draftID, "api-user-3", "制作第三版")
	seedPendingDecisionAndJob(t, server.database, draftID)

	resend := httptest.NewRecorder()
	handler.ServeHTTP(resend, apiRequest(t, http.MethodPost, resendPath(draftID, "api-user-2"),
		map[string]any{"content": "第二版改写", "idempotency_key": "resend-1"}))
	if resend.Code != http.StatusAccepted {
		t.Fatalf("resend status=%d body=%s", resend.Code, resend.Body.String())
	}
	var response MessageResendResponse
	if err := json.Unmarshal(resend.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.MessageId == "" || response.Status != Resent ||
		response.RestoredTimelineVersion == nil || *response.RestoredTimelineVersion != 3 ||
		response.RewoundMessageCount != 2 {
		t.Fatalf("resend response=%#v", response)
	}
	// 202 即代表新回合已入队；等回合真正进入模型确认串接生效。
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("resend did not enqueue a new turn")
	}

	// 时间线回到 v1（新版本 v3，父 v1）。
	var parent string
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT CAST(parent_version AS TEXT) FROM timeline_versions WHERE draft_id=? AND version=3", draftID,
	).Scan(&parent); err != nil || parent != "1" {
		t.Fatalf("parent=%q err=%v", parent, err)
	}
	// api-user-2 与其后的 api-user-3 被遮蔽；api-user-1 与新的 X′ 可见。
	visible, err := storage.ListMessages(t.Context(), server.database.Read(), draftID, 50)
	if err != nil {
		t.Fatal(err)
	}
	visibleIDs := messageIDs(visible)
	if strings.Join(visibleIDs, ",") != "api-user-1,"+response.MessageId {
		t.Fatalf("visible=%v new=%s", visibleIDs, response.MessageId)
	}
	assertMessageShadowed(t, server.database, "api-user-2", true)
	assertMessageShadowed(t, server.database, "api-user-3", true)
	assertMessageContent(t, server.database, response.MessageId, "第二版改写")
	// 失效边界后的 pending 决策/可取消 job 被取消。
	assertRowStatus(t, server.database, "SELECT status FROM jobs WHERE job_id='resend-pending-job'", "cancelled")
	assertRowStatus(t, server.database, "SELECT status FROM decisions WHERE decision_id='resend-pending-decision'", "cancelled")

	// 幂等重放：同 key 同内容返回相同结果，不产生第二次回退或重复消息。
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, apiRequest(t, http.MethodPost, resendPath(draftID, "api-user-2"),
		map[string]any{"content": "第二版改写", "idempotency_key": "resend-1"}))
	if retry.Code != http.StatusAccepted || retry.Body.String() != resend.Body.String() {
		t.Fatalf("retry status=%d body=%s first=%s", retry.Code, retry.Body.String(), resend.Body.String())
	}
	var restoreEvents int
	if err := server.database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionRestored'", draftID,
	).Scan(&restoreEvents); err != nil || restoreEvents != 1 {
		t.Fatalf("restore events=%d err=%v", restoreEvents, err)
	}

	// 同 key 不同内容 → 409。
	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, apiRequest(t, http.MethodPost, resendPath(draftID, "api-user-2"),
		map[string]any{"content": "换了内容", "idempotency_key": "resend-1"}))
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "resend_idempotency_key_reused") {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// 直接调用 applyResend 复用同 key → ErrRewindRestoreDuplicate。
	checkpoint, err := storage.GetRewindCheckpoint(t.Context(), server.database.Read(), draftID, "rewind:message:api-user-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.applyResend(
		t.Context(), draftID, checkpoint, newID("msg"), "再来一次", "resend-1", nil,
	); !errors.Is(err, reducer.ErrRewindRestoreDuplicate) {
		t.Fatalf("direct key reuse err=%v", err)
	}
}

func TestResendEarlyMessageRewindsConversationOnly(t *testing.T) {
	t.Parallel()
	server, handler, _ := resendTestServer(t)
	draftID := "draft-resend-early"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIResendMessage(t, server.database, draftID, "early-1", "还没有时间线")

	resend := httptest.NewRecorder()
	handler.ServeHTTP(resend, apiRequest(t, http.MethodPost, resendPath(draftID, "early-1"),
		map[string]any{"content": "改写首条", "idempotency_key": "resend-early"}))
	if resend.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", resend.Code, resend.Body.String())
	}
	var response MessageResendResponse
	if err := json.Unmarshal(resend.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.RestoredTimelineVersion != nil || response.RewoundMessageCount != 1 {
		t.Fatalf("early resend response=%#v", response)
	}
	// 无时间线时不产生 TimelineVersionRestored 的时间线恢复，只软遮蔽会话。
	assertMessageShadowed(t, server.database, "early-1", true)
	visible, _ := storage.ListMessages(t.Context(), server.database.Read(), draftID, 50)
	if strings.Join(messageIDs(visible), ",") != response.MessageId {
		t.Fatalf("visible=%v new=%s", messageIDs(visible), response.MessageId)
	}
}

func TestResendRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	server, handler, _ := resendTestServer(t)
	draftID := "draft-resend-errors"
	createDraftThroughAPI(t, handler, draftID)
	insertAPIResendMessage(t, server.database, draftID, "err-user-1", "第一条")

	tests := []struct {
		name    string
		draftID string
		message string
		body    any
		status  int
		reason  string
	}{
		{"missing draft", "missing", "err-user-1", map[string]any{"content": "x", "idempotency_key": "k"}, 404, "draft_not_found"},
		{"empty content", draftID, "err-user-1", map[string]any{"content": "   ", "idempotency_key": "k1"}, 400, "empty_message"},
		{"missing key", draftID, "err-user-1", map[string]any{"content": "x"}, 400, "resend_request_invalid"},
		{"oversized key", draftID, "err-user-1", map[string]any{"content": "x", "idempotency_key": strings.Repeat("k", 129)}, 400, "resend_request_invalid"},
		{"missing checkpoint", draftID, "ghost", map[string]any{"content": "x", "idempotency_key": "k2"}, 404, "resend_checkpoint_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost,
				resendPath(test.draftID, test.message), test.body))
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.reason) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// 已被遮蔽的消息不可再次编辑重发。
	resend := httptest.NewRecorder()
	handler.ServeHTTP(resend, apiRequest(t, http.MethodPost, resendPath(draftID, "err-user-1"),
		map[string]any{"content": "改写", "idempotency_key": "shadow-1"}))
	if resend.Code != http.StatusAccepted {
		t.Fatalf("first resend status=%d body=%s", resend.Code, resend.Body.String())
	}
	shadowed := httptest.NewRecorder()
	handler.ServeHTTP(shadowed, apiRequest(t, http.MethodPost, resendPath(draftID, "err-user-1"),
		map[string]any{"content": "再改", "idempotency_key": "shadow-2"}))
	if shadowed.Code != http.StatusConflict || !strings.Contains(shadowed.Body.String(), "resend_message_not_editable") {
		t.Fatalf("shadowed status=%d body=%s", shadowed.Code, shadowed.Body.String())
	}

	// 检查点尚在但锚点消息已不存在（如被上游清理）→ 404。
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_checkpoints(
			checkpoint_id,draft_id,trigger_kind,decision_boundary,job_boundary,
			summary,clip_count,duration_frames,track_count,created_at
		) VALUES('rewind:message:ghost',?,'user_message',0,0,'幽灵',0,0,0,'now')`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	ghost := httptest.NewRecorder()
	handler.ServeHTTP(ghost, apiRequest(t, http.MethodPost, resendPath(draftID, "ghost"),
		map[string]any{"content": "x", "idempotency_key": "ghost-key"}))
	if ghost.Code != http.StatusNotFound || !strings.Contains(ghost.Body.String(), "resend_message_not_found") {
		t.Fatalf("ghost status=%d body=%s", ghost.Code, ghost.Body.String())
	}

	// 历史遗留的幂等结果（new_message_id 为 NULL）重放时按参数不一致处理 → 409。
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_restore_requests(
			draft_id,idempotency_key,checkpoint_id,mode,rewound_message_count,
			cancelled_jobs,cancelled_decisions,event_ids_json,created_at
		) VALUES(?,'historical-key','rewind:message:err-user-1','conversation',0,0,0,'[]','now')`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	historical := httptest.NewRecorder()
	handler.ServeHTTP(historical, apiRequest(t, http.MethodPost, resendPath(draftID, "err-user-1"),
		map[string]any{"content": "x", "idempotency_key": "historical-key"}))
	if historical.Code != http.StatusConflict || !strings.Contains(historical.Body.String(), "resend_idempotency_key_reused") {
		t.Fatalf("historical status=%d body=%s", historical.Code, historical.Body.String())
	}

	// 幂等结果指向的 X′ 已不存在时，内容比对判为不一致 → 409。
	if _, err := server.database.Write().ExecContext(t.Context(), `
		INSERT INTO rewind_restore_requests(
			draft_id,idempotency_key,checkpoint_id,mode,new_message_id,rewound_message_count,
			cancelled_jobs,cancelled_decisions,event_ids_json,created_at
		) VALUES(?,'dangling-key','rewind:message:err-user-1','conversation','vanished-msg',0,0,0,'[]','now')`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	dangling := httptest.NewRecorder()
	handler.ServeHTTP(dangling, apiRequest(t, http.MethodPost, resendPath(draftID, "err-user-1"),
		map[string]any{"content": "x", "idempotency_key": "dangling-key"}))
	if dangling.Code != http.StatusConflict || !strings.Contains(dangling.Body.String(), "resend_idempotency_key_reused") {
		t.Fatalf("dangling status=%d body=%s", dangling.Code, dangling.Body.String())
	}
}

func TestResendReturnsStableErrorsForUnavailableRuntimeState(t *testing.T) {
	t.Parallel()
	t.Run("storage closed", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-closed"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "closed-anchor", "锚点")
		if err := server.database.Close(); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "closed-anchor"),
			map[string]any{"content": "x", "idempotency_key": "closed"}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("restore request table unavailable", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-request-table"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "request-anchor", "锚点")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE rewind_restore_requests"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "request-anchor"),
			map[string]any{"content": "x", "idempotency_key": "request-table"}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("checkpoint table unavailable", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-checkpoint-table"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "cp-anchor", "锚点")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE rewind_checkpoints"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "cp-anchor"),
			map[string]any{"content": "x", "idempotency_key": "cp-table"}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("turn queue closed", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-queue-closed"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "queue-anchor", "锚点")
		server.agent.Queue().Close()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "queue-anchor"),
			map[string]any{"content": "x", "idempotency_key": "queue-closed"}))
		if recorder.Code != http.StatusServiceUnavailable ||
			!strings.Contains(recorder.Body.String(), "turn_queue_closed") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("jobs table unavailable", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-jobs"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "jobs-anchor", "锚点")
		if _, err := server.database.Write().ExecContext(t.Context(), "DROP TABLE jobs"); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "jobs-anchor"),
			map[string]any{"content": "x", "idempotency_key": "jobs-error"}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("corrupt timeline snapshot", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-corrupt"
		createDraftThroughAPI(t, handler, draftID)
		createAPIResendTimeline(t, server.database, draftID, 1, "corrupt-clip", 30)
		insertAPIResendMessage(t, server.database, draftID, "corrupt-anchor", "锚点")
		if _, err := server.database.Write().ExecContext(t.Context(),
			"UPDATE timeline_versions SET document_json='{' WHERE draft_id=? AND version=1", draftID,
		); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "corrupt-anchor"),
			map[string]any{"content": "x", "idempotency_key": "corrupt"}))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("job state changes during atomic resend", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-job-race"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "race-anchor", "锚点")
		draft, _ := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		if result, err := reducer.Apply(t.Context(), server.database, []contracts.Event{{
			Type: "JobEnqueued", DraftID: draftID,
			Payload: map[string]any{
				"job_id": "resend-racing-job", "kind": "render_preview", "requested_by_draft_id": draftID,
				"next_run_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			},
		}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draft.StateVersion}); err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("job result=%#v err=%v", result, err)
		}
		if _, err := server.database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER change_resend_job_state
			AFTER INSERT ON rewind_restore_requests
			BEGIN UPDATE jobs SET status='succeeded' WHERE job_id='resend-racing-job'; END`); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "race-anchor"),
			map[string]any{"content": "x", "idempotency_key": "job-race"}))
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "resend_job_state_changed") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		assertRowStatus(t, server.database, "SELECT status FROM jobs WHERE job_id='resend-racing-job'", "pending")
	})

	t.Run("draft version changes during atomic resend", func(t *testing.T) {
		server, handler, _ := resendTestServer(t)
		draftID := "draft-resend-version-race"
		createDraftThroughAPI(t, handler, draftID)
		insertAPIResendMessage(t, server.database, draftID, "version-anchor", "锚点")
		before, _ := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		if _, err := server.database.Write().ExecContext(t.Context(), `
			CREATE TRIGGER change_resend_draft_version
			AFTER INSERT ON rewind_restore_requests
			BEGIN UPDATE drafts SET state_version=state_version+1 WHERE draft_id='draft-resend-version-race'; END`); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, apiRequest(t, http.MethodPost, resendPath(draftID, "version-anchor"),
			map[string]any{"content": "x", "idempotency_key": "version-race"}))
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), string(reducer.StatusVersionConflict)) {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		after, _ := storage.GetDraft(t.Context(), server.database.Read(), draftID)
		if after.StateVersion != before.StateVersion {
			t.Fatalf("failed resend must roll back version-race simulation, version=%d", after.StateVersion)
		}
	})

	failure := &rewindReducerResultError{result: reducer.Result{Status: reducer.StatusVersionConflict}}
	if !strings.Contains(failure.Error(), string(reducer.StatusVersionConflict)) {
		t.Fatalf("error=%q", failure.Error())
	}
	server, _, _ := resendTestServer(t)
	if _, err := server.applyResend(
		t.Context(), "missing-draft", storage.RewindCheckpoint{}, newID("msg"), "x", "missing", nil,
	); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing draft err=%v", err)
	}
}

func TestResendDoesNotCommitWhenTurnCancellationTimesOut(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	blocking := &rewindBlockingModel{started: make(chan struct{}), release: make(chan struct{})}
	agentService, err := agent.NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agentService.Close)
	server, err := NewServer(Config{
		Database: database, Agent: agentService, Token: testToken, Port: 8000,
		FSRoots: []string{t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	draftID := "draft-resend-timeout"
	createDraftThroughAPI(t, server.Handler(), draftID)
	insertAPIResendMessage(t, database, draftID, "timeout-anchor", "锚点")
	if !agentService.Queue().EnqueueUserMessage(draftID, "timeout-anchor", "阻塞") {
		t.Fatal("turn enqueue failed")
	}
	<-blocking.started

	body := map[string]any{"content": "改写重发", "idempotency_key": "timeout-request"}
	timeout := httptest.NewRecorder()
	server.Handler().ServeHTTP(timeout, apiRequest(t, http.MethodPost, resendPath(draftID, "timeout-anchor"), body))
	if timeout.Code != http.StatusConflict || !strings.Contains(timeout.Body.String(), "resend_cancellation_timeout") {
		t.Fatalf("status=%d body=%s", timeout.Code, timeout.Body.String())
	}
	drainingRetry := httptest.NewRecorder()
	server.Handler().ServeHTTP(drainingRetry, apiRequest(t, http.MethodPost, resendPath(draftID, "timeout-anchor"), body))
	if drainingRetry.Code != http.StatusConflict || !strings.Contains(drainingRetry.Body.String(), "resend_cancellation_timeout") {
		t.Fatalf("draining retry status=%d body=%s", drainingRetry.Code, drainingRetry.Body.String())
	}
	// 超时路径不得提交任何回退或幂等记录。
	var restores, requests int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM event_log WHERE draft_id=? AND event_type='TimelineVersionRestored'", draftID,
	).Scan(&restores); err != nil || restores != 0 {
		t.Fatalf("restore events=%d err=%v", restores, err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM rewind_restore_requests WHERE draft_id=?", draftID,
	).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("restore requests=%d err=%v", requests, err)
	}

	close(blocking.release)
	agentService.Queue().JoinDraft(draftID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		retry := httptest.NewRecorder()
		server.Handler().ServeHTTP(retry, apiRequest(t, http.MethodPost, resendPath(draftID, "timeout-anchor"), body))
		if retry.Code == http.StatusAccepted {
			break
		}
		if retry.Code != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("drained retry status=%d body=%s", retry.Code, retry.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	assertMessageShadowed(t, database, "timeout-anchor", true)
}

func insertAPIResendMessage(
	t *testing.T,
	database *storage.DB,
	draftID string,
	messageID string,
	content string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("message result=%#v err=%v", result, err)
	}
}

func createAPIResendTimeline(
	t *testing.T,
	database *storage.DB,
	draftID string,
	version int,
	clipID string,
	duration int,
) {
	t.Helper()
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionCreated", DraftID: draftID,
		Payload: map[string]any{
			"timeline_id": fmt.Sprintf("%s:v%d", draftID, version), "timeline_version": version,
			"patch_id": fmt.Sprintf("api-patch-%d", version),
			"document_json": map[string]any{
				"timeline_id": fmt.Sprintf("%s:v%d", draftID, version), "draft_id": draftID,
				"version": version, "fps": 30, "duration_frames": duration,
				"tracks": []any{map[string]any{
					"track_id": "visual_base", "track_type": "video",
					"clips": []any{map[string]any{"timeline_clip_id": clipID}},
				}},
			},
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("timeline result=%#v err=%v", result, err)
	}
}

func seedPendingDecisionAndJob(t *testing.T, database *storage.DB, draftID string) {
	t.Helper()
	draft, _ := storage.GetDraft(t.Context(), database.Read(), draftID)
	base := draft.StateVersion
	if result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": "resend-pending-decision", "scope_type": "draft", "type": "generic",
			"question": "继续？", "options": []any{}, "allow_free_text": true, "blocking": true,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("decision result=%#v err=%v", result, err)
	}
	draft, _ = storage.GetDraft(t.Context(), database.Read(), draftID)
	base = draft.StateVersion
	if result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": "resend-pending-job", "kind": "render_preview", "requested_by_draft_id": draftID,
			"next_run_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &base}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("job result=%#v err=%v", result, err)
	}
}

func messageIDs(messages []storage.Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func assertMessageShadowed(t *testing.T, database *storage.DB, messageID string, shadowed bool) {
	t.Helper()
	var rewoundAt *string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT rewound_at FROM messages WHERE message_id=?", messageID,
	).Scan(&rewoundAt); err != nil {
		t.Fatal(err)
	}
	if (rewoundAt != nil) != shadowed {
		t.Fatalf("message %s rewound_at=%v want shadowed=%v", messageID, rewoundAt, shadowed)
	}
}

func assertMessageContent(t *testing.T, database *storage.DB, messageID, content string) {
	t.Helper()
	var stored string
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT content FROM messages WHERE message_id=?", messageID,
	).Scan(&stored); err != nil || stored != content {
		t.Fatalf("message %s content=%q want=%q err=%v", messageID, stored, content, err)
	}
}

func assertRowStatus(t *testing.T, database *storage.DB, query, want string) {
	t.Helper()
	var status string
	if err := database.Read().QueryRowContext(t.Context(), query).Scan(&status); err != nil || status != want {
		t.Fatalf("query=%q status=%q want=%q err=%v", query, status, want, err)
	}
}
