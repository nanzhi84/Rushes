package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFullStreamToolCallCheckerScansPastReasoningPreamble(t *testing.T) {
	t.Parallel()

	// 空/思考前导分片不触发判定，checker 继续向后扫描直到真正的 tool_call。
	stream := schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "我先想一下"},
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-1",
			Function: schema.FunctionCall{
				Name:      "echo",
				Arguments: `{"value":"ok"}`,
			},
		}}),
	})
	found, err := FullStreamToolCallChecker(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("应扫过思考前导发现第二个 chunk 中的 tool call")
	}
}

func TestFullStreamToolCallCheckerScansPastVisibleText(t *testing.T) {
	t.Parallel()

	// 可见正文不是消息终点：Claude/Qwen 都可能在同一 assistant 消息中先叙述、后 tool_call。
	// checker 必须继续读取并把该轮路由到工具节点。
	reader, writer := schema.Pipe[*schema.Message](3)
	writer.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: "先想想"}, nil)
	writer.Send(schema.AssistantMessage("我先检查素材。", nil), nil)
	writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-after-text", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{}`},
	}}), nil)
	writer.Close()

	found, err := FullStreamToolCallChecker(context.Background(), reader)
	if !found || err != nil {
		t.Fatalf("正文后的 tool_call 应被识别，got found=%v err=%v", found, err)
	}
}

func TestFullStreamToolCallCheckerCancellationAndReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	found, err := FullStreamToolCallChecker(ctx, schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("不会读取", nil),
	}))
	if found || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel found=%v err=%v", found, err)
	}

	expected := errors.New("stream failed")
	broken := schema.StreamReaderWithConvert(
		schema.StreamReaderFromArray([]int{1}),
		func(int) (*schema.Message, error) { return nil, expected },
	)
	found, err = FullStreamToolCallChecker(t.Context(), broken)
	if found || !errors.Is(err, expected) {
		t.Fatalf("broken found=%v err=%v", found, err)
	}
}

func TestFullStreamToolCallCheckerNoTool(t *testing.T) {
	t.Parallel()

	stream := schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("只有文本", nil),
	})
	found, err := FullStreamToolCallChecker(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("纯文本流不应判定为 tool call")
	}
}
