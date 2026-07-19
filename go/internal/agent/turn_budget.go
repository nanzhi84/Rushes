package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/schema"
)

type turnBudgetContextKey struct{}

type turnBudgetState struct {
	mu               sync.Mutex
	modelCalls       int
	maxToolRounds    int
	usageCalls       int
	promptTokens     int
	cachedTokens     int
	completionTokens int
	totalTokens      int
}

func (state *turnBudgetState) recordUsage(usage *schema.TokenUsage) {
	if state == nil || usage == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.usageCalls++
	state.promptTokens += usage.PromptTokens
	state.cachedTokens += usage.PromptTokenDetails.CachedTokens
	state.completionTokens += usage.CompletionTokens
	state.totalTokens += usage.TotalTokens
}

func (state *turnBudgetState) usageSnapshot() map[string]any {
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.usageCalls == 0 {
		return nil
	}
	return map[string]any{
		"model_calls":          state.usageCalls,
		"prompt_tokens":        state.promptTokens,
		"cached_prompt_tokens": state.cachedTokens,
		"completion_tokens":    state.completionTokens,
		"total_tokens":         state.totalTokens,
	}
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

func (state *turnBudgetState) remainingToolRounds() int {
	state.mu.Lock()
	defer state.mu.Unlock()
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
		"【工具预算提醒】本回合剩余 %d 次模型与工具往返。请立即开始收敛：先用 plan.update 固化已确定但未执行的计划要点，合并可批量提交的 apply_patches，跳过非必要检索；若预算内无法完成，最终回复必须如实说明已完成、未完成和下一步。",
		remainingToolRounds,
	)
}

// turnToolResultCompactionInstruction 在回合内工具结果被主动压缩时注入，引导模型先固化
// 结论再收敛，而不是因摘要丢了细节而重复检索（#95 H1b）。
const turnToolResultCompactionInstruction = "【上下文压缩提醒】历史工具结果已超出软预算并被有界摘要，细节可能已丢失。请先用 plan.update 把已经确认的结论与关键 ID（素材、镜头、时间线片段等）固化进内容计划，再收敛输出最终回复；不要因为摘要丢了细节而重复检索同样的信息。"

func turnBudgetMessageModifier(
	ctx context.Context,
	messages []*schema.Message,
) []*schema.Message {
	prompt := coreSystemPrompt
	if state := turnBudgetFromContext(ctx); state != nil {
		if instruction := turnBudgetInstruction(state.beginModelCall()); instruction != "" {
			prompt += "\n\n" + instruction
		}
	}
	// 回合内软预算：工具结果累计超阈值时先做有界摘要，再追加收敛提示（#95 H1b）。
	if compacted, didCompact := compactTurnToolResults(messages); didCompact {
		messages = compacted
		prompt += "\n\n" + turnToolResultCompactionInstruction
	}
	return append([]*schema.Message{schema.SystemMessage(prompt)}, messages...)
}
