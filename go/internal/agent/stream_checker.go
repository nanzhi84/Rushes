package agent

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/schema"
)

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

// FullStreamToolCallChecker 必须按完整 assistant 消息判定路由。Content 与 tool_call 在同一
// 消息里并不互斥，Claude/Qwen 都可能先流出可见正文再发 tool_call；因此正文绝不能提前把
// ReAct 图路由到 END。timeoutRetryChatModel 已在 provider 边界完整缓冲，checker 通常读取的
// 是内存流；这里扫到任意 tool_call 即进工具节点，只有 EOF 仍无工具时才结束回合。
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
		if message != nil && len(message.ToolCalls) > 0 {
			return true, nil
		}
	}
}
