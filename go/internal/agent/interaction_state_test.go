package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestNormalizeDecisionTypeMapsApprovalScenarios(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"approve_content_plan": "approve_content_plan",
		"approve_speech_cut":   "approve_speech_cut",
		"approve_rough_cut":    "approve_rough_cut",
		"unexpected":           "generic",
	} {
		if got := normalizeDecisionType(input); got != want {
			t.Errorf("normalizeDecisionType(%q)=%q want=%q", input, got, want)
		}
	}
}

func TestAskUserPersistsToolCallAndRejectsSameTurnSelfAnswer(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_same_turn_decision")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	base := rushestools.WithDraftID(t.Context(), "draft_same_turn_decision")
	base = withTurnInteractionState(base, newTurnInteractionState())
	askContext := rushestools.WithToolCallID(base, "call_ask_1")
	raw, err := service.ExecuteTool(askContext, "interaction.ask_user", rushestools.AskUserInput{
		Question:     "确认口播剪辑草案？",
		DecisionType: "approve_speech_cut",
		Options: []rushestools.DecisionOptionInput{{
			OptionID: "confirm", Label: "确认执行",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting := raw.(rushestools.ToolResult)
	if waiting.Status != "waiting" || waiting.Data["turn_should_end"] != true ||
		waiting.Data["decision_type"] != "approve_speech_cut" {
		t.Fatalf("waiting=%#v", waiting)
	}
	decisionID := waiting.Data["decision_id"].(string)
	decision, err := storage.GetDecision(t.Context(), database.Read(), decisionID)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Type != "approve_speech_cut" || decision.CreatedByToolCallID == nil ||
		*decision.CreatedByToolCallID != "call_ask_1" {
		t.Fatalf("decision=%#v", decision)
	}

	blockedRaw, err := service.ExecuteTool(base, "plan.update", rushestools.PlanUpdateInput{
		Plan: map[string]any{"style": "不应写入"},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := blockedRaw.(rushestools.ToolResult)
	if blocked.Status != "waiting" || blocked.Data["blocked_tool"] != "plan.update" ||
		blocked.Data["turn_should_end"] != true {
		t.Fatalf("blocked=%#v", blocked)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), "draft_same_turn_decision")
	if err != nil || len(draft.ContentPlan) != 0 {
		t.Fatalf("blocking decision allowed plan mutation: plan=%#v err=%v", draft.ContentPlan, err)
	}

	answerContext := rushestools.WithToolCallID(base, "call_answer_1")
	answerRaw, err := service.ExecuteTool(answerContext, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "confirm",
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := answerRaw.(rushestools.ToolResult)
	if answer.Status != "waiting" || answer.Data["turn_should_end"] != true ||
		answer.Data["blocked_tool"] != "decision.answer" {
		t.Fatalf("answer=%#v", answer)
	}
	decision, err = storage.GetDecision(t.Context(), database.Read(), decisionID)
	if err != nil || decision.Status != "pending" {
		t.Fatalf("same-turn answer changed decision: %#v err=%v", decision, err)
	}
}

func TestBlockingDecisionSerializesParallelToolCalls(t *testing.T) {
	t.Parallel()
	state := newTurnInteractionState()
	ctx := withTurnInteractionState(t.Context(), state)
	release, blocked := beginTurnToolCall(ctx)
	if blocked != "" {
		t.Fatalf("unexpected initial block %q", blocked)
	}

	acquired := make(chan string, 1)
	go func() {
		unlock, decisionID := beginTurnToolCall(ctx)
		defer unlock()
		acquired <- decisionID
	}()
	select {
	case decisionID := <-acquired:
		t.Fatalf("parallel call bypassed active tool execution: %q", decisionID)
	case <-time.After(20 * time.Millisecond):
	}
	markDecisionCreatedThisTurn(ctx, "decision_parallel", true)
	release()
	select {
	case decisionID := <-acquired:
		if decisionID != "decision_parallel" {
			t.Fatalf("parallel call observed %q", decisionID)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel call remained blocked after first tool completed")
	}
}

func TestDecisionAnswerValidatesOwnershipStateAndAnswer(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_decision_owner")
	createAgentDraft(t, database, "draft_decision_other")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ownerContext := rushestools.WithDraftID(t.Context(), "draft_decision_owner")
	allowFreeText := false
	raw, err := service.ExecuteTool(ownerContext, "interaction.ask_user", rushestools.AskUserInput{
		Question: "选择节奏", AllowFreeText: &allowFreeText,
		Options: []rushestools.DecisionOptionInput{{OptionID: "fast", Label: "快节奏"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionID := raw.(rushestools.ToolResult).Data["decision_id"].(string)
	checks := []struct {
		name  string
		ctx   context.Context
		input rushestools.DecisionAnswerInput
	}{
		{"empty", ownerContext, rushestools.DecisionAnswerInput{DecisionID: decisionID}},
		{"unknown option", ownerContext, rushestools.DecisionAnswerInput{DecisionID: decisionID, OptionID: "slow"}},
		{"free text disabled", ownerContext, rushestools.DecisionAnswerInput{DecisionID: decisionID, FreeText: "更快"}},
		{"wrong draft", rushestools.WithDraftID(t.Context(), "draft_decision_other"), rushestools.DecisionAnswerInput{DecisionID: decisionID, OptionID: "fast"}},
	}
	for _, check := range checks {
		if _, executeErr := service.ExecuteTool(check.ctx, "decision.answer", check.input); executeErr == nil {
			t.Errorf("%s should fail", check.name)
		}
	}
	if _, err := service.ExecuteTool(ownerContext, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "fast",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteTool(ownerContext, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "fast",
	}); err == nil {
		t.Fatal("answered decision should not be overwritten")
	}
}
