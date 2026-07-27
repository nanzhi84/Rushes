package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type serviceToolModel struct {
	mu    sync.Mutex
	calls int
	tools []*schema.ToolInfo
}

type toolRoundBudgetModel struct {
	mu           sync.Mutex
	targetRounds int
	modelCalls   int
	toolRounds   int
	prompts      []string
}

type selfRepairServiceModel struct {
	mu    sync.Mutex
	calls int
}

type loopingFailureServiceModel struct{}

type failingServiceModel struct{}

type emptyServiceModel struct{}

type blockingFallbackScaffold struct{}

func (blockingFallbackScaffold) TryHandle(
	ctx context.Context,
	_, _, _ string,
) (string, bool, error) {
	<-ctx.Done()
	return "", true, ctx.Err()
}

type terminatingFailureLoopModel struct {
	mu          sync.Mutex
	calls       int
	terminalErr bool
}

type failingReadToolServiceModel struct {
	mu    sync.Mutex
	calls int
}

type decisionContinuationModel struct {
	mu       sync.Mutex
	messages []*schema.Message
}

func (modelValue *decisionContinuationModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *decisionContinuationModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	modelValue.messages = append([]*schema.Message(nil), messages...)
	modelValue.mu.Unlock()
	return schema.AssistantMessage("DECISION-CONTINUED", nil), nil
}

func (modelValue *decisionContinuationModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *decisionContinuationModel) lastPrompt() string {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	if len(modelValue.messages) == 0 {
		return ""
	}
	return modelValue.messages[len(modelValue.messages)-1].Content
}

func (modelValue *failingServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (*failingServiceModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("model failed")
}

func (*failingServiceModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("model failed")
}

func (*emptyServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return &emptyServiceModel{}, nil
}

func (*emptyServiceModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("", nil), nil
}

func (modelValue *emptyServiceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

// cancelDuringModelExecModel 在模型执行中阻塞，直到 turn 上下文被取消，然后返回一个
// 不包裹 context.Canceled 的普通传输错误（模拟 provider 连接中断）。用于复现“取消发生
// 在模型执行中、错误不携带 Canceled”这条 fallback 路径覆盖不到的风险路径。
type cancelDuringModelExecModel struct {
	entered chan struct{}
	once    sync.Once
}

// cancelDuringReflectionModel 先返回一段会触发 H7 重述的终态正文，再把重述调用阻塞到取消，
// 用于稳定命中 runTurn 初次取消检查之后、最终 durable commit 之前的竞态窗口。
type cancelDuringReflectionModel struct {
	entered sync.Once
	ready   chan struct{}
}

func (modelValue *cancelDuringReflectionModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *cancelDuringReflectionModel) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.entered.Do(func() { close(modelValue.ready) })
	<-ctx.Done()
	return nil, errors.New("provider disconnected during reflection")
}

func (modelValue *cancelDuringReflectionModel) Stream(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{
		schema.AssistantMessage("但等等，让我再确认已经全部完成。", nil),
	}), nil
}

func (modelValue *cancelDuringModelExecModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *cancelDuringModelExecModel) Generate(
	ctx context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.once.Do(func() { close(modelValue.entered) })
	<-ctx.Done()
	return nil, errors.New("provider 连接中断")
}

func (modelValue *cancelDuringModelExecModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *terminatingFailureLoopModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *terminatingFailureLoopModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	if modelValue.calls <= maxModelRepairAttempts+1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: agentexec.RandomID("bounded_loop_call"),
			Function: schema.FunctionCall{
				Name: "timeline.nonexistent", Arguments: `{"same":true}`,
			},
		}}), nil
	}
	if modelValue.terminalErr {
		return nil, errors.New("provider stream failed after recovery exhaustion")
	}
	return schema.AssistantMessage("已经全部完成。", nil), nil
}

func (modelValue *terminatingFailureLoopModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *serviceToolModel) WithTools(infos []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.tools = infos
	return modelValue, nil
}

func (modelValue *serviceToolModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	if modelValue.calls == 1 {
		found := false
		for _, info := range modelValue.tools {
			if info.Name == "asset.list_assets" {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("asset.list_assets 未绑定")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "call_list", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{}`},
		}}), nil
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool {
		return nil, errors.New("工具结果未回灌模型")
	}
	return schema.AssistantMessage("EINO-SERVICE-OK", nil), nil
}

func (modelValue *serviceToolModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *toolRoundBudgetModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *toolRoundBudgetModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.modelCalls++
	if len(messages) == 0 || messages[0].Role != schema.System {
		return nil, errors.New("工具预算测试缺少系统提示")
	}
	modelValue.prompts = append(modelValue.prompts, messages[0].Content)
	if len(messages) > 0 && messages[len(messages)-1].Role == schema.Tool {
		modelValue.toolRounds++
	}
	targetRounds := modelValue.targetRounds
	if targetRounds <= 0 {
		targetRounds = maxToolRoundsPerTurn
	}
	if modelValue.toolRounds < targetRounds {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: agentexec.RandomID("budget_round_call"),
			Function: schema.FunctionCall{
				Name: "asset.list_assets", Arguments: `{}`,
			},
		}}), nil
	}
	return schema.AssistantMessage(fmt.Sprintf("%d 次工具往返完成。", targetRounds), nil), nil
}

func (modelValue *toolRoundBudgetModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *toolRoundBudgetModel) snapshot() (int, int, []string) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	return modelValue.modelCalls, modelValue.toolRounds, append([]string(nil), modelValue.prompts...)
}

func (modelValue *selfRepairServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *selfRepairServiceModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	switch modelValue.calls {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "bad_call", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{`},
		}}), nil
	case 2:
		if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool ||
			!strings.Contains(messages[len(messages)-1].Content, `"status":"failed"`) {
			return nil, errors.New("工具参数错误没有回灌模型")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "fixed_call", Function: schema.FunctionCall{Name: "asset.list_assets", Arguments: `{}`},
		}}), nil
	default:
		if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool {
			return nil, errors.New("修复后的工具结果没有回灌模型")
		}
		return schema.AssistantMessage("已读取真实工具错误并自行修复。", nil), nil
	}
}

func (modelValue *selfRepairServiceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *loopingFailureServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (*loopingFailureServiceModel) Generate(
	context.Context,
	[]*schema.Message,
	...model.Option,
) (*schema.Message, error) {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       agentexec.RandomID("loop_call"),
		Function: schema.FunctionCall{Name: "timeline.nonexistent", Arguments: `{"same":true}`},
	}}), nil
}

func (modelValue *loopingFailureServiceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func (modelValue *failingReadToolServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *failingReadToolServiceModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	if modelValue.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "missing_audio", Function: schema.FunctionCall{
				Name: "audio.analyze_beats", Arguments: `{"asset_id":"asset_missing"}`,
			},
		}}), nil
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool ||
		!strings.Contains(messages[len(messages)-1].Content, `"automatic_retries":0`) ||
		!strings.Contains(messages[len(messages)-1].Content, `"retryable":false`) {
		return nil, errors.New("确定性参数失败没有立即回灌模型")
	}
	return schema.AssistantMessage("已经全部完成。", nil), nil
}

func (modelValue *failingReadToolServiceModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := modelValue.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func TestServiceRunsProductionReactAgentAndPersistsStreamedReply(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_react")
	agenttest.InsertAgentMessage(t, database, "draft_react", "user_msg", "列出素材")
	service, err := NewService(t.Context(), database, &serviceToolModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if service.react == nil {
		t.Fatal("配置模型后未创建 react.Agent")
	}
	_, stream, unsubscribe := service.Hub().Subscribe("draft_react")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage("draft_react", "user_msg", "列出素材") {
		t.Fatal("enqueue 失败")
	}
	service.Queue().JoinDraft("draft_react")
	types := map[string]bool{}
	for {
		select {
		case event := <-stream:
			typeName, _ := event["type"].(string)
			types[typeName] = true
			if typeName == "turn_ended" {
				goto done
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待 turn_ended 超时")
		}
	}
done:
	for _, expected := range []string{
		"turn_started", "tool_step_started", "tool_step_finished",
		"text_delta", "message_completed", "turn_ended",
	} {
		if !types[expected] {
			t.Fatalf("缺少 %s，events=%v", expected, types)
		}
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_react", 20)
	if err != nil || len(messages) != 3 || messages[1].Kind != "tool" ||
		messages[2].Content != "EINO-SERVICE-OK" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	modelMessages, modelErr := service.modelMessages(t.Context(), "draft_react")
	if modelErr != nil || len(modelMessages) != 3 || modelMessages[0].Role != schema.System ||
		modelMessages[1].Role != schema.User || modelMessages[2].Role != schema.Assistant ||
		!strings.Contains(modelMessages[2].Content, "EINO-SERVICE-OK") {
		t.Fatalf("tool trace 不应进入模型上下文: messages=%#v err=%v", modelMessages, modelErr)
	}
	for _, message := range modelMessages {
		if strings.Contains(message.Content, `"step_id"`) || strings.Contains(message.Content, `"args_summary"`) {
			t.Fatalf("UI tool trace 泄漏进模型上下文: %#v", message)
		}
	}
}

func TestReactAgentMakesBudgetVisibleAndAllowsFortyToolRounds(t *testing.T) {
	t.Parallel()
	if maxToolRoundsPerTurn != 40 || maxReActStepsPerTurn != reactStepsForToolRounds(maxToolRoundsPerTurn) {
		t.Fatalf(
			"budget policy hard=%d steps=%d",
			maxToolRoundsPerTurn, maxReActStepsPerTurn,
		)
	}
	for rounds, want := range map[int]int{-1: 1, 0: 1, 1: 3, 40: 81} {
		if got := reactStepsForToolRounds(rounds); got != want {
			t.Fatalf("reactStepsForToolRounds(%d)=%d want=%d", rounds, got, want)
		}
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_tool_round_budget")
	agenttest.InsertAgentMessage(t, database, "draft_tool_round_budget", "user_tool_round_budget", "连续执行多轮工具")
	modelValue := &toolRoundBudgetModel{}
	service, err := NewService(t.Context(), database, modelValue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_tool_round_budget")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage(
		"draft_tool_round_budget", "user_tool_round_budget", "连续执行多轮工具",
	) {
		t.Fatal("enqueue 失败")
	}
	service.Queue().JoinDraft("draft_tool_round_budget")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("40 次工具往返不应触发步数上限")
			case "message_completed":
				completed, _ = event["content"].(string)
			case "turn_ended":
				modelCalls, toolRounds, prompts := modelValue.snapshot()
				if event["outcome"] != "finished" || completed != "40 次工具往返完成。" ||
					modelCalls != 41 || toolRounds != 40 || len(prompts) != 41 {
					t.Fatalf(
						"completed=%q event=%#v model_calls=%d tool_rounds=%d prompts=%d",
						completed, event, modelCalls, toolRounds, len(prompts),
					)
				}
				for index := 0; index < 35; index++ {
					if prompts[index] != coreSystemPrompt {
						t.Fatalf("model call %d unexpectedly contains budget noise", index+1)
					}
				}
				if !strings.Contains(prompts[35], "工具预算提醒") ||
					!strings.Contains(prompts[35], "剩余 5 次") ||
					strings.Contains(prompts[35], "禁止再调工具") {
					t.Fatalf("model call 36 prompt=%q", prompts[35])
				}
				for index := 40; index < len(prompts); index++ {
					if !strings.Contains(prompts[index], "最后一次生成机会") ||
						!strings.Contains(prompts[index], "禁止再调工具") {
						t.Fatalf("model call %d prompt=%q", index+1, prompts[index])
					}
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("40 次工具往返后没有生成最终回复")
		}
	}
}

func TestReactAgentThirtyRoundFixtureWarnsOnCallsTwentySixAndThirtyOne(t *testing.T) {
	t.Parallel()
	const fixtureRounds = 30
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_thirty_round_fixture"
	agenttest.CreateAgentDraft(t, database, draftID)
	modelValue := &toolRoundBudgetModel{targetRounds: fixtureRounds}
	service, err := NewService(t.Context(), database, modelValue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := withTurnBudgetState(t.Context(), newTurnBudgetState(fixtureRounds))
	ctx = rushestools.WithDraftID(ctx, draftID)
	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("执行三十轮工具后收敛"),
	})
	if err != nil {
		t.Fatal(err)
	}
	modelCalls, toolRounds, prompts := modelValue.snapshot()
	if response.Content != "30 次工具往返完成。" || modelCalls != 31 ||
		toolRounds != 30 || len(prompts) != 31 {
		t.Fatalf(
			"response=%q model_calls=%d tool_rounds=%d prompts=%d",
			response.Content, modelCalls, toolRounds, len(prompts),
		)
	}
	for index := 0; index < 25; index++ {
		if prompts[index] != coreSystemPrompt {
			t.Fatalf("model call %d unexpectedly contains budget noise", index+1)
		}
	}
	if !strings.Contains(prompts[25], "工具预算提醒") ||
		!strings.Contains(prompts[25], "剩余 5 次") ||
		strings.Contains(prompts[25], "禁止再调工具") {
		t.Fatalf("model call 26 prompt=%q", prompts[25])
	}
	if !strings.Contains(prompts[30], "最后一次生成机会") ||
		!strings.Contains(prompts[30], "禁止再调工具") {
		t.Fatalf("model call 31 prompt=%q", prompts[30])
	}
}

func TestServiceReturnsToolFailureToModelForSelfRepair(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_self_repair")
	agenttest.InsertAgentMessage(t, database, "draft_self_repair", "user_self_repair", "先试错再修复")
	service, err := NewService(t.Context(), database, &selfRepairServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_self_repair")
	defer unsubscribe()
	if !service.Queue().EnqueueUserMessage("draft_self_repair", "user_self_repair", "先试错再修复") {
		t.Fatal("enqueue 失败")
	}
	service.Queue().JoinDraft("draft_self_repair")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("可修复的工具失败不应中断回合")
			case "message_completed":
				completed, _ = event["content"].(string)
			case "turn_ended":
				if completed != "已读取真实工具错误并自行修复。" {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("自修复回合未结束")
		}
	}
}

func TestServiceRejectsSuccessClaimAfterUnresolvedToolFailure(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_retry_trace")
	assetResult, assetErr := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_present", "job_id": "job_present", "kind": "video",
			"filename": "present.mp4", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: "draft_retry_trace", Payload: map[string]any{
			"asset_id": "asset_present",
		}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if assetErr != nil || assetResult.Status != reducer.StatusApplied {
		t.Fatalf("asset status=%s err=%v", assetResult.Status, assetErr)
	}
	agenttest.InsertAgentMessage(t, database, "draft_retry_trace", "user_retry_trace", "分析不存在的音频")
	service, err := NewService(t.Context(), database, &failingReadToolServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueUserMessage("draft_retry_trace", "user_retry_trace", "分析不存在的音频") {
		t.Fatal("enqueue 失败")
	}
	service.Queue().JoinDraft("draft_retry_trace")
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_retry_trace", 20)
	if err != nil {
		t.Fatal(err)
	}
	toolRows := 0
	for _, message := range messages {
		if message.Kind == "tool" {
			toolRows++
		}
	}
	final := messages[len(messages)-1]
	if toolRows != 1 || len(messages) != 3 || final.Role != "system" || final.Kind != "turn_failure" ||
		!strings.Contains(final.Content, "工具调用仍处于失败状态") || strings.Contains(final.Content, "已经全部完成") {
		t.Fatalf("未解决工具失败必须拒绝伪成功：tool_rows=%d messages=%#v", toolRows, messages)
	}
}

func TestDecisionAnswerObservationResumesAgent(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_decision_continue")
	agenttest.InsertAgentMessage(t, database, "draft_decision_continue", "user_decision_continue", "帮我做一个混剪")
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_decision_continue")
	result, err := service.ExecuteTool(ctx, "interaction.ask_user", rushestools.AskUserInput{
		Question:     "当前目标存在无法推断的关键风格冲突，请选择核心方向。",
		DecisionType: "critical",
		Options: []rushestools.DecisionOptionInput{{
			OptionID: "cinematic", Label: "电影感叙事",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionID := result.(rushestools.ToolResult).Data["decision_id"].(string)
	decision, err := storage.GetDecision(t.Context(), database.Read(), decisionID)
	if err != nil {
		t.Fatal(err)
	}
	if decision.PendingToolCall != nil || decision.PendingToolCallStatus != nil {
		t.Fatalf("普通选择不应伪装成待重放工具: %#v", decision)
	}
	if _, err := service.ExecuteTool(ctx, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "cinematic",
	}); err != nil {
		t.Fatal(err)
	}
	if !service.Queue().EnqueueUIObservation(
		"draft_decision_continue",
		"decision_resume_test",
		"decision_answered",
		map[string]any{
			"decision_id": decisionID,
			"answer":      map[string]any{"option_id": "cinematic"},
		},
	) {
		t.Fatal("决策回答未入队")
	}
	service.Queue().JoinDraft("draft_decision_continue")
	prompt := chatModel.lastPrompt()
	if !strings.Contains(prompt, "当前目标存在无法推断的关键风格冲突") ||
		!strings.Contains(prompt, "电影感叙事") ||
		!strings.Contains(prompt, "不要重复提出已经回答的问题") {
		t.Fatalf("续跑提示缺少已回答决策上下文: %q", prompt)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_decision_continue", 20)
	if err != nil || len(messages) < 2 || messages[len(messages)-1].Content != "DECISION-CONTINUED" {
		t.Fatalf("回答后未生成继续创作消息: messages=%#v err=%v", messages, err)
	}
}

func TestTerminalJobObservationResumesAgentWithFailureContext(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_job_continue")
	agenttest.InsertAgentMessage(t, database, "draft_job_continue", "user_job_continue", "生成预览并检查")
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueJobObservation(
		"draft_job_continue",
		"job_failed",
		map[string]any{
			"event": "JobFailed",
			"payload": map[string]any{
				"job_id": "job_failed", "kind": "render_preview",
				"error": map[string]any{"message": "音轨越界"},
			},
		},
	) {
		t.Fatal("job observation 未入队")
	}
	service.Queue().JoinDraft("draft_job_continue")
	prompt := chatModel.lastPrompt()
	if !strings.Contains(prompt, "render_preview") || !strings.Contains(prompt, "音轨越界") ||
		!strings.Contains(prompt, "自动续跑") || !strings.Contains(prompt, "修复并重试") {
		t.Fatalf("job 终态续跑提示不完整: %q", prompt)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_job_continue", 20)
	if err != nil || len(messages) < 2 || messages[len(messages)-1].Kind != "reply" {
		t.Fatalf("job 续跑结果应作为正常回复: messages=%#v err=%v", messages, err)
	}
}

func TestCompletedPreviewObservationSkipsDuplicateInspection(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_job_dedup")
	agenttest.InsertAgentMessage(t, database, "draft_job_dedup", "user_job_dedup", "渲染预览并检查")
	var result reducer.Result
	for index, check := range []string{"decode", "black", "freeze", "silence", "loudness"} {
		var err error
		result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
				ID: fmt.Sprintf("tool_inspected_%d", index), DraftID: "draft_job_dedup",
				Role: "system", Kind: "tool",
				Content: fmt.Sprintf(
					`{"tool":"preview.check","preview_id":"preview_done","preview_check":%q,"args_summary":"{truncated...","observation":"{\"summary\":\"ok\"}","status":"succeeded"}`,
					check,
				),
			}},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("tool message status=%s err=%v", result.Status, err)
		}
	}
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueJobObservation(
		"draft_job_dedup",
		"job_done",
		map[string]any{
			"event": "JobSucceeded",
			"payload": map[string]any{
				"job_id": "job_done", "kind": "render_preview",
				"result": map[string]any{"artifact_id": "preview_done"},
			},
		},
	) {
		t.Fatal("job observation 未入队")
	}
	service.Queue().JoinDraft("draft_job_dedup")
	if prompt := chatModel.lastPrompt(); prompt != "" {
		t.Fatalf("已质检预览不应再次唤醒模型: %q", prompt)
	}
	for index, content := range []string{
		`not-json`,
		`{"tool":"preview.check","preview_id":"preview_done","status":"failed"}`,
		`{"tool":"preview.check","args_summary":"{\"preview_id\":\"preview_legacy\"}","status":"succeeded"}`,
	} {
		result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
				ID: fmt.Sprintf("tool_ignored_%d", index), DraftID: "draft_job_dedup",
				Role: "system", Kind: "tool", Content: content,
			}},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("ignored tool message status=%s err=%v", result.Status, err)
		}
	}
	if service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", nil) {
		t.Fatal("缺少预览 ID 时不应判定为已质检")
	}
	if !service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"preview_id": "preview_done"}) {
		t.Fatal("应兼容 preview_id 形式的终态结果")
	}
	if service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"preview_id": "preview_legacy"}) {
		t.Fatal("单个旧 trace 不应冒充完整的五项原子质检")
	}
	if service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"artifact_id": "preview_other"}) {
		t.Fatal("不同预览不应被误去重")
	}
	result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "tool_decode_failed", DraftID: "draft_job_dedup", Role: "system", Kind: "tool",
			Content: `{"tool":"preview.check","preview_id":"preview_done","preview_check":"decode","status":"failed"}`,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("failed check status=%s err=%v", result.Status, err)
	}
	if service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"preview_id": "preview_done"}) {
		t.Fatal("最新一次原子检查失败时不应被更早的成功 trace 掩盖")
	}
	result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "tool_decode_recovered", DraftID: "draft_job_dedup", Role: "system", Kind: "tool",
			Content: `{"tool":"preview.check","preview_id":"preview_done","preview_check":"decode","status":"succeeded"}`,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("recovered check status=%s err=%v", result.Status, err)
	}
	if !service.executor.PreviewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"preview_id": "preview_done"}) {
		t.Fatal("原子检查重试成功后应恢复为已完成")
	}
	cancelledContext, cancel := context.WithCancel(t.Context())
	cancel()
	if service.executor.PreviewAlreadyInspected(cancelledContext, "draft_job_dedup", map[string]any{"artifact_id": "preview_done"}) {
		t.Fatal("读取历史失败时应保守地继续后台回调")
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_job_dedup", 20)
	if err != nil || len(messages) != 11 {
		t.Fatalf("去重后不应生成重复回复: messages=%#v err=%v", messages, err)
	}
	for _, message := range messages {
		if message.Kind == "reply" {
			t.Fatalf("去重后不应生成回复: %#v", message)
		}
	}
}

func TestToolReporterPairsSameNameCallsByCallIDAndPersistsPreviewID(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reporter_call_ids"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	reporter := service.toolReporter(t.Context(), draftID)
	firstCtx := rushestools.WithToolCallID(t.Context(), "call_first")
	secondCtx := rushestools.WithToolCallID(t.Context(), "call_second")
	firstInput := rushestools.PreviewCheckInput{
		PreviewID: "preview_first", Check: strings.Repeat("visual", 80),
	}
	secondInput := rushestools.PreviewCheckInput{PreviewID: "preview_second"}
	reporter(firstCtx, "preview.check", "started", firstInput, nil, nil)
	reporter(secondCtx, "preview.check", "started", secondInput, nil, nil)
	reporter(firstCtx, "preview.check", "finished", firstInput, rushestools.ToolResult{Status: "succeeded"}, nil)
	reporter(secondCtx, "preview.check", "finished", secondInput, rushestools.ToolResult{Status: "succeeded"}, nil)

	events := service.Hub().Snapshot(draftID)
	if len(events) != 4 || events[0]["step_id"] != events[2]["step_id"] ||
		events[1]["step_id"] != events[3]["step_id"] || events[0]["step_id"] == events[1]["step_id"] {
		t.Fatalf("same-name tool calls were cross-wired: %#v", events)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(messages) != 2 {
		t.Fatalf("tool trace messages=%#v err=%v", messages, err)
	}
	previewIDs := map[string]bool{}
	for _, message := range messages {
		var record struct {
			PreviewID   string `json:"preview_id"`
			ArgsSummary string `json:"args_summary"`
		}
		if err := json.Unmarshal([]byte(message.Content), &record); err != nil {
			t.Fatal(err)
		}
		previewIDs[record.PreviewID] = true
		if record.PreviewID == "preview_first" && !strings.HasSuffix(record.ArgsSummary, "...") {
			t.Fatalf("long argument fixture did not exercise truncation: %q", record.ArgsSummary)
		}
	}
	if !previewIDs["preview_first"] || !previewIDs["preview_second"] {
		t.Fatalf("persisted preview IDs=%#v", previewIDs)
	}
}

func TestToolReporterPersistsTypedIndexMissingAsFailure(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reporter_index_missing"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	reporter := service.toolReporter(t.Context(), draftID)
	ctx := rushestools.WithToolCallID(t.Context(), "call_index_missing")
	input := rushestools.SpeechSearchInput{AssetID: "asset_missing_index"}
	output := rushestools.SpeechSearchResult{
		Status: string(rushestools.StatusFailed), AssetID: "asset_missing_index",
		ErrorCode: string(rushestools.ErrCodeIndexMissing),
		Recovery:  "先调用 speech.transcribe。",
	}
	reporter(ctx, "speech.search", "started", input, nil, nil)
	reporter(ctx, "speech.search", "finished", input, output, nil)

	events := service.Hub().Snapshot(draftID)
	if len(events) != 2 || events[1]["status"] != "failed" {
		t.Fatalf("tool events=%#v", events)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	var trace struct {
		Status      string `json:"status"`
		Observation string `json:"observation"`
	}
	if err := json.Unmarshal([]byte(messages[0].Content), &trace); err != nil ||
		trace.Status != "failed" ||
		!strings.Contains(trace.Observation, string(rushestools.ErrCodeIndexMissing)) {
		t.Fatalf("trace=%#v err=%v", trace, err)
	}
}

func TestToolRecoveryPersistsPreviewIDFromSyntheticStartedArguments(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_reporter_recovery_preview"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	ctx := rushestools.WithReporter(t.Context(), service.toolReporter(t.Context(), draftID))
	endpoint := newToolRecoveryMiddleware(testRetrySafe(t)).Invokable(
		func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			reporter, ok := rushestools.ReporterFromContext(ctx)
			if !ok {
				t.Fatal("tool recovery lost reporter")
			}
			typed := rushestools.PreviewCheckInput{PreviewID: "preview_production", Check: "decode"}
			reporter(ctx, input.Name, "started", typed, nil, nil)
			reporter(ctx, input.Name, "finished", typed, rushestools.ToolResult{Status: "succeeded"}, nil)
			return &compose.ToolOutput{Result: `{"status":"succeeded"}`}, nil
		},
	)
	if _, err := endpoint(ctx, &compose.ToolInput{
		Name: "preview.check", CallID: "call_production",
		Arguments: `{"preview_id":"preview_production","check":"decode"}`,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content, `"preview_id":"preview_production"`) {
		t.Fatalf("production recovery trace=%#v err=%v", messages, err)
	}
}

func TestCompletedPreviewObservationRequestsAtomicChecks(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_job_atomic_checks"
	agenttest.CreateAgentDraft(t, database, draftID)
	agenttest.InsertAgentMessage(t, database, draftID, "user_job_atomic_checks", "生成预览并检查")
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueJobObservation(draftID, "job_atomic_checks", map[string]any{
		"event": "JobSucceeded",
		"payload": map[string]any{
			"job_id": "job_atomic_checks", "kind": "render_preview",
			"result": map[string]any{"artifact_id": "preview_atomic"},
		},
	}) {
		t.Fatal("job observation 未入队")
	}
	service.Queue().JoinDraft(draftID)
	prompt := chatModel.lastPrompt()
	for _, expected := range []string{
		"状态：成功", "preview_atomic", "preview.check", "decode", "black", "freeze",
		"silence", "loudness", "独立检查可并行", "visual",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q: %s", expected, prompt)
		}
	}
	for _, forbidden := range []string{
		"verification_report", "render_inspection", "content_contract",
		`"degraded":true`, "preview_inspection_unavailable", "自动质检",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("job bridge 不应注入组合质检字段 %q: %s", forbidden, prompt)
		}
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Kind == "tool" {
			t.Fatalf("job bridge 不应替模型执行 preview.check: %#v", message)
		}
	}
}

func TestServiceCancellationPropagatesToTurnContext(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_cancel")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.fallbackScaffold = blockingFallbackScaffold{}
	_, stream, unsubscribe := service.Hub().Subscribe("draft_cancel")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_cancel", "msg", "等待取消")
	for {
		event := <-stream
		if event["type"] == "turn_started" {
			break
		}
	}
	if !service.Queue().RequestStop("draft_cancel") {
		t.Fatal("取消请求未传播")
	}
	service.Queue().JoinDraft("draft_cancel")
	for {
		select {
		case event := <-stream:
			if event["type"] == "turn_ended" {
				if event["outcome"] != "cancelled" {
					t.Fatalf("event=%#v", event)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("未收到取消终态")
		}
	}
}

// 用户主动取消与回合失败是两条不同语义：取消只落 cancelled 终态，绝不产出
// turn_failure 系统消息，否则会把用户自己的中止误报成系统失败（issue #95 H2）。
func TestUserCancelledTurnPersistsNoFailureMessage(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_cancel_no_failure")
	agenttest.InsertAgentMessage(t, database, "draft_cancel_no_failure", "user_cancel_no_failure", "等待取消")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.fallbackScaffold = blockingFallbackScaffold{}
	_, stream, unsubscribe := service.Hub().Subscribe("draft_cancel_no_failure")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_cancel_no_failure", "user_cancel_no_failure", "等待取消")
	for {
		event := <-stream
		if event["type"] == "turn_started" {
			break
		}
	}
	if !service.Queue().RequestStop("draft_cancel_no_failure") {
		t.Fatal("取消请求未传播")
	}
	service.Queue().JoinDraft("draft_cancel_no_failure")
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("用户主动取消不应触发 turn_error")
			case "message_completed":
				t.Fatalf("用户取消不应产出任何终态消息：%#v", event)
			case "turn_ended":
				if event["outcome"] != "cancelled" {
					t.Fatalf("取消终态错误：%#v", event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_cancel_no_failure", 20)
				if listErr != nil {
					t.Fatal(listErr)
				}
				for _, message := range messages {
					if message.Kind == "turn_failure" || message.Role == "system" {
						t.Fatalf("取消回合不应落库失败消息：%#v", message)
					}
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("未收到取消终态")
		}
	}
}

// 取消发生在模型执行中、且 provider 抛出的错误不包裹 context.Canceled 时，仍必须判为
// cancelled 终态而非 turn_failure。这是只走 fallback 的
// TestUserCancelledTurnPersistsNoFailureMessage 覆盖不到的风险路径：runTurn 的取消分水岭
// 依赖 ctx.Err() 兜底，而非只看错误链（issue #95 H2）。
func TestModelExecutionCancelledPersistsNoFailureMessage(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_cancel_model_exec")
	agenttest.InsertAgentMessage(t, database, "draft_cancel_model_exec", "user_cancel_model_exec", "在模型执行中取消")
	chatModel := &cancelDuringModelExecModel{entered: make(chan struct{})}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_cancel_model_exec")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_cancel_model_exec", "user_cancel_model_exec", "在模型执行中取消")
	// 等模型真正进入执行（阻塞）后再取消，确保取消落在模型执行中而非其前后。
	select {
	case <-chatModel.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("模型未进入执行")
	}
	if !service.Queue().RequestStop("draft_cancel_model_exec") {
		t.Fatal("取消请求未传播")
	}
	service.Queue().JoinDraft("draft_cancel_model_exec")
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("模型执行中取消不应触发 turn_error")
			case "message_completed":
				t.Fatalf("模型执行中取消不应产出任何终态消息：%#v", event)
			case "turn_ended":
				if event["outcome"] != "cancelled" {
					t.Fatalf("取消终态错误：%#v", event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_cancel_model_exec", 20)
				if listErr != nil {
					t.Fatal(listErr)
				}
				for _, message := range messages {
					if message.Kind == "turn_failure" || message.Role == "system" {
						t.Fatalf("模型执行中取消不应落库失败消息：%#v", message)
					}
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("未收到取消终态")
		}
	}
}

func TestCancellationDuringReflectionStillEndsCancelledWithoutTerminalEvents(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_cancel_during_reflection"
	agenttest.CreateAgentDraft(t, database, draftID)
	agenttest.InsertAgentMessage(t, database, draftID, "user_cancel_reflection", "在回复质检时取消")
	chatModel := &cancelDuringReflectionModel{ready: make(chan struct{})}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe(draftID)
	defer unsubscribe()
	service.Queue().EnqueueUserMessage(draftID, "user_cancel_reflection", "在回复质检时取消")
	select {
	case <-chatModel.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("未进入反思重述窗口")
	}
	if !service.Queue().RequestStop(draftID) {
		t.Fatal("取消请求未传播")
	}
	service.Queue().JoinDraft(draftID)

	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case TurnStreamTurnError:
				t.Fatalf("反思窗口取消不应发 turn_error：%#v", event)
			case TurnStreamTextDelta, TurnStreamMessageCompleted:
				t.Fatalf("反思窗口取消不应泄漏终态正文：%#v", event)
			case TurnStreamTurnEnded:
				if event["outcome"] != "cancelled" {
					t.Fatalf("取消终态错误：%#v", event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
				if listErr != nil {
					t.Fatal(listErr)
				}
				if len(messages) != 1 || messages[0].Role != "user" {
					t.Fatalf("取消后不得持久化回复或失败：%#v", messages)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("反思窗口取消没有 cancelled 终态")
		}
	}
}

func TestJobObservationBridgeWakesAgentForWaitedTerminalJob(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_bridge")
	defer unsubscribe()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{
			Type: "JobEnqueued", DraftID: "draft_bridge",
			Payload: map[string]any{
				"job_id": "job_render", "kind": "render_preview",
				"requested_by_draft_id": "draft_bridge",
			},
		},
		{
			Type: "JobSucceeded", DraftID: "draft_bridge",
			Payload: map[string]any{
				"job_id": "job_render", "kind": "render_preview",
				"requested_by_draft_id": "draft_bridge", "result": map[string]any{"preview_id": "p1"},
			},
		},
	}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("apply status=%s err=%v", result.Status, err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-stream:
			if event["type"] == "message_completed" {
				if event["content"] != "render_preview 任务 job_render 已完成。" {
					t.Fatalf("event=%#v", event)
				}
				if event["kind"] != "observation" {
					t.Fatalf("后台回调应以 observation 呈现: %#v", event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_bridge", 20)
				if listErr != nil || len(messages) != 1 || messages[0].Kind != "observation" {
					t.Fatalf("messages=%#v err=%v", messages, listErr)
				}
				return
			}
		case <-deadline:
			t.Fatal("job observation 未唤醒 Agent")
		}
	}
}

func TestJobObservationBridgeRecoversBacklogAfterServiceRestart(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_restart")
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "JobEnqueued", DraftID: "draft_bridge_restart", Payload: map[string]any{
			"job_id": "job_restart", "kind": "render_preview",
			"requested_by_draft_id": "draft_bridge_restart",
		}},
		{Type: "JobSucceeded", DraftID: "draft_bridge_restart", Payload: map[string]any{
			"job_id": "job_restart", "kind": "render_preview",
			"requested_by_draft_id": "draft_bridge_restart",
			"result":                map[string]any{"preview_id": "restart_preview"},
		}},
	}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("apply status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_bridge_restart", 20)
		if listErr == nil && len(messages) == 1 && strings.Contains(messages[0].Content, "job_restart 已完成") {
			var cursor, terminalEventID int64
			if err := database.Read().QueryRowContext(t.Context(), `
				SELECT last_event_id FROM agent_job_bridge_state WHERE consumer_id='agent'`,
			).Scan(&cursor); err != nil {
				t.Fatal(err)
			}
			if err := database.Read().QueryRowContext(t.Context(), `
				SELECT event_id FROM event_log WHERE event_type='JobSucceeded'
				AND json_extract(payload_json,'$.payload.job_id')='job_restart'`,
			).Scan(&terminalEventID); err != nil || cursor < terminalEventID {
				t.Fatalf("cursor=%d terminal=%d err=%v", cursor, terminalEventID, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("service 重启后没有补扫 terminal backlog")
}

func TestJobObservationBridgeReplaysCommittedUndeliveredObservation(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_outbox")
	event := map[string]any{
		"event": "JobSucceeded", "draft_id": "draft_bridge_outbox",
		"payload": map[string]any{
			"job_id": "job_outbox", "kind": "render_preview",
			"requested_by_draft_id": "draft_bridge_outbox",
		},
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobBridgeCursor: &reducer.AgentJobBridgeCursorRow{
				ConsumerID: agentJobBridgeConsumerID, LastEventID: 99,
			},
			AgentJobObservations: []reducer.AgentJobObservationRow{{
				JobID: "job_outbox", EventID: 99, DraftID: "draft_bridge_outbox",
				Event: event, ClaimToken: "claim_outbox",
			}},
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := make(chan string, 1)
	service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{}}
	queue := NewTurnQueue(ctx, func(runCtx context.Context, item QueueItem) error {
		seen <- item.ItemID
		return service.runTurn(runCtx, item)
	})
	defer queue.Close()
	service.queue = queue
	if cursor := service.bridgeIteration(t.Context(), 99); cursor != 99 {
		t.Fatalf("cursor=%d", cursor)
	}
	queue.JoinDraft("draft_bridge_outbox")
	select {
	case jobID := <-seen:
		if jobID != "job_outbox" {
			t.Fatalf("job_id=%s", jobID)
		}
	default:
		t.Fatal("committed pending observation was not replayed")
	}
	var deliveredAt *string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT delivered_at FROM agent_job_observations WHERE job_id='job_outbox'`,
	).Scan(&deliveredAt); err != nil || deliveredAt == nil || *deliveredAt == "" {
		t.Fatalf("delivered_at=%v err=%v", deliveredAt, err)
	}
}

func TestJobObservationBridgeRetriesObservationAfterRunnerFailure(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_retry")
	event := map[string]any{
		"event": "JobSucceeded", "draft_id": "draft_bridge_retry",
		"payload": map[string]any{
			"job_id": "job_retry", "kind": "render_preview",
			"requested_by_draft_id": "draft_bridge_retry",
		},
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobBridgeCursor: &reducer.AgentJobBridgeCursorRow{
				ConsumerID: agentJobBridgeConsumerID, LastEventID: 99,
			},
			AgentJobObservations: []reducer.AgentJobObservationRow{{
				JobID: "job_retry", EventID: 99, DraftID: "draft_bridge_retry",
				Event: event, ClaimToken: "claim_retry",
			}},
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls int
	service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{}}
	queue := NewTurnQueue(ctx, func(runCtx context.Context, item QueueItem) error {
		calls++
		if calls == 1 {
			return errors.New("transient runner failure")
		}
		return service.runTurn(runCtx, item)
	})
	defer queue.Close()
	service.queue = queue
	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft("draft_bridge_retry")
	var deliveredAt *string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT delivered_at FROM agent_job_observations WHERE job_id='job_retry'`,
	).Scan(&deliveredAt); err != nil || deliveredAt != nil {
		t.Fatalf("失败后 observation 不应确认: delivered_at=%v err=%v", deliveredAt, err)
	}
	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft("draft_bridge_retry")
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT delivered_at FROM agent_job_observations WHERE job_id='job_retry'`,
	).Scan(&deliveredAt); err != nil || deliveredAt == nil || *deliveredAt == "" {
		t.Fatalf("重放成功后 observation 应确认: delivered_at=%v err=%v", deliveredAt, err)
	}
	if calls != 2 {
		t.Fatalf("runner calls=%d want=2", calls)
	}
}

func TestJobObservationBridgeSuppressesUserCancelledTurns(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "running"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			database := agenttest.AgentTestDatabase(t)
			draftID := "draft_bridge_cancel_" + name
			jobID := "job_bridge_cancel_" + name
			agenttest.CreateAgentDraft(t, database, draftID)
			event := map[string]any{
				"event": "JobSucceeded", "draft_id": draftID,
				"payload": map[string]any{
					"job_id": jobID, "kind": "render_preview",
					"requested_by_draft_id": draftID,
				},
			}
			result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
				Actor: contracts.ActorAgent,
				ResultRows: reducer.ResultRows{AgentJobObservations: []reducer.AgentJobObservationRow{{
					JobID: jobID, EventID: 1, DraftID: draftID,
					Event: event, ClaimToken: "claim_" + jobID,
				}}},
			})
			if err != nil || result.Status != reducer.StatusApplied {
				t.Fatalf("seed status=%s err=%v", result.Status, err)
			}

			queueCtx, cancelQueue := context.WithCancel(t.Context())
			defer cancelQueue()
			started := make(chan struct{}, 1)
			var mu sync.Mutex
			calls := 0
			service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{}}
			queue := NewTurnQueue(queueCtx, func(runCtx context.Context, _ QueueItem) error {
				mu.Lock()
				calls++
				mu.Unlock()
				started <- struct{}{}
				<-runCtx.Done()
				return runCtx.Err()
			})
			if pending {
				queue.workers[draftID] = &draftWorker{queue: make(chan QueueItem, 1)}
			}
			service.queue = queue
			t.Cleanup(queue.Close)

			service.dispatchPendingJobObservations(t.Context())
			if pending {
				cancelled := make(chan bool, 1)
				go func() { cancelled <- queue.CancelAndJoinDraft(draftID) }()
				deadline := time.Now().Add(time.Second)
				for {
					worker := queue.workers[draftID]
					worker.mu.Lock()
					canceling := worker.canceling
					worker.mu.Unlock()
					if canceling {
						go queue.runWorker(worker)
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("取消屏障未建立")
					}
					time.Sleep(time.Millisecond)
				}
				if !<-cancelled {
					t.Fatal("pending observation 应被取消")
				}
			} else {
				<-started
				if !queue.CancelAndJoinDraft(draftID) {
					t.Fatal("running observation 应被取消")
				}
			}

			for range 3 {
				service.dispatchPendingJobObservations(t.Context())
				queue.JoinDraft(draftID)
			}
			mu.Lock()
			callCount := calls
			mu.Unlock()
			if callCount != 1 {
				t.Fatalf("cancelled observation replayed: calls=%d", callCount)
			}
			var deliveredAt *string
			if err := database.Read().QueryRowContext(t.Context(), `
				SELECT delivered_at FROM agent_job_observations WHERE job_id=?`, jobID,
			).Scan(&deliveredAt); err != nil || deliveredAt == nil || *deliveredAt == "" {
				t.Fatalf("delivered_at=%v err=%v", deliveredAt, err)
			}
		})
	}
}

func TestJobObservationBridgeHonorsPersistentTurnCancellationSuppression(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	draftID := "draft_bridge_persistent_suppression"
	jobID := "job_bridge_persistent_suppression"
	agenttest.CreateAgentDraft(t, database, draftID)
	event := map[string]any{
		"event": "JobSucceeded", "draft_id": draftID,
		"payload": map[string]any{
			"job_id": jobID, "kind": "render_preview",
			"requested_by_draft_id": draftID,
		},
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobObservations: []reducer.AgentJobObservationRow{{
				JobID: jobID, EventID: 1, DraftID: draftID,
				Event: event, ClaimToken: "claim_" + jobID,
			}},
			AgentJobObservationSuppressions: []reducer.AgentJobObservationSuppressionRow{{JobID: jobID}},
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}
	var calls int
	queue := NewTurnQueue(t.Context(), func(context.Context, QueueItem) error {
		calls++
		return nil
	})
	t.Cleanup(queue.Close)
	service := &Service{
		database: database, queue: queue, hub: NewTurnStreamHub(0),
		bridgeInflight: map[string]struct{}{},
	}
	barrier, _ := queue.BeginDraftCancellation(draftID)
	service.dispatchPendingJobObservations(t.Context())
	barrier.Release()
	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft(draftID)
	if calls != 0 {
		t.Fatalf("被持久抑制的 observation 不应续跑: calls=%d", calls)
	}
	var deliveredAt *string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT delivered_at FROM agent_job_observations WHERE job_id=?`, jobID,
	).Scan(&deliveredAt); err != nil || deliveredAt == nil || *deliveredAt == "" {
		t.Fatalf("delivered_at=%v err=%v", deliveredAt, err)
	}
}

func TestJobObservationBridgeReleasesInflightAndHandlesClosedDependencies(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	draftID := "draft_bridge_failure_paths"
	agenttest.CreateAgentDraft(t, database, draftID)
	started := make(chan struct{})
	release := make(chan struct{})
	queue := NewTurnQueue(t.Context(), func(context.Context, QueueItem) error {
		close(started)
		<-release
		return nil
	})
	service := &Service{database: database, queue: queue, hub: NewTurnStreamHub(0)}
	observation := bridgeObservation{
		eventID: 1, draftID: draftID, jobID: "job_inflight", claimToken: "claim_inflight",
		event: map[string]any{
			"event":   "JobSucceeded",
			"payload": map[string]any{"job_id": "job_inflight", "kind": "render_preview"},
		},
	}
	if !service.dispatchJobObservation(t.Context(), observation) {
		t.Fatal("首个 observation 应入队")
	}
	<-started
	if service.dispatchJobObservation(t.Context(), observation) {
		t.Fatal("同一 job 的 inflight observation 不应重复入队")
	}
	close(release)
	queue.JoinDraft(draftID)
	service.bridgeMu.Lock()
	inflight := len(service.bridgeInflight)
	service.bridgeMu.Unlock()
	if inflight != 0 {
		t.Fatalf("消费后 inflight 未释放: %d", inflight)
	}
	queue.Close()
	observation.jobID = "job_closed_queue"
	observation.claimToken = "claim_closed_queue"
	if service.dispatchJobObservation(t.Context(), observation) {
		t.Fatal("已关闭队列不应接受 observation")
	}
	service.bridgeMu.Lock()
	inflight = len(service.bridgeInflight)
	service.bridgeMu.Unlock()
	if inflight != 0 {
		t.Fatalf("拒绝入队后 inflight 未释放: %d", inflight)
	}

	closedDatabase := agenttest.AgentTestDatabase(t)
	if err := closedDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	closedService := &Service{database: closedDatabase, bridgeInflight: map[string]struct{}{}}
	if closedService.jobObservationSuppressed(t.Context(), "job") {
		t.Fatal("存储不可用时不得把 observation 误判为已抑制")
	}
	closedService.markJobObservationDelivered(t.Context(), "job", "claim")
	if page, err := closedService.pendingJobObservations(t.Context()); err == nil || page != nil {
		t.Fatalf("存储不可用时 page=%v err=%v", page, err)
	}
	closedService.startJobObservationBridge(t.Context())
	if cursor := closedService.bridgeIteration(t.Context(), 9); cursor != 9 {
		t.Fatalf("存储不可用时 cursor=%d", cursor)
	}
}

func TestJobObservationBridgeRevisitsOldestAfterInflightClears(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_page")
	makeObservations := func(first, last int) []reducer.AgentJobObservationRow {
		observations := make([]reducer.AgentJobObservationRow, 0, last-first+1)
		for index := first; index <= last; index++ {
			jobID := fmt.Sprintf("job_page_%03d", index)
			observations = append(observations, reducer.AgentJobObservationRow{
				JobID: jobID, EventID: int64(index), DraftID: "draft_bridge_page",
				ClaimToken: "claim_" + jobID,
				Event: map[string]any{
					"event": "JobSucceeded", "draft_id": "draft_bridge_page",
					"payload": map[string]any{
						"job_id": jobID, "kind": "render_preview",
						"requested_by_draft_id": "draft_bridge_page",
					},
				},
			})
		}
		return observations
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor:      contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentJobObservations: makeObservations(1, 101)},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}

	var observedMu sync.Mutex
	observed := []string{}
	service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{
		"job_page_001": {},
	}}
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		observedMu.Lock()
		observed = append(observed, item.ItemID)
		observedMu.Unlock()
		return nil
	})
	service.queue = queue
	t.Cleanup(queue.Close)

	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft("draft_bridge_page")
	observedMu.Lock()
	firstPass := append([]string(nil), observed...)
	observedMu.Unlock()
	if slices.Contains(firstPass, "job_page_001") || len(firstPass) != jobObservationDispatchLimit-1 {
		t.Fatalf("first pass count=%d blocked_present=%v", len(firstPass), slices.Contains(firstPass, "job_page_001"))
	}
	result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobObservations: makeObservations(102, 201),
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("inject backlog status=%s err=%v", result.Status, err)
	}
	service.bridgeMu.Lock()
	delete(service.bridgeInflight, "job_page_001")
	service.bridgeMu.Unlock()

	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft("draft_bridge_page")
	observedMu.Lock()
	allObserved := append([]string(nil), observed...)
	observedMu.Unlock()
	dispatchCount := 0
	for _, jobID := range allObserved {
		if jobID == "job_page_001" {
			dispatchCount++
		}
	}
	if dispatchCount != 1 {
		t.Fatalf("oldest observation dispatch count=%d observed=%v", dispatchCount, allObserved)
	}
}

func TestJobObservationBridgeDispatchesOnePagePerIteration(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_limit")
	observations := make([]reducer.AgentJobObservationRow, 0, 201)
	for index := 1; index <= 201; index++ {
		jobID := fmt.Sprintf("job_limit_%03d", index)
		observations = append(observations, reducer.AgentJobObservationRow{
			JobID: jobID, EventID: int64(index), DraftID: "draft_bridge_limit",
			ClaimToken: "claim_" + jobID,
			Event: map[string]any{
				"event": "JobCancelled", "draft_id": "draft_bridge_limit",
				"payload": map[string]any{
					"job_id": jobID, "kind": "render_preview", "reason": "turn_cancelled",
					"requested_by_draft_id": "draft_bridge_limit",
				},
			},
		})
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobObservations: observations,
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO event_log(event_type,actor,payload_json,created_at)
		VALUES('JobSucceeded','job',?,?)`,
		`{"event":"JobSucceeded","payload":{"kind":"noop","job_id":"cursor_advance"}}`, now,
	); err != nil {
		t.Fatal(err)
	}

	service := &Service{database: database, bridgeInflight: map[string]struct{}{}}
	if cursor := service.bridgeIteration(t.Context(), 0); cursor == 0 {
		t.Fatal("event-log cursor should advance")
	}
	var delivered int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_job_observations WHERE delivered_at IS NOT NULL`,
	).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != jobObservationDispatchLimit {
		t.Fatalf("delivered=%d want=%d", delivered, jobObservationDispatchLimit)
	}
}

func TestJobObservationBridgeQuarantinesFullMalformedPage(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_malformed")
	observations := make([]reducer.AgentJobObservationRow, 0, jobObservationDispatchLimit+1)
	for index := 1; index <= jobObservationDispatchLimit+1; index++ {
		jobID := fmt.Sprintf("job_malformed_%d", index)
		observations = append(observations, reducer.AgentJobObservationRow{
			JobID: jobID, EventID: int64(index), DraftID: "draft_bridge_malformed",
			ClaimToken: "claim_" + jobID,
			Event: map[string]any{
				"event":   "JobSucceeded",
				"payload": map[string]any{"job_id": jobID, "kind": "render_preview"},
			},
		})
	}
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{
			AgentJobObservations: observations,
		},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE agent_job_observations SET event_json='not-json' WHERE event_id<=?`,
		jobObservationDispatchLimit); err != nil {
		t.Fatal(err)
	}

	observed := make(chan string, 1)
	queue := NewTurnQueue(t.Context(), func(_ context.Context, item QueueItem) error {
		observed <- item.ItemID
		return nil
	})
	t.Cleanup(queue.Close)
	service := &Service{database: database, queue: queue}
	service.dispatchPendingJobObservations(t.Context())
	var delivered int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_job_observations WHERE delivered_at IS NOT NULL`,
	).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != jobObservationDispatchLimit {
		t.Fatalf("quarantined=%d want=%d", delivered, jobObservationDispatchLimit)
	}
	select {
	case itemID := <-observed:
		t.Fatalf("first page should contain only malformed observations, got %s", itemID)
	default:
	}

	service.dispatchPendingJobObservations(t.Context())
	queue.JoinDraft("draft_bridge_malformed")
	select {
	case itemID := <-observed:
		if itemID != fmt.Sprintf("job_malformed_%d", jobObservationDispatchLimit+1) {
			t.Fatalf("item=%s", itemID)
		}
	default:
		t.Fatal("valid observation after malformed page was not dispatched on the next tick")
	}
}

func TestJobObservationBridgeDeduplicatesJobAndSplitsCancellationReason(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bridge_cancel")
	for _, item := range []struct {
		id, reason string
	}{{"job_turn_cancel", "turn_cancelled"}, {"job_manual_cancel", "user_cancelled"}} {
		agenttest.InsertJobFixtureAsset(t, database, "asset_"+item.id)
		if _, err := reducer.Apply(t.Context(), database, []contracts.Event{
			{Type: "JobEnqueued", DraftID: "draft_bridge_cancel", Payload: map[string]any{
				"job_id": item.id, "kind": "understand",
				"asset_id":              "asset_" + item.id,
				"requested_by_draft_id": "draft_bridge_cancel",
			}},
			{Type: "JobCancelled", DraftID: "draft_bridge_cancel", Payload: map[string]any{
				"job_id": item.id, "kind": "understand", "reason": item.reason,
				"requested_by_draft_id": "draft_bridge_cancel",
			}},
		}, reducer.Options{Actor: contracts.ActorUser}); err != nil {
			t.Fatal(err)
		}
	}
	draftBefore, err := storage.GetDraft(t.Context(), database.Read(), "draft_bridge_cancel")
	if err != nil {
		t.Fatal(err)
	}
	var eventCountBefore int
	if err := database.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM event_log").Scan(&eventCountBefore); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	observed := []string{}
	service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{}}
	queue := NewTurnQueue(ctx, func(runCtx context.Context, item QueueItem) error {
		mu.Lock()
		observed = append(observed, item.ItemID)
		mu.Unlock()
		return service.runTurn(runCtx, item)
	})
	defer queue.Close()
	service.queue = queue
	cursor := service.bridgeIteration(t.Context(), 0)
	queue.JoinDraft("draft_bridge_cancel")
	_ = service.bridgeIteration(t.Context(), 0)
	queue.JoinDraft("draft_bridge_cancel")
	mu.Lock()
	observedCopy := append([]string(nil), observed...)
	mu.Unlock()
	if cursor == 0 || !reflect.DeepEqual(observedCopy, []string{"job_manual_cancel"}) {
		t.Fatalf("cursor=%d observed=%v", cursor, observedCopy)
	}
	draftAfter, err := storage.GetDraft(t.Context(), database.Read(), "draft_bridge_cancel")
	if err != nil || draftAfter.StateVersion != draftBefore.StateVersion {
		t.Fatalf("bridge bookkeeping changed state_version: before=%d after=%d err=%v",
			draftBefore.StateVersion, draftAfter.StateVersion, err)
	}
	var eventCountAfter int
	if err := database.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM event_log").Scan(&eventCountAfter); err != nil || eventCountAfter != eventCountBefore {
		t.Fatalf("bridge bookkeeping emitted events: before=%d after=%d err=%v",
			eventCountBefore, eventCountAfter, err)
	}
	content, err := service.continueAfterJobObservation(t.Context(), QueueItem{
		DraftID: "draft_bridge_cancel", Kind: QueueJobObservation, ItemID: "job_manual_cancel",
		Payload: map[string]any{"job_id": "job_manual_cancel", "event": map[string]any{
			"event": "JobCancelled", "payload": map[string]any{
				"job_id": "job_manual_cancel", "kind": "understand", "reason": "user_cancelled",
			},
		}},
	}, "message")
	if err != nil || content != "后台任务已被取消：understand（job_id：job_manual_cancel）。" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestUnderstandingRepeatedRunsAllocateNewSummaryVersion(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_understand_repeat")
	video := filepath.Join(database.Paths.Temporary, "repeat.mp4")
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=green:s=64x64:r=5:d=0.4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", video,
	); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(video)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_repeat", "job_id": "job_repeat", "storage_mode": "reference",
			"reference_path": video, "kind": "video", "source": "local_path",
			"filename": "repeat.mp4", "hash": "repeat", "size": info.Size(),
			"ingest_status": "ready", "usable": true,
		}},
		{Type: "AssetLinked", DraftID: "draft_understand_repeat", Payload: map[string]any{
			"asset_id": "asset_repeat",
		}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_understand_repeat")
	cacheHits := 0
	for _, focus := range []string{"首次", "更深入", "更深入"} {
		output, err := service.ExecuteTool(ctx, "media.detect_shots", rushestools.DetectShotsInput{
			AssetID: "asset_repeat", Focus: focus,
		})
		if err != nil {
			t.Fatalf("focus=%s err=%v", focus, err)
		}
		understood := output.(rushestools.DetectShotsResult)
		if understood.Summary == nil || understood.Summary.AssetID != "asset_repeat" ||
			understood.Summary.Overall == "" || len(understood.Summary.Evidence) != 1 ||
			understood.Summary.Evidence[0].SourceEndFrame <=
				understood.Summary.Evidence[0].SourceStartFrame {
			t.Fatalf("understand 同回合缺少摘要或时间证据: %#v", understood)
		}
		if understood.CacheHit {
			cacheHits++
		}
	}
	rows, err := database.Read().QueryContext(t.Context(), `
		SELECT version FROM material_summaries WHERE asset_id='asset_repeat' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	versions := []int{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("versions=%v", versions)
	}
	if cacheHits != 1 {
		t.Fatalf("cache hits=%d want=1", cacheHits)
	}
}

func TestRewoundTimelineIsSynchronouslyRevalidatedAndQueuedForRender(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_rewind_render"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	versionOne, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := seedTimelineVersion(service,
		t.Context(), draftID, versionOne, "rewind_render_v1", nil); persistErr != nil || result.Status != "succeeded" {
		t.Fatalf("persist v1=%#v err=%v", result, persistErr)
	}
	agenttest.InsertAgentMessage(t, database, draftID, "user_rewind_render", "保留这个版本")
	checkpoint, err := storage.GetRewindCheckpoint(
		t.Context(), database.Read(), draftID, "rewind:message:user_rewind_render",
	)
	if err != nil || checkpoint.TimelineVersion == nil || *checkpoint.TimelineVersion != 1 {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	versionTwo, err := agenttest.ComposeTimeline(draftID, 2, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := seedTimelineVersion(service,
		t.Context(), draftID, versionTwo, "rewind_render_v2", nil); persistErr != nil || result.Status != "succeeded" {
		t.Fatalf("persist v2=%#v err=%v", result, persistErr)
	}
	draft, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil {
		t.Fatal(err)
	}
	restoreResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "TimelineVersionRestored", DraftID: draftID,
		Payload: map[string]any{
			"checkpoint_id": checkpoint.ID, "mode": "timeline", "timeline_version": 3,
			"restore_checkpoint_id": "rewind:restore:render",
		},
	}}, reducer.Options{Actor: contracts.ActorUser, BaseVersion: &draft.StateVersion})
	if err != nil || restoreResult.Status != reducer.StatusApplied {
		t.Fatalf("restore=%#v err=%v", restoreResult, err)
	}
	rewound, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || rewound.TimelineCurrentVersion == nil ||
		*rewound.TimelineCurrentVersion != 3 || rewound.TimelineValidated {
		t.Fatalf("rewound draft=%#v err=%v", rewound, err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	allowed, err := service.Tools().Allowed(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, toolName := range []string{"render.start", "job.read"} {
		found := false
		for _, spec := range allowed {
			found = found || spec.Name == toolName
		}
		if !found {
			t.Fatalf("回退后工具面未放行 %s: %#v", toolName, allowed)
		}
	}
	currentTimeline, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := service.ExecuteTool(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "preview", TimelineID: currentTimeline.TimelineID,
	})
	if err != nil {
		t.Fatal(err)
	}
	renderResult := raw.(rushestools.ToolResult)
	if renderResult.Status != "queued" {
		t.Fatalf("render result=%#v", renderResult)
	}
	afterRender, err := storage.GetDraft(t.Context(), database.Read(), draftID)
	if err != nil || !afterRender.TimelineValidated ||
		afterRender.TimelineCurrentVersion == nil || *afterRender.TimelineCurrentVersion != 3 {
		t.Fatalf("after render=%#v err=%v", afterRender, err)
	}
	var validationEvents int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM event_log
		WHERE draft_id=? AND event_type='TimelineValidated'`, draftID,
	).Scan(&validationEvents); err != nil || validationEvents != 3 {
		t.Fatalf("validation events=%d err=%v", validationEvents, err)
	}
	var validationReportJSON *string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT validation_report_json FROM timeline_versions
		WHERE draft_id=? AND version=3`, draftID,
	).Scan(&validationReportJSON); err != nil || validationReportJSON == nil {
		t.Fatalf("rewound validation report=%v err=%v", validationReportJSON, err)
	}
	var payloadJSON string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT payload_json FROM jobs
		WHERE draft_id=? AND kind='render_preview'`, draftID,
	).Scan(&payloadJSON); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil ||
		payload["timeline_version"] != float64(3) {
		t.Fatalf("job payload=%#v err=%v", payload, err)
	}
}

func TestFallbackMainlineDecisionReplayStatusAndPreviewInspection(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_full")
	source := filepath.Join(database.Paths.Temporary, "full-mainline.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		"testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_full", "job_id": "job_full", "storage_mode": "reference",
			"reference_path": source, "kind": "video", "source": "local_path",
			"filename": "full-mainline.mp4", "hash": "full", "size": 1,
			"probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_full", Payload: map[string]any{"asset_id": "asset_full"}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_audio_only", "job_id": "job_audio_only", "storage_mode": "reference",
			"reference_path": "/tmp/not-used.mp3", "kind": "audio", "source": "local_path",
			"filename": "not-used.mp3", "hash": "audio", "size": 1,
			"probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_full", Payload: map[string]any{"asset_id": "asset_audio_only"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_full")
	content, err := service.fallbackMainline(ctx, "draft_full")
	if err != nil || content == "" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	document, err := timeline.Latest(t.Context(), database, "draft_full")
	if err != nil || len(document.Tracks[0].Clips) != 1 || document.Tracks[0].Clips[0].AssetKind != "video" {
		t.Fatalf("fallback 主视觉应过滤纯音频: document=%#v err=%v", document, err)
	}
	if service.Tools() == nil {
		t.Fatal("registry missing")
	}
	validatedRaw, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{})
	if err != nil {
		t.Fatal(err)
	}
	validated := validatedRaw.(rushestools.ToolResult)
	beatAlignment := validated.Data["beat_alignment"].(map[string]any)
	if beatAlignment["beat_grid_present"] != false ||
		!strings.Contains(validated.Observation, "不能证明画面切点已卡点") {
		t.Fatalf("validate without beat grid=%#v", validated)
	}
	if inspected, err := service.ExecuteTool(ctx, "timeline.inspect", rushestools.TimelineInspectInput{}); err != nil || inspected.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	var previewJobID string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT job_id FROM jobs
		WHERE draft_id='draft_full' AND kind='render_preview'
		ORDER BY created_at DESC LIMIT 1`,
	).Scan(&previewJobID); err != nil {
		t.Fatal(err)
	}
	status, err := service.ExecuteTool(ctx, "job.read", rushestools.JobReadInput{JobID: previewJobID})
	if err != nil || status.(rushestools.ToolResult).Data["job_status"] == nil {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	allowFreeText, blocking := false, false
	waiting, err := service.ExecuteTool(ctx, "interaction.ask_user", rushestools.AskUserInput{
		Question: "继续？", Options: []rushestools.DecisionOptionInput{{OptionID: "yes", Label: "继续"}},
		AllowFreeText: &allowFreeText, Blocking: &blocking, DecisionType: "critical",
	})
	if err != nil || waiting.(rushestools.ToolResult).Status != "succeeded" ||
		waiting.(rushestools.ToolResult).Data["turn_should_end"] != false {
		t.Fatalf("waiting=%#v err=%v", waiting, err)
	}
	decisionID := waiting.(rushestools.ToolResult).Data["decision_id"].(string)
	if _, err := service.ExecuteTool(ctx, "decision.answer", rushestools.DecisionAnswerInput{
		DecisionID: decisionID, OptionID: "yes", Payload: map[string]any{"source": "test"},
	}); err != nil {
		t.Fatal(err)
	}

	confirm, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认忘记？", ToolName: "memory.remove",
		Arguments: map[string]any{"keys": []any{"unused_preference"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	confirmID := confirm.(rushestools.ToolResult).Data["decision_id"].(string)
	decision, err := storage.GetDecision(t.Context(), database.Read(), confirmID)
	if err != nil || len(decision.Options) != 2 {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	var decisionCountBeforeInvalid int
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM decisions`).Scan(&decisionCountBeforeInvalid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []rushestools.ConfirmActionInput{
		{Question: "未知工具？", ToolName: "missing", Arguments: map[string]any{}},
		{Question: "嵌套确认？", ToolName: "interaction.confirm_action", Arguments: map[string]any{}},
		{Question: "错误字段？", ToolName: "timeline.inspect", Arguments: map[string]any{"unknown": true}},
		{Question: "空参数？", ToolName: "media.detect_shots", Arguments: nil},
		{Question: "缺少素材？", ToolName: "media.detect_shots", Arguments: map[string]any{}},
	} {
		raw, executeErr := service.ExecuteTool(ctx, "interaction.confirm_action", invalid)
		if executeErr != nil {
			t.Fatal(executeErr)
		}
		result := raw.(rushestools.ToolResult)
		if result.Status != "validation_failed" || result.Data["error_code"] != "invalid_confirmation_target" || result.Data["recovery"] == nil {
			t.Fatalf("invalid confirmation=%#v", result)
		}
	}
	var decisionCountAfterInvalid int
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM decisions`).Scan(&decisionCountAfterInvalid); err != nil {
		t.Fatal(err)
	}
	if decisionCountAfterInvalid != decisionCountBeforeInvalid {
		t.Fatalf("invalid confirmation created decision: before=%d after=%d", decisionCountBeforeInvalid, decisionCountAfterInvalid)
	}
	replayed, err := service.replayPendingTool(ctx, QueueItem{
		DraftID: "draft_full", Kind: QueueUIObservation,
		Payload: map[string]any{
			"pending_tool_call": decision.PendingToolCall,
			"answer":            map[string]any{"option_id": "confirm"},
		},
	})
	var replayGuardErr *terminalReplyGuardError
	if replayed != "" || !errors.As(err, &replayGuardErr) || replayGuardErr.kind != "tool_failure_unresolved" {
		t.Fatalf("replayed=%q err=%v", replayed, err)
	}
	if cancelled, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
		"pending_tool_call": decision.PendingToolCall, "answer": map[string]any{"option_id": "cancel"},
	}}); err != nil || cancelled != "已取消这项操作。" {
		t.Fatalf("cancelled=%q err=%v", cancelled, err)
	}
	if _, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
		"pending_tool_call": map[string]any{"tool_name": "media.detect_shots", "arguments": nil},
		"answer":            map[string]any{"option_id": "confirm"},
	}}); err == nil {
		t.Fatal("nil confirmation arguments must fail replay validation")
	}
	if observed, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full"}); err != nil || observed == "" {
		t.Fatalf("observed=%q err=%v", observed, err)
	}

	store := media.NewObjectStore(database.Paths)
	object, err := store.PutFile(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	result, err = reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: "draft_full", Payload: map[string]any{
			"artifact_id": "preview_inspect", "timeline_version": 1, "object_hash": object.Hash,
			"object_size": object.Size, "render_width": 320, "render_height": 240,
			"render_fps": 30, "expected_duration_sec": 1,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("preview status=%s err=%v", result.Status, err)
	}
	preview, err := service.ExecuteTool(ctx, "preview.check", rushestools.PreviewCheckInput{
		PreviewID: "preview_inspect", Check: "decode",
	})
	if err != nil || preview.(rushestools.PreviewInspectionResult).Summary == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	visualPreview, err := service.ExecuteTool(ctx, "preview.check", rushestools.PreviewCheckInput{
		PreviewID: "preview_inspect", Check: "visual",
	})
	visualResult := visualPreview.(rushestools.PreviewInspectionResult)
	if err != nil || !visualResult.Degraded || visualResult.VisualFrameCount == 0 ||
		len(visualResult.Issues) != 1 || visualResult.Issues[0]["check"] != "dependencies" {
		t.Fatalf("visual preview=%#v err=%v", visualResult, err)
	}
	if _, err := service.ExecuteTool(ctx, "preview.check", rushestools.PreviewCheckInput{
		PreviewID: "missing", Check: "decode",
	}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing preview err=%v", err)
	}
	if _, err := service.ExecuteTool(ctx, "asset.import_local_file", rushestools.AssetImportInput{}); err == nil {
		t.Fatal("harness-only import should reject direct execution")
	}
	if _, err := service.ExecuteTool(ctx, "unknown", struct{}{}); err == nil {
		t.Fatal("unknown tool should fail")
	}
}

func TestConfirmationChecksToolPreconditionsWhenCreatedAndReplayed(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_confirmation_preconditions"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	arguments := map[string]any{
		"kind": "delete_clip", "timeline_clip_id": "clip_v1_001",
	}

	missingRaw, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认删除片段？", ToolName: "timeline.delete", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	missing := missingRaw.(rushestools.ToolResult)
	if missing.Status != "validation_failed" || missing.Data["error_code"] != "invalid_confirmation_target" ||
		!strings.Contains(missing.Observation, "timeline_exists") {
		t.Fatalf("missing timeline confirmation=%#v", missing)
	}
	var decisionCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM decisions WHERE draft_id=?`, draftID,
	).Scan(&decisionCount); err != nil || decisionCount != 0 {
		t.Fatalf("invalid confirmation decision count=%d err=%v", decisionCount, err)
	}

	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_confirmation", AssetKind: "video", SourceEndFrame: 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service, t.Context(), draftID, document, "confirmation_precondition_fixture", nil); err != nil {
		t.Fatal(err)
	}
	confirmRaw, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认删除片段？", ToolName: "timeline.delete", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	confirm := confirmRaw.(rushestools.ToolResult)
	if confirm.Status != "waiting" {
		t.Fatalf("confirmation=%#v", confirm)
	}
	decision, err := storage.GetDecision(t.Context(), database.Read(), confirm.Data["decision_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		UPDATE drafts SET timeline_current_version=NULL WHERE draft_id=?`, draftID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := service.replayPendingTool(ctx, QueueItem{DraftID: draftID, Payload: map[string]any{
		"pending_tool_call": decision.PendingToolCall,
		"answer":            map[string]any{"option_id": "confirm"},
	}}); err == nil || !strings.Contains(err.Error(), "timeline_exists") {
		t.Fatalf("replay must be rejected by registry precondition guard: %v", err)
	}
	var timelineVersions int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM timeline_versions WHERE draft_id=?`, draftID,
	).Scan(&timelineVersions); err != nil || timelineVersions != 1 {
		t.Fatalf("rejected replay timeline versions=%d err=%v", timelineVersions, err)
	}
}

func TestFallbackAndReplayHelperBranches(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_empty")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_empty")
	if content, err := service.fallbackMainline(ctx, "draft_empty"); err != nil || content == "" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if _, err := service.fallbackTurn(ctx, "draft_empty", "msg", "ASK_USER"); err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline("draft_empty", 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "b_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service,
		t.Context(), "draft_empty", document, "fallback_export_fixture", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.fallbackTurn(ctx, "draft_empty", "msg", "导出"); err != nil {
		t.Fatal(err)
	}
	pending, err := storage.ListPendingDecisions(t.Context(), database.Read(), "draft_empty")
	if err != nil || len(pending) == 0 ||
		pending[len(pending)-1].PendingToolCall["tool_name"] != "render.start" {
		t.Fatalf("fallback export decision=%#v err=%v", pending, err)
	}
	arguments, _ := pending[len(pending)-1].PendingToolCall["arguments"].(map[string]any)
	if arguments["kind"] != "final" || arguments["timeline_id"] != "draft_empty:v1" {
		t.Fatalf("fallback export arguments=%#v", arguments)
	}
	if chunks := runeChunks("abcdef", 0); len(chunks) != 6 {
		t.Fatalf("chunks=%v", chunks)
	}
	if got := compactJSON(make(chan int)); got != "" {
		t.Fatalf("compact channel=%q", got)
	}
	if got := compactJSON(map[string]any{"long": string(make([]byte, 300))}); len(got) > 240 {
		t.Fatalf("compact length=%d", len(got))
	}
	for _, value := range []any{"yes", agentexec.StringPointerValue("pointer"), (*string)(nil), 1} {
		_ = agentexec.InterfaceString(value)
	}
	replayedPlan, err := service.tools.DecodeInput("plan.update", map[string]any{
		"plan": map[string]any{"style": "cinematic"}, "reset": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	planInput, ok := replayedPlan.(rushestools.PlanUpdateInput)
	if !ok || planInput.Plan["style"] != "cinematic" || planInput.Reset == nil || !*planInput.Reset {
		t.Fatalf("replayed plan input=%#v type=%T", replayedPlan, replayedPlan)
	}
	if _, err := service.tools.DecodeInput("missing", map[string]any{}); err == nil {
		t.Fatal("unknown replay should fail")
	}
	for _, value := range []any{float64(1), float32(2), 3, "bad"} {
		_, _ = agentexec.NumericValue(value)
	}
}

func TestServiceAndToolFailureBranches(t *testing.T) {
	t.Parallel()
	if _, err := NewService(t.Context(), nil, nil); err == nil {
		t.Fatal("nil database should fail")
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_failures")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_failures")
	for name, input := range map[string]any{
		"media.detect_shots":          rushestools.DetectShotsInput{},
		"audio.analyze_beats":         rushestools.AudioBeatAnalysisInput{},
		"audio.analyze_speech_pauses": rushestools.SpeechPauseAnalysisInput{},
		"timeline.check":              rushestools.TimelineCheckInput{},
		"timeline.inspect":            rushestools.TimelineInspectInput{},
		"decision.answer":             rushestools.DecisionAnswerInput{DecisionID: "missing"},
	} {
		output, err := service.ExecuteTool(ctx, name, input)
		if name == "timeline.inspect" {
			result := output.(rushestools.ToolResult)
			if err != nil || result.Status != "succeeded" || result.Data["timeline_exists"] != false {
				t.Fatalf("%s output=%#v err=%v", name, output, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s should fail", name)
		}
	}
	invalid := timeline.Empty("draft_failures", 1)
	invalid.FPS = 0
	invalid.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "bad", TrackID: "visual_base", AssetID: "a", TimelineEndFrame: 1, SourceEndFrame: 1,
	}}
	result, err := seedTimelineVersion(service, ctx, "draft_failures", invalid, "invalid", nil)
	if err != nil || result.Status != "validation_failed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := service.ExecuteTool(ctx, "timeline.check", rushestools.TimelineCheckInput{}); err != nil {
		t.Fatal(err)
	}

	agenttest.CreateAgentDraft(t, database, "draft_assets_filter")
	for _, item := range []struct {
		id     string
		kind   string
		usable bool
	}{
		{"a", "video", true}, {"b", "audio", false}, {"c", "video", true},
	} {
		result, err := reducer.Apply(t.Context(), database, []contracts.Event{
			{Type: "AssetImported", Payload: map[string]any{
				"asset_id": item.id, "job_id": "job_" + item.id, "kind": item.kind, "filename": item.id,
				"usable": item.usable, "probe": map[string]any{"duration_sec": float32(2)},
			}},
			{Type: "AssetLinked", DraftID: "draft_assets_filter", Payload: map[string]any{"asset_id": item.id}},
		}, reducer.Options{Actor: contracts.ActorUser})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("asset=%s result=%#v err=%v", item.id, result, err)
		}
	}
	filtered, err := service.executor.ToolListAssets(ctx, "draft_assets_filter", rushestools.AssetListInput{
		Kind: "video", After: "a", Limit: 1, OnlyUsable: agentexec.BoolPointer(true),
	})
	if err != nil || len(filtered.Assets) != 1 || filtered.Assets[0].AssetID != "c" {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
	for _, fragment := range []string{
		"asset_id", "filename", "kind", "rel_dir", "suggested_role", "suggested_visual_role",
		"duration_frames", "timeline_fps", "usable=false", "ingest_status", "understanding_status",
	} {
		if !strings.Contains(filtered.UsageNote, fragment) {
			t.Fatalf("asset usage note missing %q: %q", fragment, filtered.UsageNote)
		}
	}
	encodedAssetResult, err := json.Marshal(filtered)
	if err != nil || !strings.Contains(string(encodedAssetResult), `"usage_note":"asset_id`) {
		t.Fatalf("asset result 未把字段口径序列化给模型: %s err=%v", encodedAssetResult, err)
	}
	audio, err := service.executor.ToolListAssets(ctx, "draft_assets_filter", rushestools.AssetListInput{Kind: "audio"})
	if err != nil || len(audio.Assets) != 1 || audio.Assets[0].SuggestedRole != "sfx" {
		t.Fatalf("audio role=%#v err=%v", audio, err)
	}
	objectiveContext, err := service.contextManager.builder.Build(t.Context(), "draft_assets_filter")
	if err != nil || !strings.Contains(objectiveContext, `"audio":1`) ||
		!strings.Contains(objectiveContext, `"suggested_role":"sfx"`) {
		t.Fatalf("objective context=%q err=%v", objectiveContext, err)
	}
	validObjectiveTimeline := timeline.Empty("draft_assets_filter", 1)
	validObjectiveTimeline.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "objective_clip", TrackID: "visual_base", AssetID: "a", AssetKind: "video",
		Role: "video", TimelineEndFrame: 1, SourceEndFrame: 1, PlaybackRate: 1,
	}}
	timelineResult, err := seedTimelineVersion(service,
		t.Context(), "draft_assets_filter", validObjectiveTimeline, "objective_valid", nil)

	if err != nil || timelineResult.Status != "succeeded" {
		t.Fatalf("valid objective timeline=%#v err=%v", timelineResult, err)
	}
	objectiveContext, err = service.contextManager.builder.Build(t.Context(), "draft_assets_filter")
	if err != nil || !strings.Contains(objectiveContext, `"validated":true`) {
		t.Fatalf("validated objective context=%q err=%v", objectiveContext, err)
	}
	invalidObjectiveTimeline := validObjectiveTimeline
	invalidObjectiveTimeline.TimelineID = "draft_assets_filter:v2"
	invalidObjectiveTimeline.Version = 2
	invalidObjectiveTimeline.FPS = 0
	timelineResult, err = seedTimelineVersion(service,
		t.Context(), "draft_assets_filter", invalidObjectiveTimeline, "objective_invalid", nil)

	if err != nil || timelineResult.Status != "validation_failed" {
		t.Fatalf("invalid objective timeline=%#v err=%v", timelineResult, err)
	}
	objectiveContext, err = service.contextManager.builder.Build(t.Context(), "draft_assets_filter")
	if err != nil || !strings.Contains(objectiveContext, `"validated":false`) {
		t.Fatalf("unvalidated objective context=%q err=%v", objectiveContext, err)
	}
}

func TestSpeechPauseAnalysisSupportsVideoAudioAndTimelineMapping(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_speech_pauses")
	source := filepath.Join(database.Paths.Temporary, "talking-head.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=160x90:r=30:d=3",
		"-f", "lavfi", "-i", `aevalsrc=if(between(t\,1\,2)\,0\,0.7*sin(2*PI*440*t)):s=44100:d=3`,
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", source,
	); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "talk_video", "job_id": "job_talk_video", "storage_mode": "reference",
			"reference_path": source, "kind": "video", "source": "local_path", "filename": "talking-head.mp4",
			"hash": "talk_video_hash", "size": info.Size(), "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 3, "has_audio": true, "fps": 30},
		}},
		{Type: "AssetLinked", DraftID: "draft_speech_pauses", Payload: map[string]any{"asset_id": "talk_video"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_speech_pauses")
	document, err := agenttest.ComposeTimeline("draft_speech_pauses", 1, []agenttest.TimelineSelection{{
		AssetID: "talk_video", AssetKind: "video", SourceEndFrame: 90, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(service,
		t.Context(), "draft_speech_pauses", document, "speech_pause_fixture", nil); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, persistErr)
	}
	output, err := service.ExecuteTool(ctx, "audio.analyze_speech_pauses", rushestools.SpeechPauseAnalysisInput{
		TimelineClipID: "clip_v1_001", ThresholdDB: -35, MinPauseFrames: 6, KeepEdgeFrames: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	analysis := output.(rushestools.SpeechPauseAnalysisResult)
	if analysis.AssetID != "talk_video" || analysis.TimelineFPS != 30 || len(analysis.Pauses) != 1 {
		t.Fatalf("analysis=%#v", analysis)
	}
	pause := analysis.Pauses[0]
	if pause.TimelineStartFrame == nil || pause.TimelineEndFrame == nil ||
		*pause.TimelineStartFrame < 25 || *pause.TimelineEndFrame > 65 ||
		*pause.TimelineEndFrame <= *pause.TimelineStartFrame {
		t.Fatalf("pause=%#v", pause)
	}
}

func TestAudioBeatPhaseNoteWarnsThatBeatEvidenceIsNotCreativeJudgment(t *testing.T) {
	t.Parallel()
	if !strings.Contains(agentexec.AudioBeatPhaseNote, "高潮") ||
		!strings.Contains(agentexec.AudioBeatPhaseNote, "不能自动等同") ||
		!strings.Contains(agentexec.AudioBeatPhaseNote, "好剪辑") {
		t.Fatalf("phase note missing creative-judgment warning: %q", agentexec.AudioBeatPhaseNote)
	}
	for _, fragment := range []string{
		"sample_frames", "samples 一一对应", "timeline_fps", "完整压缩波形", "WorldState", "24 点摘要",
	} {
		if !strings.Contains(agentexec.AudioWaveformUsageNote, fragment) {
			t.Fatalf("waveform usage note missing %q: %q", fragment, agentexec.AudioWaveformUsageNote)
		}
	}
	encodedWaveformResult, err := json.Marshal(rushestools.AudioBeatAnalysisResult{
		WaveformUsageNote: agentexec.AudioWaveformUsageNote,
	})
	if err != nil || !strings.Contains(string(encodedWaveformResult), `"waveform_usage_note":"waveform.sample_frames`) {
		t.Fatalf("waveform result 未把字段口径序列化给模型: %s err=%v", encodedWaveformResult, err)
	}
}

func TestAudioBeatAnalysisToolReturnsIntegerFrameGrid(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	if _, err := exec.LookPath("aubiotrack"); err != nil {
		t.Skip("aubio 未安装")
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_beats")
	source := filepath.Join(database.Paths.Temporary, "metronome.wav")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i",
		`aevalsrc=if(lt(mod(t\,0.5)\,0.03)\,0.9*sin(2*PI*1000*t)\,0):s=44100:d=5`,
		"-c:a", "pcm_s16le", source); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "beat_audio", "job_id": "job_beat_audio", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "metronome.wav",
			"hash": "beat_audio_hash", "size": info.Size(), "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 5, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_beats", Payload: map[string]any{"asset_id": "beat_audio"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("asset status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_beats")
	output, err := service.ExecuteTool(ctx, "audio.analyze_beats", rushestools.AudioBeatAnalysisInput{
		AssetID: "beat_audio", MaxBeats: 32, WaveformPoints: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	beats := output.(rushestools.AudioBeatAnalysisResult)
	if beats.BPM < 110 || beats.BPM > 130 || len(beats.BeatFrames) < 4 ||
		len(beats.EveryTwoBeatFrames) < 2 || beats.TimelineFPS != 30 ||
		beats.Waveform.SampleIntervalFrames <= 0 || len(beats.Waveform.Samples) == 0 ||
		len(beats.Waveform.SampleFrames) != len(beats.Waveform.Samples) ||
		len(beats.Waveform.Samples) > 32 || beats.Waveform.Encoding != media.WaveformEncoding ||
		!strings.Contains(beats.PhaseNote, "不能自动等同于高潮或好剪辑") ||
		beats.WaveformUsageNote != agentexec.AudioWaveformUsageNote {
		t.Fatalf("beats=%#v", beats)
	}
}

func TestAudioLayoutDataWarnsWhenSFXDoesNotAccentBGM(t *testing.T) {
	document := timeline.Empty("draft_audio_layout", 1)
	document.DurationFrames = 300
	for index := range document.Tracks {
		switch document.Tracks[index].TrackID {
		case "bgm":
			document.Tracks[index].Clips = []timeline.Clip{{
				TimelineClipID: "bgm_1", TrackID: "bgm", TimelineStartFrame: 0, TimelineEndFrame: 180,
			}}
		case "sfx":
			document.Tracks[index].Clips = []timeline.Clip{{
				TimelineClipID: "sfx_late", TrackID: "sfx", TimelineStartFrame: 210, TimelineEndFrame: 240,
			}}
		}
	}
	layout := agentexec.AudioLayoutData(document)
	warnings := layout["warnings"].([]string)
	without := layout["sfx_without_bgm"].([]string)
	if len(warnings) != 2 || len(without) != 1 || without[0] != "sfx_late" {
		t.Fatalf("layout=%#v", layout)
	}
}

func TestModelFailureStillRepliesAndEndsTurn(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_model_error")
	agenttest.InsertAgentMessage(t, database, "draft_model_error", "user_error", "fail")
	service, err := NewService(t.Context(), database, &failingServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_model_error")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_model_error", "user_error", "fail")
	service.Queue().JoinDraft("draft_model_error")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("模型失败不应让回合静默终止")
			case "message_completed":
				completed, _ = event["content"].(string)
				// 失败终态复用现有 message 类事件通道：live message_completed
				// 以 kind=turn_failure 呈现，前端据此渲染失败提示行。
				if event["kind"] != "turn_failure" {
					t.Fatalf("失败回合 message_completed 应带 kind=turn_failure：%#v", event)
				}
			case "turn_ended":
				if event["outcome"] != "failed" || !strings.Contains(completed, "本轮没有完成") {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				// 回合失败终态必须持久化为 role=system, kind=turn_failure 消息，
				// 刷新页面能从 DB 读回而不再无声死亡（issue #95 H2）。
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_model_error", 20)
				if listErr != nil || len(messages) < 2 {
					t.Fatalf("messages=%#v err=%v", messages, listErr)
				}
				failure := messages[len(messages)-1]
				if failure.Role != "system" || failure.Kind != "turn_failure" ||
					failure.Content != completed {
					t.Fatalf("失败终态未落库为系统失败消息：%#v", failure)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("失败回合没有给用户回复并正常收尾")
		}
	}
}

func TestEmptyModelReplyStillProducesVisibleFailure(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_empty_model")
	agenttest.InsertAgentMessage(t, database, "draft_empty_model", "user_empty_model", "不要静默")
	service, err := NewService(t.Context(), database, &emptyServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_empty_model")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_empty_model", "user_empty_model", "不要静默")
	service.Queue().JoinDraft("draft_empty_model")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "message_completed":
				completed, _ = event["content"].(string)
			case "turn_ended":
				if event["outcome"] != "failed" || !strings.Contains(completed, "本轮没有完成") ||
					!strings.Contains(completed, "模型没有生成最终回复") {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("空模型回复导致回合静默")
		}
	}
}

func TestExhaustedRecoveryReplyIsVisibleAndMarkedFailed(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_bounded_recovery")
	agenttest.InsertAgentMessage(t, database, "draft_bounded_recovery", "user_bounded_recovery", "不要循环")
	service, err := NewService(t.Context(), database, &terminatingFailureLoopModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_bounded_recovery")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_bounded_recovery", "user_bounded_recovery", "不要循环")
	service.Queue().JoinDraft("draft_bounded_recovery")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "message_completed":
				completed, _ = event["content"].(string)
				if event["kind"] != "turn_failure" {
					t.Fatalf("恢复耗尽必须由 harness 输出确定性失败：%#v", event)
				}
			case "turn_ended":
				if event["outcome"] != "failed" || !strings.Contains(completed, "工具自修复次数已经用尽") ||
					strings.Contains(completed, "已经全部完成") {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_bounded_recovery", 20)
				if listErr != nil {
					t.Fatal(listErr)
				}
				toolRows := 0
				for _, message := range messages {
					if message.Kind == "tool" {
						toolRows++
					}
				}
				if toolRows != 1 {
					t.Fatalf("重复失败不应污染 UI：tool_rows=%d messages=%#v", toolRows, messages)
				}
				reply := messages[len(messages)-1]
				if reply.Role != "system" || reply.Kind != "turn_failure" || reply.Content != completed {
					t.Fatalf("恢复耗尽的确定性失败应落 system/turn_failure：%#v", reply)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("恢复预算耗尽后没有可见终态")
		}
	}
}

func TestExhaustedRecoveryOverridesProviderErrorWithoutModelFallback(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_bounded_recovery_provider_error"
	agenttest.CreateAgentDraft(t, database, draftID)
	agenttest.InsertAgentMessage(t, database, draftID, "user_bounded_recovery_error", "不要循环")
	modelValue := &terminatingFailureLoopModel{terminalErr: true}
	service, err := NewService(t.Context(), database, modelValue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.Queue().EnqueueUserMessage(draftID, "user_bounded_recovery_error", "不要循环")
	service.Queue().JoinDraft(draftID)

	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil {
		t.Fatal(err)
	}
	final := messages[len(messages)-1]
	if final.Kind != "turn_failure" || !strings.Contains(final.Content, "工具自修复次数已经用尽") ||
		strings.Contains(final.Content, "provider stream failed") {
		t.Fatalf("恢复耗尽应覆盖 provider 错误并使用固定正文：%#v", final)
	}
	modelValue.mu.Lock()
	calls := modelValue.calls
	modelValue.mu.Unlock()
	if calls != maxModelRepairAttempts+2 {
		t.Fatalf("恢复耗尽后不得为失败收尾再次调用模型：calls=%d", calls)
	}
}

func TestRepeatedFailedToolLoopStillRepliesAndEndsTurn(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_tool_loop")
	agenttest.InsertAgentMessage(t, database, "draft_tool_loop", "user_tool_loop", "loop")
	service, err := NewService(t.Context(), database, &loopingFailureServiceModel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_tool_loop")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_tool_loop", "user_tool_loop", "loop")
	service.Queue().JoinDraft("draft_tool_loop")
	completed := ""
	for {
		select {
		case event := <-stream:
			switch event["type"] {
			case "turn_error":
				t.Fatal("重复工具失败不应让 UI 卡死")
			case "message_completed":
				completed, _ = event["content"].(string)
			case "turn_ended":
				if event["outcome"] != "failed" || !strings.Contains(completed, "本轮没有完成") ||
					!strings.Contains(completed, "模型修复失败次数") {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("重复失败回合没有给用户回复并结束")
		}
	}
}

func TestJobBridgeSkipsMalformedAndUnrelatedEvents(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows := []string{
		`not-json`,
		`{"event":"JobSucceeded","payload":{"kind":"noop","job_id":"j"}}`,
		`{"event":"JobSucceeded","payload":{"kind":"render_preview","job_id":"j"}}`,
		`{"event":"JobSucceeded","draft_id":"missing","payload":{"kind":"render_preview"}}`,
	}
	for index, payload := range rows {
		if _, err := database.Write().ExecContext(t.Context(), `
			INSERT INTO event_log(event_type,actor,payload_json,created_at) VALUES('JobSucceeded','job',?,?)`, payload, now); err != nil {
			t.Fatalf("row=%d err=%v", index, err)
		}
	}
	if cursor := service.bridgeIteration(t.Context(), 0); cursor != int64(len(rows)) {
		t.Fatalf("cursor=%d", cursor)
	}
}

func TestServiceClosedDatabaseFailureBoundaries(t *testing.T) {
	if agentexec.StringPointerValue("") != nil {
		t.Fatal("空字符串不应生成指针")
	}
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, "draft_closed")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.Close()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_closed")
	for name, input := range map[string]any{
		"asset.list_assets":           rushestools.AssetListInput{},
		"media.detect_shots":          rushestools.DetectShotsInput{AssetID: "asset"},
		"shot.search":                 rushestools.ShotSearchInput{},
		"audio.analyze_beats":         rushestools.AudioBeatAnalysisInput{AssetID: "asset"},
		"audio.analyze_speech_pauses": rushestools.SpeechPauseAnalysisInput{AssetID: "asset"},
		"interaction.ask_user":        rushestools.AskUserInput{Question: "?", DecisionType: "critical"},
		"decision.answer":             rushestools.DecisionAnswerInput{DecisionID: "decision"},
		"plan.update":                 rushestools.PlanUpdateInput{Plan: map[string]any{"status": "closed-db"}},
		"timeline.check":              rushestools.TimelineCheckInput{},
		"timeline.inspect":            rushestools.TimelineInspectInput{},
		"preview.check":               rushestools.PreviewCheckInput{PreviewID: "preview", Check: "decode"},
	} {
		if _, err := service.ExecuteTool(ctx, name, input); err == nil {
			t.Fatalf("closed database: %s 应失败", name)
		}
	}
	if _, _, err := service.executor.FindRenderJob(t.Context(), "render_preview", "closed", false); err == nil {
		t.Fatal("closed findRenderJob 应失败")
	}
	if _, err := service.modelMessages(ctx, "draft_closed"); err == nil {
		t.Fatal("closed modelMessages 应失败")
	}
	if _, err := service.fallbackMainline(ctx, "draft_closed"); err == nil {
		t.Fatal("closed fallback mainline 应失败")
	}
	if _, err := seedTimelineVersion(service, ctx, "draft_closed", timeline.Empty("draft_closed", 1), "closed", nil); err == nil {
		t.Fatal("closed persist timeline 应失败")
	}
	if err := service.runTurn(t.Context(), QueueItem{
		DraftID: "draft_closed", Kind: QueueUserMessage,
		Payload: map[string]any{"content": "ordinary"},
	}); err == nil {
		t.Fatal("assistant message 持久化到关闭数据库应失败")
	}
	for _, event := range service.hub.Snapshot("draft_closed") {
		if event["type"] == TurnStreamTextDelta || event["type"] == TurnStreamMessageCompleted {
			t.Fatalf("持久化失败前不得泄漏最终正文事件：%#v", event)
		}
	}
	if cursor := service.bridgeIteration(t.Context(), 9); cursor != 9 {
		t.Fatalf("closed bridge cursor=%d", cursor)
	}
	reporter := service.toolReporter(t.Context(), "draft_closed")
	reporter(t.Context(), "orphan", "finished", nil, nil, errors.New("tool failed"))
}
