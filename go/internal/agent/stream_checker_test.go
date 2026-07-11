package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFullStreamToolCallCheckerScansPastFirstChunk(t *testing.T) {
	t.Parallel()

	stream := schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("我先想一下", nil),
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
		t.Fatal("应发现第二个 chunk 中的 tool call")
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
