package agent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestTurnBudgetMessageModifierInjectsOnlyWhenConvergenceIsRequired(t *testing.T) {
	t.Parallel()
	const fixtureToolRoundBudget = 30
	input := []*schema.Message{schema.UserMessage("继续剪辑")}
	withoutState := turnBudgetMessageModifier(t.Context(), input)
	if len(withoutState) != 2 || withoutState[0].Content != systemPrompt ||
		len(input) != 1 || input[0].Content != "继续剪辑" {
		t.Fatalf("without state=%#v input=%#v", withoutState, input)
	}

	state := newTurnBudgetState(fixtureToolRoundBudget)
	ctx := withTurnBudgetState(t.Context(), state)
	for call := 1; call <= fixtureToolRoundBudget+2; call++ {
		messages := turnBudgetMessageModifier(ctx, input)
		prompt := messages[0].Content
		switch {
		case call <= 25:
			if prompt != systemPrompt {
				t.Fatalf("model call %d must add zero budget bytes", call)
			}
		case call == 26:
			if !strings.Contains(prompt, "工具预算提醒") ||
				!strings.Contains(prompt, "剩余 5 次") ||
				strings.Contains(prompt, "禁止再调工具") {
				t.Fatalf("model call 26 prompt=%q", prompt)
			}
		case call >= 31:
			if !strings.Contains(prompt, "最后一次生成机会") ||
				!strings.Contains(prompt, "禁止再调工具") {
				t.Fatalf("model call %d prompt=%q", call, prompt)
			}
		}
		if len(input) != 1 || input[0].Content != "继续剪辑" {
			t.Fatalf("model call %d mutated input=%#v", call, input)
		}
	}
}

func TestTurnBudgetInstructionCoversRemainingBranches(t *testing.T) {
	t.Parallel()
	if turnBudgetInstruction(6) != "" ||
		!strings.Contains(turnBudgetInstruction(5), "剩余 5 次") ||
		!strings.Contains(turnBudgetInstruction(1), "剩余 1 次") ||
		!strings.Contains(turnBudgetInstruction(0), "禁止再调工具") ||
		!strings.Contains(turnBudgetInstruction(-1), "禁止再调工具") {
		t.Fatal("turn budget instruction branch mismatch")
	}
	state := newTurnBudgetState(-1)
	if remaining := state.beginModelCall(); remaining != 0 {
		t.Fatalf("negative budget remaining=%d", remaining)
	}
}
