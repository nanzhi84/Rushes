package agent

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/cloudwego/eino/schema"
)

// modelRoundSignal 表示单个模型流分片对「本轮是工具调用轮还是终态文本轮」的判定意义。它是
// 终态回复直通流式（#95 H5）的责任分界事实源：model_retry.go 的 Stream 直通分类与本文件的
// StreamToolCallChecker 路由判定共用同一规则，两处必须逐块一致——否则会出现 Stream 已直通、
// checker 却路由到工具节点（或反之）的错配。
type modelRoundSignal int

const (
	// modelRoundSignalNone 是前导分片（空增量、纯思考 ReasoningContent、未闭合占位），不触发
	// 判定，继续向后扫描。
	modelRoundSignalNone modelRoundSignal = iota
	// modelRoundSignalToolCall 是出现 tool_call 的分片，本轮判定为工具调用轮。
	modelRoundSignalToolCall
	// modelRoundSignalText 是出现可见正文的分片，本轮判定为终态文本轮。
	modelRoundSignalText
)

// passthroughLateToolCallCount 记录「终态轮直通后仍出现 tool_call 分片」的次数——即
// classifyModelChunk 依赖的「工具轮不在 tool_call 前吐可见 Content」假设被真实模型违反的次数。
// 它让这个假设在生产上可证伪：常态应恒为 0，一旦非 0 就说明某模型会在一轮里先吐正文再发
// tool_call，而该 tool_call 因该轮已被判终态、已直通而不会被执行。streamAgent 在消费终态流时
// 检测并 +1（同时 slog.Warn），H3 度量落地后据此聚合暴露（#95 H5，决策 2 观测保护）。
var passthroughLateToolCallCount atomic.Int64

// defaultStreamToolCallChecker 与 Eino ReAct 默认 checker 对齐：跳过前导空块，只看
// 第一块可判定内容。自建并发图允许调用方传 nil 时必须回退到它，不能 nil dereference。
func defaultStreamToolCallChecker(
	_ context.Context,
	stream *schema.StreamReader[*schema.Message],
) (bool, error) {
	defer stream.Close()
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if message != nil && len(message.ToolCalls) > 0 {
			return true, nil
		}
		if message == nil || len(message.Content) == 0 {
			continue
		}
		return false, nil
	}
}

// classifyModelChunk 按「先到先判」规则归类单个分片：首个 tool_call 判工具调用轮、首个非空
// Content 判终态文本轮，二者都未出现时是前导分片。它依赖当前工具模型的约定——真正的工具轮在
// tool_call 之前不会吐可见 Content（思考走 ReasoningContent）。这个约定让终态文本轮能在首个
// 正文块处提前判定、从而直通首 token；代价是若某模型在一轮里先吐 Content 再发 tool_call，会被
// 误判成终态轮（该 tool_call 不执行）。后果有界——用户看到未执行工具的正文、可在下一轮继续，
// H2 失败留痕健在——但不做静默假设：streamAgent 会检测直通后晚到的 tool_call 并告警计数
// （passthroughLateToolCallCount），让该假设可证伪。当前 dashscope/ark 工具模型均满足该约定。
func classifyModelChunk(message *schema.Message) modelRoundSignal {
	if message == nil {
		return modelRoundSignalNone
	}
	if len(message.ToolCalls) > 0 {
		return modelRoundSignalToolCall
	}
	if message.Content != "" {
		return modelRoundSignalText
	}
	return modelRoundSignalNone
}

// FullStreamToolCallChecker 判定模型流是否发起工具调用，并在判定确定时立即返回，不把整条流读到
// EOF。默认 checker 只看首块；本实现扫描到「首个 tool_call」或「首个可见正文块」为止：
//   - 命中 tool_call → true，eino 路由到工具节点；
//   - 命中可见正文 → false，eino 立刻路由到 END，让终态回复直通给用户（#95 H5）；
//     在此之前的空/思考前导分片不触发判定，继续扫描（修复默认 checker 只看首块的问题）。
//
// Eino 会把模型流复制后传入 checker，因此这里消费并关闭传入流不会吞掉最终输出。早退是直通流式
// 首 token 低延迟的关键：分支无需等整轮生成完毕即可路由。分类与 model_retry.go 的直通判定共用
// classifyModelChunk 保持逐块一致。
func FullStreamToolCallChecker(
	ctx context.Context,
	stream *schema.StreamReader[*schema.Message],
) (bool, error) {
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}

		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch classifyModelChunk(message) {
		case modelRoundSignalToolCall:
			return true, nil
		case modelRoundSignalText:
			return false, nil
		}
	}
}
