package agent

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/schema"
)

// FullStreamToolCallChecker 会扫描完整模型流，修复默认 checker 只看首块的问题。
// Eino 会把模型流复制后传入 checker，因此这里消费并关闭传入流不会吞掉最终输出。
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
