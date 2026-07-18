package agent

import (
	"context"

	"github.com/nanzhi84/Rushes/go/internal/agentexec"
)

// beginTurnToolCall 是引擎侧的决策屏障入口：读取回合交互态，取得工具执行互斥并
// 返回本回合是否已存在阻塞决策。状态类型与内部字段在 agentexec，这里只做引擎语义。
func beginTurnToolCall(ctx context.Context) (func(), string) {
	state := agentexec.InteractionStateFromContext(ctx)
	if state == nil {
		return func() {}, ""
	}
	return state.BeginToolCall()
}
