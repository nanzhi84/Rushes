package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// gatedFinalReplyModel 在每个后续分片前等待测试放行，用于无墙钟地证明首个 text_delta
// 到达时 provider 消息尚未结束。
type gatedFinalReplyModel struct {
	chunks  []string
	advance chan struct{}
	usage   *schema.TokenUsage
}

func (stub *gatedFinalReplyModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *gatedFinalReplyModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage(strings.Join(stub.chunks, ""), nil), nil
}

func (stub *gatedFinalReplyModel) Stream(
	ctx context.Context, _ []*schema.Message, _ ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](len(stub.chunks) + 1)
	go func() {
		defer writer.Close()
		for index, chunk := range stub.chunks {
			if index > 0 {
				select {
				case <-stub.advance:
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

func TestFinalReplyStreamsBeforeTerminalMessageCompletes(t *testing.T) {
	t.Parallel()
	// 普通回复不触发反思重述；provider text_delta 实时到达，最终全文仍由门禁后
	// message_completed 权威收口。
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_passthrough")
	chunks := []string{"你好，", "我已经", "帮你把", "气口剪掉了。"}
	stub := &gatedFinalReplyModel{
		chunks: chunks, advance: make(chan struct{}),
		usage: &schema.TokenUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
	}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_passthrough")
	defer unsubscribe()

	if !service.Queue().EnqueueUserMessage("draft_passthrough", "user_passthrough", "把气口剪掉") {
		t.Fatal("enqueue failed")
	}
	// 关键：不在读取前 JoinDraft——它会阻塞到回合结束，使所有 delta 在被读取前就已入队、读取
	// 时刻全部挤到回合末尾、时间戳失真。这里实时消费订阅流，delta 的到达时刻才反映真实流式节奏。

	var deltaTexts []string
	var completedContent string
	var sawCompleted bool
	deltaIndex := 0
	deadline := time.After(10 * time.Second)
collect:
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case TurnStreamTextDelta:
				if deltaIndex >= len(chunks) || event["delta"] != chunks[deltaIndex] {
					t.Fatalf("第 %d 个实时分片=%q want=%q", deltaIndex, event["delta"], chunks[deltaIndex])
				}
				deltaTexts = append(deltaTexts, event["delta"].(string))
				deltaIndex++
				if deltaIndex < len(chunks) {
					// 此时 provider 正阻塞在下一个分片前；能先收到当前 delta 就证明不是
					// 回合结束后的伪流式重放。
					stub.advance <- struct{}{}
				}
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
}

// textThenToolModel 首轮先吐叙述正文，再发 asset.list_assets；第二轮在真实工具结果后
// 生成最终回复，精确覆盖 Claude Code 的 text → tool_use → tool_result → text 形态。
type textThenToolModel struct {
	mu    sync.Mutex
	calls int
}

func (stub *textThenToolModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *textThenToolModel) Generate(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage("素材已经检查完毕。", nil), nil
}

func (stub *textThenToolModel) Stream(
	context.Context, []*schema.Message, ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	stub.mu.Lock()
	stub.calls++
	round := stub.calls
	stub.mu.Unlock()
	if round > 1 {
		return schema.StreamReaderFromArray([]*schema.Message{
			schema.AssistantMessage("素材已经检查完毕。", nil),
		}), nil
	}
	reader, writer := schema.Pipe[*schema.Message](3)
	writer.Send(schema.AssistantMessage("我先检查一下素材。", nil), nil)
	writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call_list", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: "{}"},
	}}), nil)
	writer.Close()
	return reader, nil
}

func TestTextBeforeToolCallStreamsAndStillExecutesTool(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_late_toolcall")
	stub := &textThenToolModel{}
	service, err := NewService(t.Context(), database, stub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_late_toolcall")
	defer unsubscribe()

	if !service.Queue().EnqueueUserMessage("draft_late_toolcall", "user_late", "剪一下") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft("draft_late_toolcall")

	sequence := make([]string, 0, 8)
	var finalContent string
	var finalReplacement string
	var sawDiscard bool
	var outcome any
	deadline := time.After(5 * time.Second)
	for outcome == nil {
		select {
		case event := <-stream:
			switch event["type"] {
			case TurnStreamTextDelta:
				sequence = append(sequence, "text:"+event["delta"].(string))
			case TurnStreamMessageDiscarded:
				sawDiscard = true
			case TurnStreamMessageCompleted:
				kind, _ := event["kind"].(string)
				sequence = append(sequence, "complete:"+kind)
				if kind == "reply" {
					finalContent, _ = event["content"].(string)
					finalReplacement, _ = event["replaces_message_id"].(string)
				}
			case TurnStreamToolStepStarted:
				sequence = append(sequence, "tool:"+event["tool"].(string))
			}
			if event["type"] == TurnStreamTurnEnded {
				outcome = event["outcome"]
			}
		case <-deadline:
			t.Fatal("等待 turn_ended 超时")
		}
	}

	joined := strings.Join(sequence, "|")
	for _, ordered := range []string{
		"text:我先检查一下素材。", "complete:narration", "tool:asset.list_assets",
		"text:素材已经检查完毕。", "complete:reply",
	} {
		if !strings.Contains(joined, ordered) {
			t.Fatalf("缺少 Claude Code 流式阶段 %q：%s", ordered, joined)
		}
	}
	if strings.Index(joined, "text:我先检查一下素材。") >= strings.Index(joined, "complete:narration") ||
		strings.Index(joined, "complete:narration") >= strings.Index(joined, "tool:asset.list_assets") ||
		strings.Index(joined, "tool:asset.list_assets") >= strings.LastIndex(joined, "text:素材已经检查完毕。") ||
		strings.LastIndex(joined, "text:素材已经检查完毕。") >= strings.LastIndex(joined, "complete:reply") {
		t.Fatalf("事件顺序不是 text→tool→text：%s", joined)
	}
	if outcome != "finished" || finalContent != "素材已经检查完毕。" ||
		finalReplacement == "" || sawDiscard || stub.calls != 2 {
		t.Fatalf("outcome=%v final=%q replacement=%q discarded=%t calls=%d sequence=%s",
			outcome, finalContent, finalReplacement, sawDiscard, stub.calls, joined)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_late_toolcall", 20)
	if err != nil {
		t.Fatal(err)
	}
	var narrationIndex, toolIndex, replyIndex = -1, -1, -1
	for index, message := range messages {
		switch message.Kind {
		case "narration":
			narrationIndex = index
		case "tool":
			toolIndex = index
		case "reply":
			replyIndex = index
		}
	}
	if narrationIndex < 0 || toolIndex <= narrationIndex || replyIndex <= toolIndex ||
		messages[narrationIndex].Content != "我先检查一下素材。" ||
		messages[replyIndex].Content != finalContent {
		t.Fatalf("刷新后的持久化顺序不是 narration→tool→reply：%#v", messages)
	}
}
