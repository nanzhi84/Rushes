package agentexec

import (
	"context"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestCommitDurableTerminalUsesFenceAndFallback(t *testing.T) {
	t.Parallel()
	base := context.Background()
	if got := WithDurableTerminalCommit(base, nil); got != base {
		t.Fatal("nil fence should preserve context")
	}
	commitCalls := 0
	commit := func() (bool, error) {
		commitCalls++
		return true, nil
	}
	if applied, err := CommitDurableTerminal(t.Context(), commit); err != nil || !applied || commitCalls != 1 {
		t.Fatalf("fallback applied=%v calls=%d err=%v", applied, commitCalls, err)
	}
	fenceCalls := 0
	ctx := WithDurableTerminalCommit(context.Background(), func(inner func() (bool, error)) (bool, error) {
		fenceCalls++
		return inner()
	})
	if applied, err := CommitDurableTerminal(ctx, commit); err != nil || !applied ||
		fenceCalls != 1 || commitCalls != 2 {
		t.Fatalf("fenced applied=%v fence_calls=%d commit_calls=%d err=%v",
			applied, fenceCalls, commitCalls, err)
	}
}

func TestAskUserFencesOnlyBlockingDecisionCommit(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		draftID   string
		blocking  *bool
		wantFence int
	}{
		{draftID: "draft_blocking_decision", wantFence: 1},
		{draftID: "draft_nonblocking_decision", blocking: BoolPointer(false), wantFence: 0},
	} {
		agenttest.CreateAgentDraft(t, database, test.draftID)
		fenceCalls := 0
		ctx := rushestools.WithDraftID(t.Context(), test.draftID)
		ctx = WithTurnInteractionState(ctx, NewTurnInteractionState())
		ctx = WithDurableTerminalCommit(ctx, func(commit func() (bool, error)) (bool, error) {
			fenceCalls++
			return commit()
		})
		_, err := exec.ExecuteTool(ctx, "interaction.ask_user", rushestools.AskUserInput{
			Question: "哪一种核心目标才是正确方向？",
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "a", Label: "方向 A"}, {OptionID: "b", Label: "方向 B"},
			},
			Blocking: test.blocking, DecisionType: "critical",
		})
		if err != nil || fenceCalls != test.wantFence {
			t.Fatalf("draft=%s fence_calls=%d want=%d err=%v",
				test.draftID, fenceCalls, test.wantFence, err)
		}
	}
}
