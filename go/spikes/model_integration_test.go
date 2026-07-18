//go:build integration

package spikes

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"

	rushagent "github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/config"
	"github.com/nanzhi84/Rushes/go/internal/providers"
)

func requireLiveEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value != "" {
		return value
	}
	if os.Getenv("RUSHES_REQUIRE_LIVE_MODELS") == "1" {
		t.Fatalf("缺少必需的真实模型配置 %s", name)
	}
	t.Skipf("未配置 %s，跳过真实模型 spike", name)
	return ""
}

func TestQwenGenerateStreamToolAndReact(t *testing.T) {
	key := requireLiveEnv(t, "RUSHES_DASHSCOPE_API_KEY")
	ctx := t.Context()
	chatModel, err := providers.NewQwen(ctx, providers.QwenConfig{
		APIKey:  key,
		Model:   envOr("RUSHES_LLM_MODEL", providers.DefaultChatModel),
		Timeout: 180 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	nonStream, err := chatModel.Generate(ctx, []*schema.Message{
		schema.UserMessage("只输出 QWEN-NONSTREAM-OK，不要补充其他文字。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(nonStream.Content) == "" {
		t.Fatal("qwen 非流式返回为空")
	}

	stream, err := chatModel.Stream(ctx, []*schema.Message{
		schema.UserMessage("只输出 QWEN-STREAM-OK，不要补充其他文字。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := drainMessages(stream)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(joined.Content) == "" {
		t.Fatal("qwen 流式返回为空")
	}

	echo, err := utils.InferTool(
		"echo",
		"原样返回 value",
		func(_ context.Context, input echoInput) (echoOutput, error) {
			return echoOutput{Value: input.Value}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	info, err := echo.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := chatModel.WithTools([]*schema.ToolInfo{info})
	if err != nil {
		t.Fatal(err)
	}
	toolStream, err := withTools.Stream(
		ctx,
		[]*schema.Message{schema.UserMessage("调用 echo，value 必须是 qwen-tool-ok。")},
		model.WithToolChoice(schema.ToolChoiceForced, "echo"),
	)
	if err != nil {
		t.Fatal(err)
	}
	toolChunks, err := drainMessages(toolStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolChunks) == 0 {
		t.Fatal("qwen tool stream 无 chunk")
	}
	firstChunkHasTool := len(toolChunks[0].ToolCalls) > 0
	allToolMessage, err := schema.ConcatMessages(toolChunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(allToolMessage.ToolCalls) == 0 || allToolMessage.ToolCalls[0].Function.Name != "echo" {
		t.Fatalf("qwen 未生成 echo tool call: %#v", allToolMessage.ToolCalls)
	}
	t.Logf("qwen StreamToolCallChecker 观测：首 chunk 含 tool call=%t，总 chunk=%d", firstChunkHasTool, len(toolChunks))

	invokeCount := 0
	reactEcho, err := utils.InferTool(
		"echo",
		"原样返回 value；必须用它完成请求",
		func(_ context.Context, input echoInput) (echoOutput, error) {
			invokeCount++
			return echoOutput{Value: input.Value}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chatModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{reactEcho},
		},
		StreamToolCallChecker: rushagent.FullStreamToolCallChecker,
		MaxStep:               6,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentStream, err := agent.Stream(ctx, []*schema.Message{
		schema.SystemMessage("必须先调用 echo 工具，再用一句中文确认成功。"),
		schema.UserMessage("请用 echo 处理 react-ok。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drainMessages(agentStream); err != nil {
		t.Fatal(err)
	}
	if invokeCount == 0 {
		t.Fatal("真实 qwen ReAct 未调用 InferTool")
	}
}

func TestArkHTTPClientAndRetry(t *testing.T) {
	key := requireLiveEnv(t, "RUSHES_ARK_API_KEY")
	modelID := requireLiveEnv(t, "RUSHES_ARK_MODEL")
	retries := 1
	chatModel, err := providers.NewArk(t.Context(), providers.ArkConfig{
		APIKey:  key,
		Model:   modelID,
		Timeout: 120 * time.Second,
		Retries: retries,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chatModel.Generate(t.Context(), []*schema.Message{
		schema.UserMessage("只输出 ARK-SPIKE-OK，不要补充其他文字。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(response.Content) == "" {
		t.Fatal("ark 返回为空")
	}
}

// TestArkTiersAssemblySmoke 覆盖 #103 M2 新增的厂商开关装配路径：
// RUSHES_CHAT_PROVIDER=ark 时经 config.ResolveChatProvider + providers.NewArkTiers
// 装配聊天/视觉双档，并对聊天档做一次真实回合。默认跳过；仅在配置真实 ark 密钥、
// 且（缺配置时）RUSHES_REQUIRE_LIVE_MODELS=1 才强制运行。
func TestArkTiersAssemblySmoke(t *testing.T) {
	key := requireLiveEnv(t, "RUSHES_ARK_API_KEY")
	chatModel := requireLiveEnv(t, "RUSHES_ARK_CHAT_MODEL")
	visionModel := envOr("RUSHES_ARK_VISION_MODEL", chatModel)

	provider, err := config.ResolveChatProvider("ark")
	if err != nil {
		t.Fatal(err)
	}
	if provider != config.ProviderArk {
		t.Fatalf("provider=%q，期望 ark", provider)
	}

	tiers, err := providers.NewArkTiers(t.Context(), providers.ArkTierConfig{
		APIKey:      key,
		AccessKey:   os.Getenv("RUSHES_ARK_ACCESS_KEY"),
		SecretKey:   os.Getenv("RUSHES_ARK_SECRET_KEY"),
		BaseURL:     os.Getenv("RUSHES_ARK_BASE_URL"),
		Region:      os.Getenv("RUSHES_ARK_REGION"),
		ChatModel:   chatModel,
		VisionModel: visionModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tiers.Chat == nil || tiers.Vision == nil {
		t.Fatal("ark 双档装配缺少 chat 或 vision")
	}

	response, err := tiers.Chat.Generate(t.Context(), []*schema.Message{
		schema.UserMessage("只输出 ARK-TIERS-OK，不要补充其他文字。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(response.Content) == "" {
		t.Fatal("ark 双档聊天返回为空")
	}
	t.Logf("ARK_TIERS_ASSEMBLY_OK content_len=%d", len(response.Content))
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func drainMessages(stream *schema.StreamReader[*schema.Message]) ([]*schema.Message, error) {
	defer stream.Close()
	var messages []*schema.Message
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return messages, nil
		}
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
}
