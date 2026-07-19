package agent

import (
	"context"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// toolRouter 按 registry.Effect 逐消息在并行/串行两个 ToolsNode 间路由（#103 G3b 路线 2a）。
// 一条 assistant 消息的 tool_calls 全为 EffectReadOnly → 并行节点;含任何写工具、空 tool_calls
// 或未知工具 → 串行节点（保序是正确性,不是性能取舍）。结果按原下标聚合由 eino 两种模式各自
// 保证,不依赖调度顺序。
//
// 两个 ToolsNode 共享同一 Tools 与中间件,仅 ExecuteSequentially 相反。把本路由挂成图节点
// （AddLambdaNode）的 react 图复刻在 impl-s1a 的 react 循环 PR 落地后基于其结构接入。
type toolRouter struct {
	parallel *compose.ToolsNode
	serial   *compose.ToolsNode
	readOnly func(name string) bool
}

// newToolRouter 用同一份 ToolsNodeConfig 构造并行与串行两个执行节点,读写分类事实源是
// registry.Effect（经 effectOf 注入）,不建第二清单。
func newToolRouter(
	ctx context.Context,
	config compose.ToolsNodeConfig,
	effectOf func(string) (rushestools.Effect, bool),
) (*toolRouter, error) {
	parallelConfig := config
	parallelConfig.ExecuteSequentially = false
	serialConfig := config
	serialConfig.ExecuteSequentially = true
	parallel, err := compose.NewToolNode(ctx, &parallelConfig)
	if err != nil {
		return nil, err
	}
	serial, err := compose.NewToolNode(ctx, &serialConfig)
	if err != nil {
		return nil, err
	}
	return &toolRouter{
		parallel: parallel,
		serial:   serial,
		readOnly: func(name string) bool {
			effect, ok := effectOf(name)
			return ok && effect == rushestools.EffectReadOnly
		},
	}, nil
}

// allReadOnly 报告消息的每个 tool_call 是否都是只读工具。空 tool_calls 保守视为非只读。
func (router *toolRouter) allReadOnly(message *schema.Message) bool {
	if message == nil || len(message.ToolCalls) == 0 {
		return false
	}
	for _, call := range message.ToolCalls {
		if !router.readOnly(call.Function.Name) {
			return false
		}
	}
	return true
}

// node 选择本条消息的执行节点:全只读走并行,否则走串行。
func (router *toolRouter) node(message *schema.Message) *compose.ToolsNode {
	if router.allReadOnly(message) {
		return router.parallel
	}
	return router.serial
}

// Invoke 把整条消息委派给所选节点。含写工具的消息走串行节点,由 eino 久经考验的串行路径保证
// 保序;全只读消息走并行节点,聚合仍按原下标保序。
func (router *toolRouter) Invoke(
	ctx context.Context, input *schema.Message, opts ...compose.ToolsNodeOption,
) ([]*schema.Message, error) {
	return router.node(input).Invoke(ctx, input, opts...)
}
