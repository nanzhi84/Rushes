package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// delayedFinalReplyModel 的终态文本轮按 delay 逐块推送，用于验证终态门禁先缓冲完整正文，
// 再统一输出通过真值检查的内容。
type delayedFinalReplyModel struct {
	chunks []string
	delay  time.Duration
	usage  *schema.TokenUsage
}

func (stub *delayedFinalReplyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *delayedFinalReplyModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage(strings.Join(stub.chunks, ""), nil), nil
}

func (stub *delayedFinalReplyModel) Stream(
	ctx context.Context, _ []*schema.Message, _ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](len(stub.chunks) + 1)
	go func() {
		defer writer.Close()
		for index, chunk := range stub.chunks {
			if index > 0 {
				select {
				case <-time.After(stub.delay):
				case <-ctx.Done():
					return
				}
			}
			message := schema.AssistantMessage(chunk, nil)
			if index == len(stub.chunks)-1 && stub.usage != nil {
				message.ResponseMeta = &schema.ResponseMeta{Usage: stub.usage}
			}
			if closed := writer.Send(message, nil); closed {
				return
			}
		}
	}()
	return reader, nil
}

func TestFinalReplyStreamsOnlyAfterTerminalBufferCompletes(t *testing.T) {
	t.Parallel()
	// 普通回复不触发反思重述；text_delta 与 message_completed 必须严格一致。
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_passthrough")
	chunkDelay := 250 * time.Millisecond
	chunks := []string{"你好，", "我已经", "帮你把", "气口剪掉了。"}
	stub := &delayedFinalReplyModel{
		chunks: chunks, delay: chunkDelay,
		usage: &schema.TokenUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
	}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_passthrough")
	defer unsubscribe()

	start := time.Now()
	if !service.Queue().EnqueueUserMessage("draft_passthrough", "user_passthrough", "把气口剪掉") {
		t.Fatal("enqueue failed")
	}
	// 关键：不在读取前 JoinDraft——它会阻塞到回合结束，使所有 delta 在被读取前就已入队、读取
	// 时刻全部挤到回合末尾、时间戳失真。这里实时消费订阅流，delta 的到达时刻才反映真实流式节奏。

	var deltaTexts []string
	var firstDeltaAt, lastDeltaAt time.Time
	var completedContent string
	var sawCompleted bool
	deadline := time.After(10 * time.Second)
collect:
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case TurnStreamTextDelta:
				if firstDeltaAt.IsZero() {
					firstDeltaAt = time.Now()
				}
				lastDeltaAt = time.Now()
				deltaTexts = append(deltaTexts, event["delta"].(string))
			case TurnStreamMessageCompleted:
				completedContent, _ = event["content"].(string)
				sawCompleted = true
			case TurnStreamTurnEnded:
				break collect
			}
		case <-deadline:
			t.Fatal("等待 turn_ended 超时")
		}
	}
	service.Queue().JoinDraft("draft_passthrough")

	// golden 核心：text_delta 序列拼接必须与 message_completed 全文逐字一致。
	joined := strings.Join(deltaTexts, "")
	want := strings.Join(chunks, "")
	if !sawCompleted || completedContent != want || joined != want {
		t.Fatalf("门禁后 delta 序列与 message_completed 不一致：deltas=%q completed=%q want=%q", joined, completedContent, want)
	}
	// 终态正文必须先完整缓冲；放行后的 delta 会紧邻发出，不能在模型生成过程中提前泄漏。
	spread := lastDeltaAt.Sub(firstDeltaAt)
	if spread >= 100*time.Millisecond {
		t.Fatalf("门禁放行后的 delta 不应继续按模型生成延迟分散：%s", spread)
	}
	// 首 delta 只能在最后一个模型 chunk 到达后出现（3×delay）。
	ttft := firstDeltaAt.Sub(start)
	if ttft < 3*chunkDelay-100*time.Millisecond {
		t.Fatalf("终态正文在完整缓冲前泄漏：first_delta=%s want>=%s", ttft, 3*chunkDelay-100*time.Millisecond)
	}
	t.Logf("终态缓冲：first_delta=%s，放行后 delta spread=%s", ttft, spread)
}

// lateToolCallReplyModel 在终态文本轮里先吐成功正文、再吐一个 timeline.update 分片，
// 用于验证未执行的晚到工具调用会使整条缓冲回复失败。
type lateToolCallReplyModel struct{}

func (lateToolCallReplyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return lateToolCallReplyModel{}, nil
}

func (lateToolCallReplyModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage("先给你个结论", nil), nil
}

func (lateToolCallReplyModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](3)
	writer.Send(schema.AssistantMessage("先给你个结论", nil), nil)
	writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
		ID: "late_call", Function: schema.FunctionCall{Name: "timeline.update", Arguments: "{}"},
	}}), nil)
	writer.Close()
	return reader, nil
}

func TestBufferedFinalReplyWithLateToolCallFailsWithoutLeakingSuccess(t *testing.T) {
	// 不并行：passthroughLateToolCallCount 是包级计数器，串行才能对增量做精确断言。
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_late_toolcall")
	service, err := NewService(t.Context(), database, lateToolCallReplyModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_late_toolcall")
	defer unsubscribe()

	before := passthroughLateToolCallCount.Load()
	if !service.Queue().EnqueueUserMessage("draft_late_toolcall", "user_late", "剪一下") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft("draft_late_toolcall")

	var completedContent string
	var completedKind string
	var leakedSuccess bool
	var toolStarted bool
	var outcome any
	deadline := time.After(5 * time.Second)
	for outcome == nil {
		select {
		case event := <-stream:
			if event["type"] == TurnStreamTextDelta && strings.Contains(event["delta"].(string), "先给你个结论") {
				leakedSuccess = true
			}
			if event["type"] == TurnStreamToolStepStarted && event["tool"] == "timeline.update" {
				toolStarted = true
			}
			if event["type"] == TurnStreamMessageCompleted {
				completedContent, _ = event["content"].(string)
				completedKind, _ = event["kind"].(string)
			}
			if event["type"] == TurnStreamTurnEnded {
				outcome = event["outcome"]
			}
		case <-deadline:
			t.Fatal("等待 turn_ended 超时")
		}
	}

	if outcome != "failed" || completedKind != "turn_failure" ||
		!strings.Contains(completedContent, "该调用未被执行") {
		t.Fatalf("晚到 tool_call 必须确定性失败：outcome=%v kind=%q content=%q", outcome, completedKind, completedContent)
	}
	if leakedSuccess || toolStarted || strings.Contains(completedContent, "先给你个结论") {
		t.Fatalf("不得泄漏成功正文或伪装执行工具：leaked=%t tool_started=%t content=%q", leakedSuccess, toolStarted, completedContent)
	}
	if after := passthroughLateToolCallCount.Load(); after != before+1 {
		t.Fatalf("晚到 tool_call 应被检测计数一次：before=%d after=%d", before, after)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_late_toolcall", 20)
	if err != nil {
		t.Fatal(err)
	}
	final := messages[len(messages)-1]
	if final.Role != "system" || final.Kind != "turn_failure" || final.Content != completedContent {
		t.Fatalf("只能持久化确定性终态失败：%#v", final)
	}
}
