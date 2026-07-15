package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/schema"
)

type turnBudgetContextKey struct{}

type turnBudgetState struct {
	mu            sync.Mutex
	modelCalls    int
	maxToolRounds int
}

func newTurnBudgetState(maxToolRounds int) *turnBudgetState {
	return &turnBudgetState{maxToolRounds: max(0, maxToolRounds)}
}

func withTurnBudgetState(ctx context.Context, state *turnBudgetState) context.Context {
	return context.WithValue(ctx, turnBudgetContextKey{}, state)
}

func turnBudgetFromContext(ctx context.Context) *turnBudgetState {
	state, _ := ctx.Value(turnBudgetContextKey{}).(*turnBudgetState)
	return state
}

func (state *turnBudgetState) beginModelCall() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.modelCalls++
	return max(0, state.maxToolRounds+1-state.modelCalls)
}

func turnBudgetInstruction(remainingToolRounds int) string {
	if remainingToolRounds > 5 {
		return ""
	}
	if remainingToolRounds <= 0 {
		return "【工具预算提醒】这是本回合最后一次生成机会。禁止再调工具，直接输出最终回复；如仍未完成，必须如实说明已完成、未完成和下一步。"
	}
	return fmt.Sprintf(
		"【工具预算提醒】本回合剩余 %d 次模型与工具往返。请立即开始收敛：合并可批量提交的 apply_patches，跳过非必要检索；若预算内无法完成，最终回复必须如实说明已完成、未完成和下一步。",
		remainingToolRounds,
	)
}

func turnBudgetMessageModifier(
	ctx context.Context,
	messages []*schema.Message,
) []*schema.Message {
	prompt := systemPrompt
	if state := turnBudgetFromContext(ctx); state != nil {
		if instruction := turnBudgetInstruction(state.beginModelCall()); instruction != "" {
			prompt += "\n\n" + instruction
		}
	}
	return append([]*schema.Message{schema.SystemMessage(prompt)}, messages...)
}
