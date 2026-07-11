package spikes

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
)

type echoInput struct {
	Value string `json:"value" jsonschema:"required" jsonschema_description:"需要原样返回的文本"`
}

type echoOutput struct {
	Value string `json:"value"`
}

type scriptedToolModel struct {
	mu    sync.Mutex
	calls int
	tools []*schema.ToolInfo
}

func (m *scriptedToolModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = tools
	return m, nil
}

func (m *scriptedToolModel) Generate(
	_ context.Context,
	input []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++

	if m.calls == 1 {
		if len(m.tools) != 1 {
			return nil, errors.New("没有绑定唯一 echo 工具")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call-echo",
			Function: schema.FunctionCall{
				Name:      m.tools[0].Name,
				Arguments: `{"value":"eino-ok"}`,
			},
		}}), nil
	}

	if len(input) == 0 || input[len(input)-1].Role != schema.Tool {
		return nil, errors.New("第二轮没有收到工具结果")
	}
	return schema.AssistantMessage("REACT-SPIKE-OK", nil), nil
}

func (m *scriptedToolModel) Stream(
	ctx context.Context,
	input []*schema.Message,
	opts ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestReactNewAgentWithInferTool(t *testing.T) {
	t.Parallel()

	var invoked bool
	echo, err := utils.InferTool(
		"echo",
		"原样返回输入，用于验证 Eino ReAct 工具循环",
		func(_ context.Context, input echoInput) (echoOutput, error) {
			invoked = true
			return echoOutput(input), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	chatModel := &scriptedToolModel{}
	agent, err := react.NewAgent(t.Context(), &react.AgentConfig{
		ToolCallingModel: chatModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{echo},
		},
		MaxStep: 6,
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := agent.Generate(t.Context(), []*schema.Message{
		schema.UserMessage("请调用 echo 工具"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !invoked {
		t.Fatal("InferTool 未被 ReAct agent 调用")
	}
	if response.Content != "REACT-SPIKE-OK" {
		t.Fatalf("unexpected response: %q", response.Content)
	}
}
