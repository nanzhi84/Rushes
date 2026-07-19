package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestClassifyModelChunkFollowsFirstSignal(t *testing.T) {
	t.Parallel()

	if classifyModelChunk(nil) != modelRoundSignalNone {
		t.Fatal("nil 分片应视为前导")
	}
	if classifyModelChunk(&schema.Message{Role: schema.Assistant, ReasoningContent: "先想想"}) != modelRoundSignalNone {
		t.Fatal("纯思考前导（空 Content）应视为前导")
	}
	if classifyModelChunk(schema.AssistantMessage("答案", nil)) != modelRoundSignalText {
		t.Fatal("非空 Content 应判终态文本")
	}
	toolChunk := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call-1", Function: schema.FunctionCall{Name: "echo", Arguments: `{}`},
	}})
	if classifyModelChunk(toolChunk) != modelRoundSignalToolCall {
		t.Fatal("含 tool_call 应判工具调用轮")
	}
	// tool_call 优先于 Content：同一分片若两者都有，按工具调用轮处理（安全侧）。
	mixed := schema.AssistantMessage("附带解释", []schema.ToolCall{{
		ID: "call-2", Function: schema.FunctionCall{Name: "echo", Arguments: `{}`},
	}})
	if classifyModelChunk(mixed) != modelRoundSignalToolCall {
		t.Fatal("tool_call 与 Content 并存应优先判工具调用轮")
	}
}

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

func TestFullStreamToolCallCheckerStopsAtFirstVisibleText(t *testing.T) {
	t.Parallel()

	// 首个可见正文即判终态文本轮并早退：即便后续分片带 tool_call 也不再读取（直通流式的关键，
	// 依赖工具轮不会在 tool_call 前吐可见 Content 的约定）。用 Pipe 在正文后塞一个错误分片，
	// 若 checker 读到该分片就会返回错误——断言它没有，证明确实早退。
	reader, writer := schema.Pipe[*schema.Message](3)
	writer.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: "先想想"}, nil)
	writer.Send(schema.AssistantMessage("答案开始", nil), nil)
	writer.Send(nil, errors.New("早退后不应读到这里"))
	writer.Close()

	found, err := FullStreamToolCallChecker(context.Background(), reader)
	if found || err != nil {
		t.Fatalf("首个正文块后应早退返回 (false,nil)，got found=%v err=%v", found, err)
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
