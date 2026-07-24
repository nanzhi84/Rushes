package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
)

// delayedFinalReplyModel 的终态文本轮按 delay 逐块推送，用于验证直通流式：首 token 早于整轮
// 生成完毕，deltas 随生成过程分散到达而非在缓冲后一次性涌出。
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

func TestFinalReplyStreamsThroughIncrementally(t *testing.T) {
	t.Parallel()
	// H5/H7 交点：本用例用普通回复（不触发 H7 反思重述），故 text_delta 序列与 message_completed
	// 全文严格一致；若 H7 重述命中，message_completed 会被整体替换为重述版、与已流出的 delta 不同，
	// 那是 H7 的既定语义例外，不在本 golden 约束内。
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
		t.Fatalf("直通 delta 序列与 message_completed 不一致：deltas=%q completed=%q want=%q", joined, completedContent, want)
	}
	// 直通证明：deltas 随生成过程分散到达（首末间隔接近 3×delay）。若是缓冲后一次性涌出，间隔≈0。
	spread := lastDeltaAt.Sub(firstDeltaAt)
	if spread < 300*time.Millisecond {
		t.Fatalf("delta 首末间隔=%s 过小，疑似缓冲而非直通（期望≈%s）", spread, 3*chunkDelay)
	}
	// TTFT 验收：首 token 远早于整轮生成完毕（<1s 本地基准）。
	ttft := firstDeltaAt.Sub(start)
	if ttft >= time.Second {
		t.Fatalf("终态回复首 token 延迟=%s ≥ 1s，未达直通基准", ttft)
	}
	t.Logf("直通基准：TTFT=%s，delta 首末间隔=%s（%d 段 %s 间隔）", ttft, spread, len(chunks), chunkDelay)
}

// lateToolCallReplyModel 在终态文本轮里先吐可见正文、再吐一个 tool_call 分片，用于验证决策 2 的
// 观测保护：直通后晚到的 tool_call 被检测（计数 +1），且回合仍以正文正常收尾、不执行该工具。
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
		ID: "late_call", Function: schema.FunctionCall{Name: "timeline.check", Arguments: "{}"},
	}}), nil)
	writer.Close()
	return reader, nil
}

func TestPassThroughLateToolCallIsDetectedButTurnFinishes(t *testing.T) {
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
	var outcome any
	deadline := time.After(5 * time.Second)
	for outcome == nil {
		select {
		case event := <-stream:
			if event["type"] == TurnStreamMessageCompleted {
				completedContent, _ = event["content"].(string)
			}
			if event["type"] == TurnStreamTurnEnded {
				outcome = event["outcome"]
			}
		case <-deadline:
			t.Fatal("等待 turn_ended 超时")
		}
	}

	if outcome != "finished" || completedContent != "先给你个结论" {
		t.Fatalf("直通后晚到 tool_call 不应打断回合：outcome=%v content=%q", outcome, completedContent)
	}
	if after := passthroughLateToolCallCount.Load(); after != before+1 {
		t.Fatalf("晚到 tool_call 应被检测计数一次：before=%d after=%d", before, after)
	}
}
