package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestNormalizeDecisionTypeMapsKnownScenarios(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"critical":             "critical",
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
	base = agentexec.WithTurnInteractionState(base, agentexec.NewTurnInteractionState())
	askContext := rushestools.WithToolCallID(base, "call_ask_1")
	raw, err := service.ExecuteTool(askContext, "interaction.ask_user", rushestools.AskUserInput{
		Question:     "当前素材支持两条互相冲突的主线，且无法判断用户目标，请选择核心方向。",
		DecisionType: "critical",
		Options: []rushestools.DecisionOptionInput{{
			OptionID: "product", Label: "产品体验",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waiting := raw.(rushestools.ToolResult)
	if waiting.Status != "waiting" || waiting.Data["turn_should_end"] != true ||
		waiting.Data["decision_type"] != "critical" {
		t.Fatalf("waiting=%#v", waiting)
	}
	decisionID := waiting.Data["decision_id"].(string)
	decision, err := storage.GetDecision(t.Context(), database.Read(), decisionID)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Type != "critical" || decision.CreatedByToolCallID == nil ||
		*decision.CreatedByToolCallID != "call_ask_1" {
		t.Fatalf("decision=%#v", decision)
	}
	directAnswer, err := service.toolDecisionAnswer(askContext, "draft_same_turn_decision", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "product",
	})
	if err != nil || directAnswer.Status != "failed" || directAnswer.Data["turn_should_end"] != true {
		t.Fatalf("same-turn direct answer=%#v err=%v", directAnswer, err)
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

func TestAdjudicateDecisionAnswerTrustedOptionPayloadWins(t *testing.T) {
	t.Parallel()
	answer, err := AdjudicateDecisionAnswer(storage.Decision{
		Options: []map[string]any{{
			"option_id": "story",
			"payload":   map[string]any{"shared": "trusted", "preset": "narrative"},
		}},
		AllowFreeText: true,
	}, " story ", "  保留人物情绪  ", map[string]any{
		"shared": "caller", "source": "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := answer["payload"].(map[string]any)
	if !ok || payload["shared"] != "trusted" || payload["preset"] != "narrative" ||
		payload["source"] != "user" || answer["free_text"] != "保留人物情绪" {
		t.Fatalf("answer=%#v", answer)
	}
}

func TestAskUserRejectsCreativeApprovalAndVerboseCriticalQuestion(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_autonomous_editing")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	ctx := rushestools.WithDraftID(t.Context(), "draft_autonomous_editing")
	ctx = agentexec.WithTurnInteractionState(ctx, agentexec.NewTurnInteractionState())
	for name, input := range map[string]rushestools.AskUserInput{
		"reversible approval": {
			Question: "请逐项确认口播删保项和 B-roll 方案。", DecisionType: "approve_speech_cut",
		},
		"verbose critical": {
			Question: strings.Repeat("细", 241), DecisionType: "critical",
		},
	} {
		raw, executeErr := service.ExecuteTool(ctx, "interaction.ask_user", input)
		if executeErr != nil {
			t.Fatalf("%s: %v", name, executeErr)
		}
		result := raw.(rushestools.ToolResult)
		if result.Status != "failed" {
			t.Errorf("%s result=%#v", name, result)
		}
	}

	var decisionCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM decisions WHERE draft_id='draft_autonomous_editing'`).Scan(&decisionCount); err != nil {
		t.Fatal(err)
	}
	if decisionCount != 0 {
		t.Fatalf("rejected questions created %d decisions", decisionCount)
	}
	raw, err := service.ExecuteTool(ctx, "plan.update", rushestools.PlanUpdateInput{
		Plan: map[string]any{"editing_policy": "autonomous"},
	})
	if err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("rejected question blocked later work: raw=%#v err=%v", raw, err)
	}
}

func TestBlockingDecisionSerializesParallelToolCalls(t *testing.T) {
	t.Parallel()
	state := agentexec.NewTurnInteractionState()
	ctx := agentexec.WithTurnInteractionState(t.Context(), state)
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
		Question: "关键节奏目标存在冲突，请选择方向。", DecisionType: "critical",
		AllowFreeText: &allowFreeText,
		Options:       []rushestools.DecisionOptionInput{{OptionID: "fast", Label: "快节奏"}},
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
