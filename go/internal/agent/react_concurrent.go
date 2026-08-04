package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	einoagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const (
	concurrentNodeModel     = "model"
	concurrentNodeTools     = "tools"
	concurrentNodePreviewQA = "preview_qa"
)

// concurrentReactState 累积本回合消息,供 modelPreHandle/toolsPreHandle 逐轮追加。不做 eino
// 中断/检查点序列化(Rushes 用自有 WorldState checkpoint,不用 eino 图中断)。
type concurrentReactState struct {
	Messages []*schema.Message
}

// concurrentReactAgent 是 eino react 图的 Rushes 复刻(#103 G3b 路线 2a)。唯一实质差异:把单个
// ToolsNode 换成按 Registry Spec 逐消息路由的 toolRouter——纯读与资源隔离 detector 并行，
// edit/control 及重复 detector 资源串行保序。模型侧语义全部原样保留:同一 H5 直通包装模型、
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
	specOf func(string) (rushestools.Spec, bool),
	maxStep int,
	toolCallChecker func(context.Context, *schema.StreamReader[*schema.Message]) (bool, error),
	messageModifier func(context.Context, []*schema.Message) []*schema.Message,
	previewQA *automaticPreviewQAController,
) (*concurrentReactAgent, error) {
	if toolCallChecker == nil {
		toolCallChecker = defaultStreamToolCallChecker
	}
	toolInfos, err := toolInfosFromConfig(ctx, toolsConfig)
	if err != nil {
		return nil, err
	}
	boundModel, err := einoagent.ChatModelWithTools(nil, chatModel, toolInfos)
	if err != nil {
		return nil, err
	}
	router, err := newToolRouter(ctx, toolsConfig, specOf)
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
	routerLambda := compose.InvokableLambdaWithOption(func(
		ctx context.Context,
		input *schema.Message,
		opts ...compose.ToolsNodeOption,
	) ([]*schema.Message, error) {
		return router.Invoke(ctx, input, opts...)
	})
	if err := graph.AddLambdaNode(concurrentNodeTools, routerLambda, compose.WithStatePreHandler(toolsPreHandle)); err != nil {
		return nil, err
	}
	if previewQA != nil {
		previewPreHandle := func(
			_ context.Context,
			input *schema.Message,
			state *concurrentReactState,
		) (*schema.Message, error) {
			if input == nil {
				return nil, errors.New("preview QA 缺少模型终态候选")
			}
			state.Messages = append(state.Messages, input)
			return input, nil
		}
		previewLambda := compose.InvokableLambda(func(
			ctx context.Context,
			candidate *schema.Message,
		) ([]*schema.Message, error) {
			var history []*schema.Message
			if err := compose.ProcessState[*concurrentReactState](ctx, func(
				_ context.Context,
				state *concurrentReactState,
			) error {
				history = append([]*schema.Message(nil), state.Messages...)
				return nil
			}); err != nil {
				return nil, err
			}
			report, err := previewQA.Run(ctx, history, candidate)
			if err != nil {
				return nil, err
			}
			if report == nil {
				return nil, errors.New("preview QA 未返回报告消息")
			}
			return []*schema.Message{report}, nil
		})
		if err := graph.AddLambdaNode(
			concurrentNodePreviewQA,
			previewLambda,
			compose.WithStatePreHandler(previewPreHandle),
		); err != nil {
			return nil, err
		}
	}

	// model → branch → {tools, preview_qa, END}。工具轮继续沿用 H5 早退；
	// 终态文本在启用自动 Preview QA 时会读完分支副本，以便只在真实交付声明边界
	// 插入 Harness 节点。候选正文不会提前流向用户。
	modelBranch := func(ctx context.Context, stream *schema.StreamReader[*schema.Message]) (string, error) {
		if previewQA != nil {
			return previewAwareModelBranch(ctx, stream, previewQA)
		}
		isToolCall, err := toolCallChecker(ctx, stream)
		if err != nil {
			return "", err
		}
		if isToolCall {
			return concurrentNodeTools, nil
		}
		return compose.END, nil
	}
	branchTargets := map[string]bool{concurrentNodeTools: true, compose.END: true}
	if previewQA != nil {
		branchTargets[concurrentNodePreviewQA] = true
	}
	if err := graph.AddBranch(concurrentNodeModel,
		compose.NewStreamGraphBranch(modelBranch, branchTargets)); err != nil {
		return nil, err
	}
	// Rushes 不设 ToolReturnDirectly:工具轮恒回到模型(省略 react 的 return-directly 分支)。
	if err := graph.AddEdge(concurrentNodeTools, concurrentNodeModel); err != nil {
		return nil, err
	}
	if previewQA != nil {
		if err := graph.AddEdge(concurrentNodePreviewQA, concurrentNodeModel); err != nil {
			return nil, err
		}
	}

	compiledMaxStep := maxStep
	if previewQA != nil {
		compiledMaxStep += 2 * maxAutomaticPreviewQAPassesPerTurn
	}
	runnable, err := graph.Compile(ctx,
		compose.WithMaxRunSteps(compiledMaxStep),
		compose.WithNodeTriggerMode(compose.AnyPredecessor),
	)
	if err != nil {
		return nil, err
	}
	return &concurrentReactAgent{runnable: runnable}, nil
}

func previewAwareModelBranch(
	ctx context.Context,
	stream *schema.StreamReader[*schema.Message],
	previewQA *automaticPreviewQAController,
) (string, error) {
	defer stream.Close()
	var content strings.Builder
	terminalTextSeen := false
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if message == nil {
			continue
		}
		if len(message.ToolCalls) > 0 {
			// timeoutRetryChatModel 已在 provider 边界读完整条消息。
			// 正文后出现 tool_call 是 Claude/Qwen 的合法流式形态，
			// 正文已由 modelStreamPreviewSession 持久化为 narration。
			return concurrentNodeTools, nil
		}
		if message.Content != "" {
			terminalTextSeen = true
			content.WriteString(message.Content)
		}
	}
	if !terminalTextSeen {
		return compose.END, nil
	}
	candidate := schema.AssistantMessage(content.String(), nil)
	var history []*schema.Message
	if err := compose.ProcessState[*concurrentReactState](ctx, func(
		_ context.Context,
		state *concurrentReactState,
	) error {
		history = append([]*schema.Message(nil), state.Messages...)
		return nil
	}); err != nil {
		return "", err
	}
	shouldRun, err := previewQA.ShouldRun(ctx, history, candidate)
	if err != nil {
		modelStreamPreviewFromContext(ctx).discardPending()
		return "", err
	}
	if shouldRun {
		// 候选已触发自动 Stop Gate / Preview QA，说明它尚不是可交付终态。
		// 在 Harness 继续检查并把反馈回灌模型前先撤销可见 preview，避免旧的
		// “已完成”正文在后续修复期间以 streaming 消息残留。
		modelStreamPreviewFromContext(ctx).discardPending()
		// Stream 用量已由 timeoutRetryChatModel 在完整 provider 消息
		// 边界统一记账，这里只负责路由，避免 Preview QA 重复累计。
		return concurrentNodePreviewQA, nil
	}
	return compose.END, nil
}

// Generate 与 Stream 与 react.Agent 对齐:分别走底层 runnable 的 Invoke/Stream。生产 turn 流走
// Stream(H5 直通);脚本模型测试走 Generate。
func (reactAgent *concurrentReactAgent) Generate(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einoagent.AgentOption,
) (*schema.Message, error) {
	return reactAgent.runnable.Invoke(ctx, messages, einoagent.GetComposeOptions(opts...)...)
}

func (reactAgent *concurrentReactAgent) Stream(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einoagent.AgentOption,
) (*schema.StreamReader[*schema.Message], error) {
	return reactAgent.runnable.Stream(ctx, messages, einoagent.GetComposeOptions(opts...)...)
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
