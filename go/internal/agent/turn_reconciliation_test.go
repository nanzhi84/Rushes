package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type reconciliationBlockingModel struct {
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newReconciliationBlockingModel() *reconciliationBlockingModel {
	return &reconciliationBlockingModel{started: make(chan struct{}), release: make(chan struct{})}
}

func (stub *reconciliationBlockingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *reconciliationBlockingModel) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	stub.startOnce.Do(func() { close(stub.started) })
	select {
	case <-stub.release:
		return schema.AssistantMessage("启动对账已续跑", nil), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (stub *reconciliationBlockingModel) Stream(
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

func (stub *reconciliationBlockingModel) unblock() {
	stub.releaseOnce.Do(func() { close(stub.release) })
}

func TestPersistedTurnCandidatesPairFIFOAcrossOverlappingUserTurns(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_fifo_users"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	for index, row := range []reducer.MessageRow{
		{ID: "user_fifo_1", DraftID: draftID, Role: "user", Kind: "user", Content: "先做第一件事"},
		{ID: "user_fifo_2", DraftID: draftID, Role: "user", Kind: "user", Content: "再做第二件事"},
		{ID: "reply_fifo_1", DraftID: draftID, Role: "assistant", Kind: "reply", Content: "第一件事已完成"},
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorUser, CreatedAt: base.Add(time.Duration(index) * time.Second),
			ResultRows: reducer.ResultRows{Message: &row},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed FIFO message %d result=%#v err=%v", index, result, err)
		}
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].itemID != "user_fifo_2" {
		t.Fatalf("前一回合晚到回复必须只核销 U1，candidates=%#v", candidates)
	}
}

func TestPersistedTurnCandidatesDoNotAttributeDecisionReplyToLaterUser(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_fifo_decision"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedAnsweredDecision(t, database, draftID, "decision_fifo_1")
	for index, row := range []reducer.MessageRow{
		{ID: "user_after_decision", DraftID: draftID, Role: "user", Kind: "user", Content: "排在决策续跑后"},
		{ID: "decision_reply", DraftID: draftID, Role: "assistant", Kind: "reply", Content: "决策续跑已完成"},
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor:      contracts.ActorUser,
			CreatedAt:  time.Date(2026, 7, 19, 12, 5, 2+index, 0, time.UTC),
			ResultRows: reducer.ResultRows{Message: &row},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed decision FIFO message %d result=%#v err=%v", index, result, err)
		}
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].itemID != "user_after_decision" {
		t.Fatalf("决策回复不得核销后入库 user，candidates=%#v", candidates)
	}
}

func TestReconcilePersistedMultipleUserTurnsRestoresFIFOOnce(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_multiple_users"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 11, 30, 0, 0, time.UTC)
	for index, row := range []reducer.MessageRow{
		{ID: "user_multiple_1", DraftID: draftID, Role: "user", Kind: "user", Content: "第一条未完成"},
		{ID: "user_multiple_2", DraftID: draftID, Role: "user", Kind: "user", Content: "第二条也未完成"},
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorUser, CreatedAt: base.Add(time.Duration(index) * time.Second),
			ResultRows: reducer.ResultRows{Message: &row},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed unmatched user %d result=%#v err=%v", index, result, err)
		}
	}

	blocking := newReconciliationBlockingModel()
	service, err := NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if pending := service.Queue().PendingCount(draftID); pending != 2 {
		t.Fatalf("启动应按 FIFO 一次重建两个未完成回合: pending=%d", pending)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := service.Queue().PendingCount(draftID); pending != 2 {
		t.Fatalf("运行中二次对账不得重复入队: pending=%d", pending)
	}
	blocking.unblock()
	service.Queue().JoinDraft(draftID)
	service.Close()

	restarted, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	candidates, err := restarted.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("两个已补驱回合均有终态后不应再出现候选: %#v", candidates)
	}
}

func TestPersistedTurnCandidatesCountBlockingDecisionReplyOnlyOnce(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_blocking_decision_fifo"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	for index, row := range []reducer.MessageRow{
		{ID: "user_before_blocking_decision", DraftID: draftID, Role: "user", Kind: "user", Content: "先执行再确认"},
		{ID: "user_queued_behind_decision", DraftID: draftID, Role: "user", Kind: "user", Content: "这是后排请求"},
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorUser, CreatedAt: base.Add(time.Duration(index) * time.Second),
			ResultRows: reducer.ResultRows{Message: &row},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed user %d result=%#v err=%v", index, result, err)
		}
	}
	decisionResult := seedPendingDecisionAt(
		t, database, draftID, "decision_blocking_fifo", base.Add(2*time.Second),
	)
	reply := reducer.MessageRow{
		ID: "reply_waiting_decision", DraftID: draftID, Role: "assistant", Kind: "reply",
		Content: "请回答上面的决策卡。",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, CreatedAt: base.Add(3 * time.Second),
		ResultRows: reducer.ResultRows{Message: &reply},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed decision reply result=%#v err=%v", result, err)
	}
	answeredVersion := decisionResult.DraftStateVersions[draftID]
	answerResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": "decision_blocking_fifo",
			"answer":      map[string]any{"option_id": "yes", "answered_via": "button"},
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &answeredVersion, CreatedAt: base.Add(4 * time.Second),
	})
	if err != nil || answerResult.Status != reducer.StatusApplied {
		t.Fatalf("answer decision result=%#v err=%v", answerResult, err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].itemID != "decision_blocking_fifo" ||
		candidates[1].itemID != "user_queued_behind_decision" {
		t.Fatalf("回答后的 continuation 应先于 pending 期间丢失的 U2 恢复: %#v", candidates)
	}
}

func TestPersistedTurnCandidatesRespectConversationClearAnchorForDecision(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_cleared_decision"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedAnsweredDecision(t, database, draftID, "decision_before_clear")
	reply := reducer.MessageRow{
		ID: "decision_reply_before_clear", DraftID: draftID, Role: "assistant", Kind: "reply",
		Content: "旧决策已经处理完成。",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, CreatedAt: time.Date(2026, 7, 19, 12, 5, 2, 0, time.UTC),
		ResultRows: reducer.ResultRows{Message: &reply},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed decision reply result=%#v err=%v", result, err)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	clearMessage := reducer.MessageRow{
		ID: "context_clear_anchor", DraftID: draftID, Role: "system_observation",
		Kind: "context_reset", Content: "对话上下文已清空。",
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "ConversationContextCleared", DraftID: draftID,
		Payload: map[string]any{"message_id": clearMessage.ID},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion,
		CreatedAt:  time.Date(2026, 7, 19, 12, 6, 0, 0, time.UTC),
		ResultRows: reducer.ResultRows{Message: &clearMessage},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("clear conversation result=%#v err=%v", result, err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("清空锚点前的 decision answer 不得重放: %#v", candidates)
	}
}

func TestPersistedTurnCandidatesDecisionCreatedCrashDoesNotReplayCompletedContinuation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_decision_created_crash"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	user := reducer.MessageRow{
		ID: "user_before_decision_created_crash", DraftID: draftID,
		Role: "user", Kind: "user", Content: "需要确认就暂停",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, CreatedAt: base,
		ResultRows: reducer.ResultRows{Message: &user},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed user result=%#v err=%v", result, err)
	}
	queuedUser := reducer.MessageRow{
		ID: "user_queued_during_decision_crash", DraftID: draftID,
		Role: "user", Kind: "user", Content: "这条请求仍应在 continuation 后补驱",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, CreatedAt: base.Add(500 * time.Millisecond),
		ResultRows: reducer.ResultRows{Message: &queuedUser},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed queued user result=%#v err=%v", result, err)
	}
	decisionResult := seedPendingDecisionAt(
		t, database, draftID, "decision_created_crash", base.Add(time.Second),
	)
	// 模拟 DecisionCreated 已提交、同回合等待提示尚未来得及落库就崩溃。用户在重启后
	// 回答，continuation 正常完成；再次重启不得把它当作未完成 decision 重放。
	answeredVersion := decisionResult.DraftStateVersions[draftID]
	answerResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": "decision_created_crash",
			"answer":      map[string]any{"option_id": "yes", "answered_via": "button"},
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &answeredVersion, CreatedAt: base.Add(2 * time.Second),
	})
	if err != nil || answerResult.Status != reducer.StatusApplied {
		t.Fatalf("answer decision result=%#v err=%v", answerResult, err)
	}
	beforeReply, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := beforeReply.persistedTurnCandidates(t.Context())
	if err != nil {
		beforeReply.Close()
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].itemID != "decision_created_crash" ||
		candidates[1].itemID != queuedUser.ID {
		beforeReply.Close()
		t.Fatalf("无 terminal 时必须按 continuation→U2 恢复: %#v", candidates)
	}
	beforeReply.Close()

	reply := reducer.MessageRow{
		ID: "decision_continuation_reply", DraftID: draftID, Role: "assistant", Kind: "reply",
		Content: "决策续跑已完成。",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, CreatedAt: base.Add(3 * time.Second),
		ResultRows: reducer.ResultRows{Message: &reply},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed continuation reply result=%#v err=%v", result, err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err = service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].itemID != queuedUser.ID {
		t.Fatalf("continuation 完成后只应补驱此前暂停的 U2: %#v", candidates)
	}
}

func TestExactCancellationAfterBlockingDecisionConsumesOnlyQueuedTurn(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_exact_cancel_after_decision"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
	for index, row := range []reducer.MessageRow{
		{ID: "user_before_exact_cancel", DraftID: draftID, Role: "user", Kind: "user", Content: "先执行"},
		{ID: "user_queued_exact_cancel", DraftID: draftID, Role: "user", Kind: "user", Content: "排队任务应被取消"},
	} {
		if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorUser, CreatedAt: base.Add(time.Duration(index) * time.Second),
			ResultRows: reducer.ResultRows{Message: &row},
		}); err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed user %d result=%#v err=%v", index, result, err)
		}
	}
	decisionResult := seedPendingDecisionAt(
		t, database, draftID, "decision_before_exact_cancel", base.Add(2*time.Second),
	)
	cancelMarker := reducer.MessageRow{
		ID: "exact_cancel_marker", DraftID: draftID, Role: "system_observation",
		Kind:    contracts.TurnCancelledObservationKind,
		Content: contracts.TurnCancelledObservationContent(1),
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, CreatedAt: base.Add(3 * time.Second),
		ResultRows: reducer.ResultRows{Message: &cancelMarker},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed exact cancel result=%#v err=%v", result, err)
	}
	answeredVersion := decisionResult.DraftStateVersions[draftID]
	if result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": "decision_before_exact_cancel",
			"answer":      map[string]any{"option_id": "yes", "answered_via": "button"},
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &answeredVersion, CreatedAt: base.Add(4 * time.Second),
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("answer result=%#v err=%v", result, err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].kind != QueueUIObservation ||
		candidates[0].itemID != "decision_before_exact_cancel" {
		t.Fatalf("exact cancel must keep only decision continuation: %#v", candidates)
	}
}

func TestPersistedTurnCandidatesIgnoreDeliveredJobReplyWhenPairingUser(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_delivered_job_reply"
	agenttest.CreateAgentDraft(t, database, draftID)
	base := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	user := reducer.MessageRow{
		ID: "user_after_job_started", DraftID: draftID,
		Role: "user", Kind: "user", Content: "这条用户请求尚未处理",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, CreatedAt: base,
		ResultRows: reducer.ResultRows{Message: &user},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed user result=%#v err=%v", result, err)
	}
	deliveredAt := base.Add(time.Second).Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO agent_job_observations(
			job_id,event_id,draft_id,event_json,claim_token,created_at,delivered_at
		) VALUES('job_reconcile_delivered',88001,?,'{}','claim_reconcile_delivered',?,?)`,
		draftID, base.Add(-time.Second).Format(time.RFC3339Nano), deliveredAt,
	); err != nil {
		t.Fatal(err)
	}
	jobReply := reducer.MessageRow{
		ID: "delivered_job_reply", DraftID: draftID, Role: "assistant", Kind: "reply",
		Content: "后台任务已完成。",
	}
	if result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, CreatedAt: base.Add(time.Second),
		ResultRows: reducer.ResultRows{Message: &jobReply},
	}); err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed delivered job reply result=%#v err=%v", result, err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	candidates, err := service.persistedTurnCandidates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].itemID != user.ID {
		t.Fatalf("已交付 job 的 reply 不得核销 user trigger: %#v", candidates)
	}
}

func TestReconcilePersistedUserTurnAfterCommitBeforeEnqueueCrash(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_user"
	agenttest.CreateAgentDraft(t, database, draftID)
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "user_reconcile", DraftID: draftID, Role: "user", Kind: "user", Content: "继续完成剪辑",
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed message result=%#v err=%v", result, err)
	}
	result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "tool_before_crash", DraftID: draftID, Role: "system", Kind: "tool",
			Content: `{"step_id":"tool_before_crash","status":"succeeded"}`,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed tool trace result=%#v err=%v", result, err)
	}

	blocking := newReconciliationBlockingModel()
	service, err := NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if pending := service.Queue().PendingCount(draftID); pending != 1 {
		t.Fatalf("启动对账 pending=%d want=1", pending)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := service.Queue().PendingCount(draftID); pending != 1 {
		t.Fatalf("重复对账 pending=%d want=1", pending)
	}
	blocking.unblock()
	service.Queue().JoinDraft(draftID)
	service.Close()

	restarted, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := restarted.Queue().PendingCount(draftID); pending != 0 {
		t.Fatalf("已有回复仍被误驱: pending=%d", pending)
	}
}

func TestReconcilePersistedAnsweredDecisionOnceAcrossRestart(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_decision"
	agenttest.CreateAgentDraft(t, database, draftID)
	seedAnsweredDecision(t, database, draftID, "decision_reconcile")
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor:     contracts.ActorAgent,
		CreatedAt: time.Date(2026, 7, 19, 12, 5, 2, 0, time.UTC),
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "decision_tool_before_crash", DraftID: draftID, Role: "system", Kind: "tool",
			Content: `{"step_id":"decision_tool_before_crash","status":"succeeded"}`,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed decision tool trace result=%#v err=%v", result, err)
	}

	blocking := newReconciliationBlockingModel()
	service, err := NewService(t.Context(), database, blocking)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	if pending := service.Queue().PendingCount(draftID); pending != 1 {
		t.Fatalf("decision 补驱 pending=%d want=1", pending)
	}
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := service.Queue().PendingCount(draftID); pending != 1 {
		t.Fatalf("decision 重复对账 pending=%d want=1", pending)
	}
	blocking.unblock()
	service.Queue().JoinDraft(draftID)
	service.Close()

	restarted, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := restarted.Queue().PendingCount(draftID); pending != 0 {
		t.Fatalf("decision 已产出回复仍被二次启动补驱: pending=%d", pending)
	}
}

func TestReconcilePersistedUserTurnStopsAtPendingBlockingDecision(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reconcile_pending_decision"
	agenttest.CreateAgentDraft(t, database, draftID)
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, CreatedAt: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "user_before_pending_decision", DraftID: draftID, Role: "user", Kind: "user",
			Content: "需要我确认就问我",
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed user result=%#v err=%v", result, err)
	}
	seedPendingDecision(t, database, draftID, "decision_waiting")

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending := service.Queue().PendingCount(draftID); pending != 0 {
		t.Fatalf("已持久化阻塞决策的 user 回合不应重跑: pending=%d", pending)
	}
}

func TestReconcilePersistedTurnIgnoresCancellationMarkerAndRewoundDecision(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const cancelledDraft = "draft_reconcile_cancelled"
	agenttest.CreateAgentDraft(t, database, cancelledDraft)
	for index, row := range []reducer.MessageRow{
		{ID: "user_cancelled_1", DraftID: cancelledDraft, Role: "user", Kind: "user", Content: "不要继续"},
		{ID: "user_cancelled_2", DraftID: cancelledDraft, Role: "user", Kind: "user", Content: "排队的也停掉"},
		{ID: "user_cancelled_3", DraftID: cancelledDraft, Role: "user", Kind: "user", Content: "全部停止"},
		{ID: "cancel_marker", DraftID: cancelledDraft, Role: "system_observation",
			Kind: contracts.TurnCancelledObservationKind, Content: contracts.TurnCancelledObservationContent(3)},
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorUser, CreatedAt: time.Date(2026, 7, 19, 12, 0, index, 0, time.UTC),
			ResultRows: reducer.ResultRows{Message: &row},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("seed cancellation message %d result=%#v err=%v", index, result, err)
		}
	}

	const rewoundDraft = "draft_reconcile_rewound_decision"
	agenttest.CreateAgentDraft(t, database, rewoundDraft)
	seedAnsweredDecision(t, database, rewoundDraft, "decision_rewound")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO event_log(event_type,actor,draft_id,payload_json,state_version,created_at)
		VALUES('TimelineVersionRestored','user',?, '{}', 3, ?)`,
		rewoundDraft, time.Date(2026, 7, 19, 12, 10, 0, 0, time.UTC).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.ReconcilePersistedTurns(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, draftID := range []string{cancelledDraft, rewoundDraft} {
		if pending := service.Queue().PendingCount(draftID); pending != 0 {
			t.Fatalf("draft %s 被误驱: pending=%d", draftID, pending)
		}
	}
}

func seedAnsweredDecision(t *testing.T, database *storage.DB, draftID, decisionID string) {
	t.Helper()
	createdAt := time.Date(2026, 7, 19, 12, 5, 0, 0, time.UTC)
	result := seedPendingDecisionAt(t, database, draftID, decisionID, createdAt)
	answeredVersion := result.DraftStateVersions[draftID]
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionAnswered", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": decisionID,
			"answer":      map[string]any{"option_id": "yes", "answered_via": "button"},
		},
	}}, reducer.Options{
		Actor: contracts.ActorUser, BaseVersion: &answeredVersion,
		CreatedAt: createdAt.Add(time.Second),
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("answer decision status=%s err=%v", result.Status, err)
	}
}

func seedPendingDecision(t *testing.T, database *storage.DB, draftID, decisionID string) reducer.Result {
	t.Helper()
	return seedPendingDecisionAt(
		t, database, draftID, decisionID, time.Date(2026, 7, 19, 12, 0, 1, 0, time.UTC),
	)
}

func seedPendingDecisionAt(
	t *testing.T,
	database *storage.DB,
	draftID, decisionID string,
	createdAt time.Time,
) reducer.Result {
	t.Helper()
	draftVersion := 0
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DecisionCreated", DraftID: draftID,
		Payload: map[string]any{
			"decision_id": decisionID, "scope_type": "draft", "type": "critical",
			"question": "继续吗？", "options": []map[string]any{{"option_id": "yes", "label": "继续"}},
			"blocking": true, "allow_free_text": false,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent, BaseVersion: &draftVersion, CreatedAt: createdAt})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("create decision status=%s err=%v", result.Status, err)
	}
	return result
}
