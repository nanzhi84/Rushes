package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// destructiveConfirmationInterceptor 是 G2 的第一个执行拦截器（#103 G2）：当工具
// Effect==EffectDestructive、本次输入确实走破坏性路径、且 ctx 未持有本回合已确认的重放
// 凭证时，拒绝执行并要求模型先经 interaction.confirm_action 取得用户确认。模型自觉先调
// confirm 的现行为不变；忘调时从「直接执行」变为「被拦下并提示」。
//
// 确认后的重放走 replayPendingTool→executeReported 直连 Service.ExecuteTool，绕过 eino
// 执行闭包，因此本拦截器只把守模型主路径；凭证判定仍保留，语义自洽且防未来路径变化。
func destructiveConfirmationInterceptor(ctx context.Context, spec rushestools.Spec, input any) error {
	if spec.Effect != rushestools.EffectDestructive {
		return nil
	}
	if !inputIsDestructive(spec.Name, input) {
		return nil
	}
	if agentexec.IsConfirmedToolReplay(ctx) {
		return nil
	}
	return &rushestools.InterceptorRejection{
		Observation: "该操作会造成不可逆或影响 agent 之外的改动，必须先经 interaction.confirm_action 获得用户确认后才能执行。",
		Data: map[string]any{
			"error_code":  string(rushestools.ErrCodeConfirmationRequired),
			"tool":        spec.Name,
			"recovery":    "先经 interaction.confirm_action 取得用户确认；确认后系统会自动重放本次调用。",
			"next_action": "调用 interaction.confirm_action，在 tool_name 传本工具名、arguments 原样传本次参数；用户确认后系统会自动重放执行。",
		},
	}
}

// inputIsDestructive 把「工具级 Effect」精化为「本次调用是否真的破坏性」。Effect 是必要不
// 充分信号：memory.update 仅在携带 remove_keys（删除长期记忆）时才破坏，纯新增/更新可逆、
// 豁免确认（G2 验收明确纯新增不受影响，见 tools/registry.go 的 registerMemoryUpdate 注释）。
// 未来的删除类工具默认按破坏性处理。
func inputIsDestructive(name string, input any) bool {
	switch name {
	case "memory.update":
		update, ok := input.(rushestools.MemoryUpdateInput)
		if !ok {
			// 类型断言失败是意外情形；安全闸门默认方向必须 fail-closed，交由确认。
			return true
		}
		return len(update.RemoveKeys) > 0
	default:
		return true
	}
}

// isInterceptorRejection 报告 err 是否为拦截器的策略拒绝。恢复中间件据此在重试判定里对拒绝
// 做结构性短路（与 context.Canceled 同级），不依赖拒绝文案是否恰好含 transient 词。
func isInterceptorRejection(err error) bool {
	var rejection *rushestools.InterceptorRejection
	return errors.As(err, &rejection)
}

// marshalInterceptorRejection 把策略拒绝渲染成模型可读的结构化工具结果；状态用 failed，让
// 模型据 observation/next_action 改走 confirm_action，但它不进恢复账（中间件不记失败）。
func marshalInterceptorRejection(rejection *rushestools.InterceptorRejection) string {
	encoded, _ := json.Marshal(map[string]any{
		"status":      string(rushestools.StatusFailed),
		"observation": rejection.Observation,
		"data":        rejection.Data,
	})
	return string(encoded)
}

// rejectionToolResult 供 reporter 记录被拦调用的终态；以结果而非错误上报，因为策略拒绝不是
// 工具执行失败。
func rejectionToolResult(rejection *rushestools.InterceptorRejection) rushestools.ToolResult {
	return rushestools.ToolResult{
		Status:      string(rushestools.StatusFailed),
		Observation: rejection.Observation,
		Data:        rejection.Data,
	}
}
