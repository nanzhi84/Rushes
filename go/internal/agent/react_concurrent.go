package agent

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	concurrentNodeModel = "model"
	concurrentNodeTools = "tools"
)

// concurrentReactState 累积本回合消息,供 modelPreHandle/toolsPreHandle 逐轮追加。不做 eino
// 中断/检查点序列化(Rushes 用自有 WorldState checkpoint,不用 eino 图中断)。
type concurrentReactState struct {
	Messages []*schema.Message
}

// concurrentReactAgent 是 eino react 图的 Rushes 复刻(#103 G3b 路线 2a)。唯一实质差异:把单个
// ToolsNode 换成按 registry.Effect 逐消息路由的 toolRouter——一条 assistant 消息的 tool_calls
// 全为只读则并行执行、含写则串行(保序是正确性)。模型侧语义全部原样保留:同一 H5 直通包装模型、
// 同一 StreamToolCallChecker(含 H5 早退)、同一 MessageModifier(H1b turnBudget)、同一
// modelPreHandle/toolsPreHandle 状态累积、同一 MaxStep/AnyPredecessor 编译。
//
// 省略两处对 Rushes 恒 no-op、且依赖 eino 未导出内部的机制:① react 的 tool-result collector
// 中间件(Rushes 只消费终态文本流、工具进度走自有 reporter,从不设 eino sender);② return-directly
// 分支(Rushes 不设 ToolReturnDirectly,工具轮恒回模型)。
type concurrentReactAgent struct {
	runnable compose.Runnable[[]*schema.Message, *schema.Message]
}

func newConcurrentReactAgent(
	ctx context.Context,
	chatModel model.ToolCallingChatModel,
	toolsConfig compose.ToolsNodeConfig,
	effectOf func(string) (rushestools.Effect, bool),
	maxStep int,
	toolCallChecker func(context.Context, *schema.StreamReader[*schema.Message]) (bool, error),
	messageModifier func(context.Context, []*schema.Message) []*schema.Message,
) (*concurrentReactAgent, error) {
	toolInfos, err := toolInfosFromConfig(ctx, toolsConfig)
	if err != nil {
		return nil, err
	}
	boundModel, err := einoagent.ChatModelWithTools(nil, chatModel, toolInfos)
	if err != nil {
		return nil, err
	}
	router, err := newToolRouter(ctx, toolsConfig, effectOf)
	if err != nil {
		return nil, err
	}

	graph := compose.NewGraph[[]*schema.Message, *schema.Message](
		compose.WithGenLocalState(func(context.Context) *concurrentReactState {
			return &concurrentReactState{Messages: make([]*schema.Message, 0, maxStep+1)}
		}),
	)

	// modelPreHandle 与 react 一致:累积消息,再套 MessageModifier(H1b turnBudget 收敛提醒)。
	modelPreHandle := func(ctx context.Context, input []*schema.Message, state *concurrentReactState) ([]*schema.Message, error) {
		state.Messages = append(state.Messages, input...)
		if messageModifier == nil {
			return state.Messages, nil
		}
		modified := make([]*schema.Message, len(state.Messages))
		copy(modified, state.Messages)
		return messageModifier(ctx, modified), nil
	}
	if err := graph.AddChatModelNode(concurrentNodeModel, boundModel, compose.WithStatePreHandler(modelPreHandle)); err != nil {
		return nil, err
	}
	if err := graph.AddEdge(compose.START, concurrentNodeModel); err != nil {
		return nil, err
	}

	// toolsPreHandle 与 react 一致:把上一条 assistant 消息追加进状态(input==nil 为中断恢复兜底)。
	toolsPreHandle := func(_ context.Context, input *schema.Message, state *concurrentReactState) (*schema.Message, error) {
		if input == nil {
			return state.Messages[len(state.Messages)-1], nil
		}
		state.Messages = append(state.Messages, input)
		return input, nil
	}
	routerLambda := compose.InvokableLambda(func(ctx context.Context, input *schema.Message) ([]*schema.Message, error) {
		return router.Invoke(ctx, input)
	})
	if err := graph.AddLambdaNode(concurrentNodeTools, routerLambda, compose.WithStatePreHandler(toolsPreHandle)); err != nil {
		return nil, err
	}

	// model → branch(StreamToolCallChecker,含 H5 早退)→ {tools, END}。
	modelBranch := func(ctx context.Context, stream *schema.StreamReader[*schema.Message]) (string, error) {
		isToolCall, err := toolCallChecker(ctx, stream)
		if err != nil {
			return "", err
		}
		if isToolCall {
			return concurrentNodeTools, nil
		}
		return compose.END, nil
	}
	if err := graph.AddBranch(concurrentNodeModel,
		compose.NewStreamGraphBranch(modelBranch, map[string]bool{concurrentNodeTools: true, compose.END: true})); err != nil {
		return nil, err
	}
	// Rushes 不设 ToolReturnDirectly:工具轮恒回到模型(省略 react 的 return-directly 分支)。
	if err := graph.AddEdge(concurrentNodeTools, concurrentNodeModel); err != nil {
		return nil, err
	}

	runnable, err := graph.Compile(ctx,
		compose.WithMaxRunSteps(maxStep),
		compose.WithNodeTriggerMode(compose.AnyPredecessor),
	)
	if err != nil {
		return nil, err
	}
	return &concurrentReactAgent{runnable: runnable}, nil
}

// Generate 与 Stream 与 react.Agent 对齐:分别走底层 runnable 的 Invoke/Stream。生产 turn 流走
// Stream(H5 直通);脚本模型测试走 Generate。
func (reactAgent *concurrentReactAgent) Generate(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	return reactAgent.runnable.Invoke(ctx, messages)
}

func (reactAgent *concurrentReactAgent) Stream(ctx context.Context, messages []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return reactAgent.runnable.Stream(ctx, messages)
}

func toolInfosFromConfig(ctx context.Context, config compose.ToolsNodeConfig) ([]*schema.ToolInfo, error) {
	infos := make([]*schema.ToolInfo, 0, len(config.Tools))
	for _, toolValue := range config.Tools {
		info, err := toolValue.Info(ctx)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}
