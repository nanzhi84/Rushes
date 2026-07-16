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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
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
	mu    sync.Mutex
	calls int
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
			ID: randomID("bounded_loop_call"),
			Function: schema.FunctionCall{
				Name: "timeline.nonexistent", Arguments: `{"same":true}`,
			},
		}}), nil
	}
	return schema.AssistantMessage("本轮工具修复未完成，请告诉我下一步怎么处理。", nil), nil
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
			ID: randomID("budget_round_call"),
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
			ID: "bad_call", Function: schema.FunctionCall{Name: "timeline.nonexistent", Arguments: `{}`},
		}}), nil
	case 2:
		if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool ||
			!strings.Contains(messages[len(messages)-1].Content, "unknown_tool") {
			return nil, errors.New("未知工具错误没有回灌模型")
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
		ID:       randomID("loop_call"),
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
	return schema.AssistantMessage("工具失败，但本轮已正常回复。", nil), nil
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_react")
	insertAgentMessage(t, database, "draft_react", "user_msg", "列出素材")
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
	if maxToolRoundsPerTurn != 40 || maxReActStepsPerTurn != 81 {
		t.Fatalf(
			"budget policy hard=%d steps=%d",
			maxToolRoundsPerTurn, maxReActStepsPerTurn,
		)
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_tool_round_budget")
	insertAgentMessage(t, database, "draft_tool_round_budget", "user_tool_round_budget", "连续执行多轮工具")
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
	database := agentTestDatabase(t)
	const draftID = "draft_thirty_round_fixture"
	createAgentDraft(t, database, draftID)
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_self_repair")
	insertAgentMessage(t, database, "draft_self_repair", "user_self_repair", "先试错再修复")
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

func TestServiceReturnsDeterministicToolFailureInOneVisibleTrace(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_retry_trace")
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
	insertAgentMessage(t, database, "draft_retry_trace", "user_retry_trace", "分析不存在的音频")
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
	if toolRows != 1 || len(messages) != 3 || messages[len(messages)-1].Content != "工具失败，但本轮已正常回复。" {
		t.Fatalf("确定性失败应立即返回且只展示一个工具终态：tool_rows=%d messages=%#v", toolRows, messages)
	}
}

func TestDecisionAnswerObservationResumesAgent(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_decision_continue")
	insertAgentMessage(t, database, "draft_decision_continue", "user_decision_continue", "帮我做一个混剪")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_job_continue")
	insertAgentMessage(t, database, "draft_job_continue", "user_job_continue", "生成预览并检查")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_job_dedup")
	insertAgentMessage(t, database, "draft_job_dedup", "user_job_dedup", "渲染预览并检查")
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: "tool_inspected", DraftID: "draft_job_dedup", Role: "system", Kind: "tool",
			Content: `{"tool":"render.inspect_preview","args_summary":"{\"preview_id\":\"preview_done\"}","observation":"{\"summary\":\"ok\"}","status":"succeeded"}`,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("tool message status=%s err=%v", result.Status, err)
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
		`{"tool":"render.inspect_preview","args_summary":"{\"preview_id\":\"preview_done\"}","status":"failed"}`,
		`{"tool":"render.inspect_preview","args_summary":"not-json","status":"succeeded"}`,
	} {
		result, err = reducer.Apply(t.Context(), database, nil, reducer.Options{
			Actor: contracts.ActorAgent, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
				ID: fmt.Sprintf("tool_ignored_%d", index), DraftID: "draft_job_dedup",
				Role: "system", Kind: "tool", Content: content,
			}},
		})
		if err != nil || result.Status != reducer.StatusApplied {
			t.Fatalf("ignored tool message status=%s err=%v", result.Status, err)
		}
	}
	if service.previewAlreadyInspected(t.Context(), "draft_job_dedup", nil) {
		t.Fatal("缺少预览 ID 时不应判定为已质检")
	}
	if !service.previewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"preview_id": "preview_done"}) {
		t.Fatal("应兼容 preview_id 形式的终态结果")
	}
	if service.previewAlreadyInspected(t.Context(), "draft_job_dedup", map[string]any{"artifact_id": "preview_other"}) {
		t.Fatal("不同预览不应被误去重")
	}
	cancelledContext, cancel := context.WithCancel(t.Context())
	cancel()
	if service.previewAlreadyInspected(cancelledContext, "draft_job_dedup", map[string]any{"artifact_id": "preview_done"}) {
		t.Fatal("读取历史失败时应保守地继续后台回调")
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_job_dedup", 20)
	if err != nil || len(messages) != 5 {
		t.Fatalf("去重后不应生成重复回复: messages=%#v err=%v", messages, err)
	}
}

func TestCompletedPreviewObservationIncludesStructuredVerificationReport(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_job_report")
	insertAgentMessage(t, database, "draft_job_report", "user_job_report", "生成预览并按合同检查")
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	planResult, err := service.toolPlanUpdate(t.Context(), "draft_job_report", rushestools.PlanUpdateInput{
		Plan: map[string]any{}, Contract: &rushestools.ContentPlanContract{TargetDurationFrames: 30},
	})
	if err != nil || planResult.Status != "succeeded" {
		t.Fatalf("plan=%#v err=%v", planResult, err)
	}
	document, err := timeline.ComposeInitial("draft_job_report", 1, []timeline.Selection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.persistTimeline(t.Context(), "draft_job_report", document, "preview_report_fixture"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(database.Paths.Temporary, "preview-report.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=64x64:d=1:r=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-c:a", "aac", path,
	); err != nil {
		t.Fatal(err)
	}
	object, err := media.NewObjectStore(database.Paths).PutFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeFFmpeg := filepath.Join(fakeBin, "ffmpeg")
	fakeScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then\n  echo 'decoder leaked %s %s' >&2\n  exit 1\nfi\nexit 0\n", object.Hash, object.Path)
	if err := os.WriteFile(fakeFFmpeg, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	applyResult, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: "draft_job_report", Payload: map[string]any{
			"artifact_id": "preview_report", "timeline_version": 1,
			"object_hash": object.Hash, "object_size": object.Size,
			"render_width": 64, "render_height": 64, "render_fps": 30,
			"expected_duration_sec": 1,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || applyResult.Status != reducer.StatusApplied {
		t.Fatalf("preview status=%s err=%v", applyResult.Status, err)
	}
	report, err := service.previewVerificationReport(t.Context(), "draft_job_report", map[string]any{"artifact_id": "preview_report"})
	if err != nil {
		t.Fatal(err)
	}
	encodedReport, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"error_code":"preview_decode_failed"`, "预览视频无法完整解码。"} {
		if !strings.Contains(string(encodedReport), expected) {
			t.Fatalf("verification report missing %q: %s", expected, encodedReport)
		}
	}
	if !service.Queue().EnqueueJobObservation("draft_job_report", "job_report", map[string]any{
		"event": "JobSucceeded",
		"payload": map[string]any{
			"job_id": "job_report", "kind": "render_preview",
			"result": map[string]any{"artifact_id": "preview_report"},
		},
	}) {
		t.Fatal("job observation 未入队")
	}
	service.Queue().JoinDraft("draft_job_report")
	prompt := chatModel.lastPrompt()
	for _, expected := range []string{
		"verification_report", "render_inspection", "content_contract", `"pass":true`, "preview_report",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q: %s", expected, prompt)
		}
	}
	for _, sensitive := range []string{object.Path, object.Hash, "decoder leaked", "ffmpeg"} {
		if strings.Contains(prompt, sensitive) {
			t.Fatalf("prompt leaked decode detail %q: %s", sensitive, prompt)
		}
	}
}

func TestCompletedPreviewObservationContinuesWhenAutomaticInspectionFails(t *testing.T) {
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_job_degraded_report")
	insertAgentMessage(t, database, "draft_job_degraded_report", "user_job_degraded_report", "生成预览")
	chatModel := &decisionContinuationModel{}
	service, err := NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial("draft_job_degraded_report", 1, []timeline.Selection{{
		AssetID: "fixture", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.persistTimeline(t.Context(), "draft_job_degraded_report", document, "preview_degraded_fixture"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	missingHash := strings.Repeat("a", 64)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?,?,1,?);
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview_degraded','draft_job_degraded_report',1,?,'{}',?)`,
		missingHash, missingHash, now, missingHash, now,
	); err != nil {
		t.Fatal(err)
	}
	if !service.Queue().EnqueueJobObservation("draft_job_degraded_report", "job_degraded_report", map[string]any{
		"event": "JobSucceeded",
		"payload": map[string]any{
			"job_id": "job_degraded_report", "kind": "render_preview",
			"result": map[string]any{"artifact_id": "preview_degraded"},
		},
	}) {
		t.Fatal("job observation 未入队")
	}
	service.Queue().JoinDraft("draft_job_degraded_report")
	prompt := chatModel.lastPrompt()
	for _, expected := range []string{
		"状态：成功", "verification_report", `"degraded":true`, `"check":"inspection"`,
		`"error_code":"preview_inspection_unavailable"`, "自动质检暂不可用，请稍后重试。", "preview_degraded",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q: %s", expected, prompt)
		}
	}
	for _, sensitive := range []string{database.Paths.Objects, missingHash, "ffprobe", "No such file", "no such file"} {
		if strings.Contains(prompt, sensitive) {
			t.Fatalf("prompt leaked inspection detail %q: %s", sensitive, prompt)
		}
	}
}

func TestServiceCancellationPropagatesToTurnContext(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_cancel")
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

func TestJobObservationBridgeWakesAgentForWaitedTerminalJob(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge_restart")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge_outbox")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge_retry")
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
			database := agentTestDatabase(t)
			draftID := "draft_bridge_cancel_" + name
			jobID := "job_bridge_cancel_" + name
			createAgentDraft(t, database, draftID)
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
	database := agentTestDatabase(t)
	draftID := "draft_bridge_persistent_suppression"
	jobID := "job_bridge_persistent_suppression"
	createAgentDraft(t, database, draftID)
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
	database := agentTestDatabase(t)
	draftID := "draft_bridge_failure_paths"
	createAgentDraft(t, database, draftID)
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

	closedDatabase := agentTestDatabase(t)
	if err := closedDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	closedService := &Service{database: closedDatabase, bridgeInflight: map[string]struct{}{}}
	if closedService.jobObservationSuppressed(t.Context(), "job") {
		t.Fatal("存储不可用时不得把 observation 误判为已抑制")
	}
	closedService.markJobObservationDelivered(t.Context(), "job", "claim")
	if page, _, scanned := closedService.pendingJobObservationPage(t.Context(), 9); page != nil || scanned != -1 {
		t.Fatalf("存储不可用时 page=%v scanned=%d", page, scanned)
	}
	closedService.startJobObservationBridge(t.Context())
	if cursor := closedService.bridgeIteration(t.Context(), 9); cursor != 9 {
		t.Fatalf("存储不可用时 cursor=%d", cursor)
	}
}

func TestJobObservationBridgeScansPastInflightPage(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge_page")
	observations := make([]reducer.AgentJobObservationRow, 0, 101)
	for index := 1; index <= 101; index++ {
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
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor:      contracts.ActorAgent,
		ResultRows: reducer.ResultRows{AgentJobObservations: observations},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed status=%s err=%v", result.Status, err)
	}

	queueCtx, cancelQueue := context.WithCancel(t.Context())
	service := &Service{database: database, hub: NewTurnStreamHub(0), bridgeInflight: map[string]struct{}{}}
	queue := NewTurnQueue(queueCtx, func(runCtx context.Context, _ QueueItem) error {
		<-runCtx.Done()
		return runCtx.Err()
	})
	service.queue = queue
	t.Cleanup(func() {
		cancelQueue()
		queue.Close()
	})

	service.dispatchPendingJobObservations(t.Context())
	service.dispatchPendingJobObservations(t.Context())
	service.bridgeMu.Lock()
	_, scheduled := service.bridgeInflight["job_page_101"]
	inflightCount := len(service.bridgeInflight)
	service.bridgeMu.Unlock()
	if !scheduled || inflightCount != 101 {
		t.Fatalf("job 101 scheduled=%v inflight=%d", scheduled, inflightCount)
	}
}

func TestJobObservationBridgeDeduplicatesJobAndSplitsCancellationReason(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bridge_cancel")
	for _, item := range []struct {
		id, reason string
	}{{"job_turn_cancel", "turn_cancelled"}, {"job_manual_cancel", "user_cancelled"}} {
		if _, err := reducer.Apply(t.Context(), database, []contracts.Event{
			{Type: "JobEnqueued", DraftID: "draft_bridge_cancel", Payload: map[string]any{
				"job_id": item.id, "kind": "understand",
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_understand_repeat")
	font := filepath.Join(database.Paths.Temporary, "repeat.otf")
	if err := os.WriteFile(font, []byte("font fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_repeat", "job_id": "job_repeat", "storage_mode": "reference",
			"reference_path": font, "kind": "font", "source": "local_path",
			"filename": "repeat.otf", "hash": "repeat", "size": 1, "ingest_status": "ready",
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
		output, err := service.ExecuteTool(ctx, "understand.materials", rushestools.UnderstandInput{
			AssetIDs: []string{"asset_repeat"}, Focus: focus,
		})
		if err != nil {
			t.Fatalf("focus=%s err=%v", focus, err)
		}
		understood := output.(rushestools.UnderstandResult)
		if len(understood.Summaries) != 1 || understood.Summaries[0].AssetID != "asset_repeat" ||
			understood.Summaries[0].Overall == "" || len(understood.Summaries[0].Evidence) != 1 ||
			understood.Summaries[0].Evidence[0].SourceEndFrame <=
				understood.Summaries[0].Evidence[0].SourceStartFrame {
			t.Fatalf("understand 同回合缺少摘要或时间证据: %#v", understood)
		}
		if len(understood.CacheHitAssetIDs) > 0 {
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

func TestTimelineToolsComposePatchValidateInspectRestoreAndQueueRender(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_timeline_tools")
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "asset_timeline", "job_id": "job_asset", "storage_mode": "reference",
			"reference_path": "/tmp/not-read-during-compose.mp4", "kind": "video",
			"source": "local_path", "filename": "clip.mp4", "hash": "hash", "size": 1,
			"probe": map[string]any{"duration_sec": 3}, "ingest_status": "ready",
		}},
		{Type: "AssetLinked", DraftID: "draft_timeline_tools", Payload: map[string]any{"asset_id": "asset_timeline"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_timeline_tools")
	if _, err := service.ExecuteTool(ctx, "timeline.compose_initial", rushestools.ComposeInitialInput{
		Clips: []rushestools.ComposeClip{{
			AssetID: "asset_timeline", SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	draft, _ := storage.GetDraft(t.Context(), database.Read(), "draft_timeline_tools")
	if draft.TimelineCurrentVersion == nil || *draft.TimelineCurrentVersion != 1 || !draft.TimelineValidated {
		t.Fatalf("draft after compose=%#v", draft)
	}
	if _, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: map[string]any{
		"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -2.0,
	}}); err != nil {
		t.Fatal(err)
	}
	batchRaw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{
		{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -3.0},
		{"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 1.25},
	}})
	if err != nil || batchRaw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("batch=%#v err=%v", batchRaw, err)
	}
	failedBatchRaw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{
		"kind": "insert_clip", "track_id": "sfx", "timeline_start_frame": 1000,
		"asset_id": "missing", "asset_kind": "audio", "source_start_frame": 0, "source_end_frame": 30,
	}}})
	if err != nil || failedBatchRaw.(rushestools.ToolResult).Status != "failed" {
		t.Fatalf("failed batch=%#v err=%v", failedBatchRaw, err)
	}
	invalidBatchRaw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{
		{
			"kind": "insert_clip", "track_id": "sfx", "timeline_start_frame": 20,
			"asset_id": "sfx", "asset_kind": "audio", "source_start_frame": 0, "source_end_frame": 20,
		},
		{
			"kind": "trim_clip", "timeline_clip_id": "clip_v1_001",
			"source_start_frame": 0, "source_end_frame": 20,
		},
	}})
	if err != nil || invalidBatchRaw.(rushestools.ToolResult).Status != "failed" {
		t.Fatalf("invalid batch=%#v err=%v", invalidBatchRaw, err)
	}
	latestAfterInvalid, err := timeline.Latest(t.Context(), database, "draft_timeline_tools")
	if err != nil || latestAfterInvalid.Version != 3 {
		t.Fatalf("invalid batch must not persist: latest=%#v err=%v", latestAfterInvalid, err)
	}
	inspected, err := service.ExecuteTool(ctx, "timeline.inspect", rushestools.TimelineInspectInput{})
	inspectResult := inspected.(rushestools.ToolResult)
	tracks, tracksOK := inspectResult.Data["tracks"].([]map[string]any)
	if err != nil || inspectResult.Observation == "" || !tracksOK || len(tracks) != 7 {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	firstRenderRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	secondRenderRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	firstRender := firstRenderRaw.(rushestools.ToolResult)
	secondRender := secondRenderRaw.(rushestools.ToolResult)
	if firstRender.Status != "queued" || secondRender.Status != "queued" ||
		firstRender.Data["job_id"] != secondRender.Data["job_id"] {
		t.Fatalf("render idempotency first=%#v second=%#v", firstRender, secondRender)
	}
	var renderJobs, minRenderRetries, maxRenderRetries int
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*),MIN(max_retries),MAX(max_retries)
		 FROM jobs WHERE kind='render_preview' AND status='pending'`,
	).Scan(&renderJobs, &minRenderRetries, &maxRenderRetries); err != nil ||
		renderJobs != 1 || minRenderRetries != 2 || maxRenderRetries != 2 {
		t.Fatalf("render jobs=%d retries=%d..%d err=%v",
			renderJobs, minRenderRetries, maxRenderRetries, err)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE jobs SET status='failed' WHERE job_id=?", firstRender.Data["job_id"]); err != nil {
		t.Fatal(err)
	}
	retriedRenderRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	retriedRender := retriedRenderRaw.(rushestools.ToolResult)
	if retriedRender.Status != "queued" || retriedRender.Data["job_id"] == firstRender.Data["job_id"] {
		t.Fatalf("failed render retry=%#v", retriedRender)
	}
	reusedRetryRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if reusedRetryRaw.(rushestools.ToolResult).Data["job_id"] != retriedRender.Data["job_id"] {
		t.Fatalf("active retry not reused=%#v", reusedRetryRaw)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE jobs SET status='cancelled' WHERE job_id=?", retriedRender.Data["job_id"]); err != nil {
		t.Fatal(err)
	}
	secondRetryRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	secondRetry := secondRetryRaw.(rushestools.ToolResult)
	if secondRetry.Status != "queued" || secondRetry.Data["job_id"] == retriedRender.Data["job_id"] {
		t.Fatalf("cancelled render retry=%#v", secondRetry)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE jobs SET status='succeeded' WHERE job_id=?", secondRetry.Data["job_id"]); err != nil {
		t.Fatal(err)
	}
	completedRenderRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	completedRender := completedRenderRaw.(rushestools.ToolResult)
	if completedRender.Status != "succeeded" || completedRender.Data["job_id"] != secondRetry.Data["job_id"] {
		t.Fatalf("completed render idempotency=%#v", completedRender)
	}
	var retryJobs, minRetryBudget, maxRetryBudget int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*),MIN(max_retries),MAX(max_retries)
		FROM jobs WHERE kind='render_preview'`,
	).Scan(&retryJobs, &minRetryBudget, &maxRetryBudget); err != nil || retryJobs != 3 ||
		minRetryBudget != 2 || maxRetryBudget != 2 {
		t.Fatalf("render retry jobs=%d retries=%d..%d err=%v",
			retryJobs, minRetryBudget, maxRetryBudget, err)
	}
	var timelineRows, latestTimelineVersion int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*),MAX(version) FROM timeline_versions WHERE draft_id='draft_timeline_tools'`).Scan(&timelineRows, &latestTimelineVersion); err != nil || timelineRows != latestTimelineVersion {
		t.Fatalf("timeline rows=%d latest=%d err=%v", timelineRows, latestTimelineVersion, err)
	}
}

func TestFallbackMainlineDecisionReplayStatusAndPreviewInspection(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_full")
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
	content, err := service.fallbackFullMainline(ctx, "draft_full")
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
	validatedRaw, err := service.ExecuteTool(ctx, "timeline.validate", rushestools.TimelineValidateInput{})
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
	status, err := service.ExecuteTool(ctx, "render.status", rushestools.RenderStatusInput{})
	if err != nil || status.(rushestools.ToolResult).Data["running_jobs"] == nil {
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
		Question: "确认导出？", ToolName: "render.final_mp4", Arguments: map[string]any{},
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
		{Question: "空参数？", ToolName: "understand.materials", Arguments: nil},
		{Question: "缺少素材？", ToolName: "understand.materials", Arguments: map[string]any{}},
		{Question: "空画幅？", ToolName: "render.final_mp4", Arguments: map[string]any{"orientation": nil}},
		{Question: "片段参数缺失？", ToolName: "timeline.compose_initial", Arguments: map[string]any{"clips": []any{map[string]any{}}}},
		{Question: "空批量补丁？", ToolName: "timeline.apply_patches", Arguments: map[string]any{"ops": []any{nil}}},
		{Question: "空补丁？", ToolName: "timeline.apply_patch", Arguments: map[string]any{"op": map[string]any{}}},
		{Question: "缺少片段？", ToolName: "timeline.apply_patch", Arguments: map[string]any{"op": map[string]any{"kind": "delete_clip"}}},
		{Question: "未知补丁？", ToolName: "timeline.apply_patch", Arguments: map[string]any{"op": map[string]any{"kind": "unknown"}}},
		{Question: "补丁附加字段？", ToolName: "timeline.apply_patch", Arguments: map[string]any{"op": map[string]any{"kind": "delete_clip", "clip_id": "clip_1", "extra": true}}},
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
	if err != nil || replayed == "" {
		t.Fatalf("replayed=%q err=%v", replayed, err)
	}
	if cancelled, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
		"pending_tool_call": decision.PendingToolCall, "answer": map[string]any{"option_id": "cancel"},
	}}); err != nil || cancelled != "已取消这项操作。" {
		t.Fatalf("cancelled=%q err=%v", cancelled, err)
	}
	if _, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
		"pending_tool_call": map[string]any{"tool_name": "understand.materials", "arguments": nil},
		"answer":            map[string]any{"option_id": "confirm"},
	}}); err == nil {
		t.Fatal("nil confirmation arguments must fail replay validation")
	}
	for _, arguments := range []map[string]any{
		{"op": map[string]any{}},
		{"op": map[string]any{"kind": "delete_clip"}},
		{"op": map[string]any{"kind": "unknown"}},
		{"op": map[string]any{"kind": "delete_clip", "clip_id": "clip_1", "extra": true}},
	} {
		if _, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
			"pending_tool_call": map[string]any{"tool_name": "timeline.apply_patch", "arguments": arguments},
			"answer":            map[string]any{"option_id": "confirm"},
		}}); err == nil {
			t.Fatalf("invalid timeline patch must fail replay validation: %#v", arguments)
		}
	}
	beforeNullReplay, err := timeline.Latest(t.Context(), database, "draft_full")
	if err != nil {
		t.Fatal(err)
	}
	for name, arguments := range map[string]map[string]any{
		"render.final_mp4":       {"orientation": nil},
		"timeline.apply_patches": {"ops": []any{nil}},
	} {
		if _, err := service.replayPendingTool(ctx, QueueItem{DraftID: "draft_full", Payload: map[string]any{
			"pending_tool_call": map[string]any{"tool_name": name, "arguments": arguments},
			"answer":            map[string]any{"option_id": "confirm"},
		}}); err == nil {
			t.Fatalf("explicit null must fail replay validation: %s %#v", name, arguments)
		}
	}
	afterNullReplay, err := timeline.Latest(t.Context(), database, "draft_full")
	if err != nil {
		t.Fatal(err)
	}
	if afterNullReplay.Version != beforeNullReplay.Version {
		t.Fatalf("explicit null replay modified timeline: before=%d after=%d", beforeNullReplay.Version, afterNullReplay.Version)
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
	preview, err := service.ExecuteTool(ctx, "render.inspect_preview", rushestools.RenderInspectInput{
		PreviewID: "preview_inspect", Checks: []string{"decode"},
	})
	if err != nil || preview.(rushestools.PreviewInspectionResult).Summary == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
	}
	visualPreview, err := service.ExecuteTool(ctx, "render.inspect_preview", rushestools.RenderInspectInput{
		PreviewID: "preview_inspect", Checks: []string{"visual"},
	})
	visualResult := visualPreview.(rushestools.PreviewInspectionResult)
	if err != nil || !visualResult.Degraded || visualResult.VisualFrameCount == 0 ||
		len(visualResult.Issues) != 1 || visualResult.Issues[0]["check"] != "dependencies" {
		t.Fatalf("visual preview=%#v err=%v", visualResult, err)
	}
	if _, err := service.ExecuteTool(ctx, "render.inspect_preview", rushestools.RenderInspectInput{PreviewID: "missing"}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing preview err=%v", err)
	}
	if _, err := service.ExecuteTool(ctx, "asset.import_local_file", rushestools.AssetImportInput{}); err == nil {
		t.Fatal("harness-only import should reject direct execution")
	}
	if _, err := service.ExecuteTool(ctx, "unknown", struct{}{}); err == nil {
		t.Fatal("unknown tool should fail")
	}
	if _, err := service.ExecuteTool(t.Context(), "render.status", rushestools.RenderStatusInput{}); err == nil {
		t.Fatal("tool without draft should fail")
	}
}

func TestConfirmationChecksToolPreconditionsWhenCreatedAndReplayed(t *testing.T) {
	database := agentTestDatabase(t)
	const draftID = "draft_confirmation_preconditions"
	createAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	arguments := map[string]any{
		"op": map[string]any{"kind": "delete_clip", "timeline_clip_id": "clip_v1_001"},
	}

	missingRaw, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认删除片段？", ToolName: "timeline.apply_patch", Arguments: arguments,
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

	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "asset_confirmation", AssetKind: "video", SourceEndFrame: 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.persistTimeline(t.Context(), draftID, document, "confirmation_precondition_fixture"); err != nil {
		t.Fatal(err)
	}
	confirmRaw, err := service.ExecuteTool(ctx, "interaction.confirm_action", rushestools.ConfirmActionInput{
		Question: "确认删除片段？", ToolName: "timeline.apply_patch", Arguments: arguments,
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_empty")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_empty")
	if content, err := service.fallbackFullMainline(ctx, "draft_empty"); err != nil || content == "" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	if _, err := service.fallbackTurn(ctx, "draft_empty", "msg", "ASK_USER"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.fallbackTurn(ctx, "draft_empty", "msg", "导出"); err != nil {
		t.Fatal(err)
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
	for _, value := range []any{"yes", stringPointerValue("pointer"), (*string)(nil), 1} {
		_ = interfaceString(value)
	}
	replayed, err := service.tools.DecodeInput("timeline.apply_patch", map[string]any{
		"op": map[string]any{"kind": "delete_clip", "clip_id": "clip_replay"},
	})
	if err != nil {
		t.Fatal(err)
	}
	patchInput, ok := replayed.(rushestools.TimelinePatchInput)
	if !ok || reflect.TypeOf(patchInput.Op) != reflect.TypeFor[rushestools.TimelineOp]() {
		t.Fatalf("replayed timeline patch type=%T op=%T", replayed, patchInput.Op)
	}
	if patchInput.Op["kind"] != "delete_clip" || patchInput.Op["clip_id"] != "clip_replay" {
		t.Fatalf("replayed timeline op=%#v", patchInput.Op)
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
		_, _ = numericValue(value)
	}
}

func TestServiceAndToolFailureBranches(t *testing.T) {
	t.Parallel()
	if _, err := NewService(t.Context(), nil, nil); err == nil {
		t.Fatal("nil database should fail")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_failures")
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_failures")
	for name, input := range map[string]any{
		"understand.materials":        rushestools.UnderstandInput{},
		"audio.analyze_beats":         rushestools.AudioBeatAnalysisInput{},
		"audio.analyze_speech_pauses": rushestools.SpeechPauseAnalysisInput{},
		"timeline.apply_patch":        rushestools.TimelinePatchInput{},
		"timeline.apply_patches":      rushestools.TimelinePatchBatchInput{},
		"timeline.recut_to_beats":     rushestools.TimelineBeatRecutInput{},
		"timeline.validate":           rushestools.TimelineValidateInput{},
		"timeline.inspect":            rushestools.TimelineInspectInput{},
		"render.preview":              rushestools.RenderPreviewInput{},
		"render.final_mp4":            rushestools.RenderFinalInput{},
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
		if name == "timeline.recut_to_beats" {
			if err != nil || output.(rushestools.ToolResult).Status != "failed" {
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
	result, err := service.persistTimeline(ctx, "draft_failures", invalid, "invalid")
	if err != nil || result.Status != "validation_failed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := service.ExecuteTool(ctx, "timeline.validate", rushestools.TimelineValidateInput{}); err != nil {
		t.Fatal(err)
	}

	createAgentDraft(t, database, "draft_assets_filter")
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
	filtered, err := service.toolListAssets(ctx, "draft_assets_filter", rushestools.AssetListInput{
		Kind: "video", After: "a", Limit: 1, OnlyUsable: boolPointer(true),
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
	audio, err := service.toolListAssets(ctx, "draft_assets_filter", rushestools.AssetListInput{Kind: "audio"})
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
	timelineResult, err := service.persistTimeline(
		t.Context(), "draft_assets_filter", validObjectiveTimeline, "objective_valid",
	)
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
	timelineResult, err = service.persistTimeline(
		t.Context(), "draft_assets_filter", invalidObjectiveTimeline, "objective_invalid",
	)
	if err != nil || timelineResult.Status != "validation_failed" {
		t.Fatalf("invalid objective timeline=%#v err=%v", timelineResult, err)
	}
	objectiveContext, err = service.contextManager.builder.Build(t.Context(), "draft_assets_filter")
	if err != nil || !strings.Contains(objectiveContext, `"validated":false`) {
		t.Fatalf("unvalidated objective context=%q err=%v", objectiveContext, err)
	}
}

func TestAudioBeatPhaseNoteWarnsThatBeatEvidenceIsNotCreativeJudgment(t *testing.T) {
	t.Parallel()
	if !strings.Contains(audioBeatPhaseNote, "高潮") ||
		!strings.Contains(audioBeatPhaseNote, "不能自动等同") ||
		!strings.Contains(audioBeatPhaseNote, "好剪辑") {
		t.Fatalf("phase note missing creative-judgment warning: %q", audioBeatPhaseNote)
	}
	for _, fragment := range []string{
		"sample_frames", "samples 一一对应", "timeline_fps", "完整压缩波形", "WorldState", "24 点摘要",
	} {
		if !strings.Contains(audioWaveformUsageNote, fragment) {
			t.Fatalf("waveform usage note missing %q: %q", fragment, audioWaveformUsageNote)
		}
	}
	encodedWaveformResult, err := json.Marshal(rushestools.AudioBeatAnalysisResult{
		WaveformUsageNote: audioWaveformUsageNote,
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_beats")
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
		beats.WaveformUsageNote != audioWaveformUsageNote {
		t.Fatalf("beats=%#v", beats)
	}
}

func TestSpeechPauseAnalysisSupportsVideoAudioAndTimelineMapping(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_speech_pauses")
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
	if _, err := service.ExecuteTool(ctx, "timeline.compose_initial", rushestools.ComposeInitialInput{
		Clips: []rushestools.ComposeClip{{
			AssetID: "talk_video", SourceStartFrame: 0, SourceEndFrame: 90, Role: "a_roll",
		}},
	}); err != nil {
		t.Fatal(err)
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

func TestBeatRecutToolAcceptsModelChosenSFXFrameAndKeepsSeparateTrack(t *testing.T) {
	fakeBin := t.TempDir()
	fakeAubio := filepath.Join(fakeBin, "aubiotrack")
	if err := os.WriteFile(fakeAubio, []byte("#!/bin/sh\nprintf '1.000000\\n1.333333\\n1.666667\\n2.000000\\n2.333333\\n2.666667\\n3.000000\\n3.333333\\n3.666667\\n4.000000\\n4.333333\\n4.666667\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_beat_recut")
	source := filepath.Join(database.Paths.Temporary, "fake-audio.wav")
	if err := os.WriteFile(source, []byte("fake audio source"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "bgm_audio", "job_id": "job_bgm", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "bgm.wav",
			"hash": "bgm_hash", "size": 17, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 4.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_beat_recut", Payload: map[string]any{"asset_id": "bgm_audio", "linked_at": now}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "sfx_audio", "job_id": "job_sfx", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "fire.wav",
			"hash": "sfx_hash", "size": 17, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 1.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_beat_recut", Payload: map[string]any{"asset_id": "sfx_audio", "linked_at": now}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}

	document, err := timeline.ComposeInitial("draft_beat_recut", 1, []timeline.Selection{
		{AssetID: "video_1", AssetKind: "video", SourceEndFrame: 50},
		{AssetID: "video_2", AssetKind: "video", SourceEndFrame: 50},
		{AssetID: "video_3", AssetKind: "video", SourceEndFrame: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[4].Clips = []timeline.Clip{{
		TimelineClipID: "bgm", TrackID: "bgm", AssetID: "bgm_audio", AssetKind: "audio", Role: "bgm",
		TimelineStartFrame: 0, TimelineEndFrame: 120, SourceEndFrame: 120, PlaybackRate: 1,
	}}
	document.Tracks[6].Clips = []timeline.Clip{{
		TimelineClipID: "old_sfx", TrackID: "sfx", AssetID: "sfx_audio", AssetKind: "audio", Role: "sfx",
		TimelineStartFrame: 120, TimelineEndFrame: 150, SourceEndFrame: 30, PlaybackRate: 1,
	}}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if persisted, err := service.persistTimeline(t.Context(), "draft_beat_recut", document, "fixture"); err != nil || persisted.Status != "succeeded" {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	ctx := rushestools.WithDraftID(t.Context(), "draft_beat_recut")
	expectFailed := func(name string, input rushestools.TimelineBeatRecutInput) {
		t.Helper()
		output, callErr := service.ExecuteTool(ctx, "timeline.recut_to_beats", input)
		if callErr != nil || output.(rushestools.ToolResult).Status != "failed" {
			t.Fatalf("%s output=%#v err=%v", name, output, callErr)
		}
	}
	expectFailed("missing bgm", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "missing", CutFrames: []int{30, 70, 110},
	})
	expectFailed("wrong cut count", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30},
	})
	expectFailed("non increasing cuts", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30, 30, 110},
	})
	expectFailed("non beat cut", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{31, 70, 110},
	})
	expectFailed("clip too short", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{70, 110, 150},
	})
	expectFailed("missing sfx start", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30, 70, 110},
		SFX: &rushestools.BeatRecutSFXInput{AssetID: "sfx_audio", DurationFrames: 10},
	})
	nonBeatStart, validStart := 40, 70
	expectFailed("sfx outside timeline", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30, 70, 110},
		SFX: &rushestools.BeatRecutSFXInput{AssetID: "sfx_audio", StartFrame: &validStart, DurationFrames: 50},
	})
	expectFailed("missing sfx asset", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30, 70, 110},
		SFX: &rushestools.BeatRecutSFXInput{AssetID: "missing", StartFrame: &validStart, DurationFrames: 10},
	})
	loudGain := 13.0
	expectFailed("invalid sfx gain", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm", CutFrames: []int{30, 70, 110},
		SFX: &rushestools.BeatRecutSFXInput{AssetID: "sfx_audio", StartFrame: &validStart, DurationFrames: 10, GainDB: &loudGain},
	})
	output, err := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: "bgm",
		CutFrames:         []int{30, 70, 110},
		SFX: &rushestools.BeatRecutSFXInput{
			AssetID: "sfx_audio", StartFrame: &nonBeatStart, DurationFrames: 10,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := output.(rushestools.ToolResult)
	if toolResult.Status != "succeeded" || toolResult.Data["duration_frames"] != 110 {
		t.Fatalf("result=%#v", toolResult)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_beat_recut")
	if err != nil || latest.Version != 2 || latest.DurationFrames != 110 || !timeline.Validate(latest).Valid {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	if got := []int{
		latest.Tracks[0].Clips[0].TimelineEndFrame,
		latest.Tracks[0].Clips[1].TimelineEndFrame,
		latest.Tracks[0].Clips[2].TimelineEndFrame,
	}; !reflect.DeepEqual(got, []int{30, 70, 110}) {
		t.Fatalf("cuts=%v", got)
	}
	if bgm := latest.Tracks[4].Clips[0]; bgm.TimelineStartFrame != 0 || bgm.TimelineEndFrame != 110 {
		t.Fatalf("bgm=%#v", bgm)
	}
	if len(latest.Tracks[6].Clips) != 1 || latest.Tracks[6].Clips[0].TimelineStartFrame != 40 ||
		latest.Tracks[6].Clips[0].TimelineEndFrame != 50 || latest.Tracks[6].Clips[0].GainDB != -12 {
		t.Fatalf("sfx=%#v", latest.Tracks[6].Clips)
	}
	if warnings := audioLayoutData(latest)["warnings"].([]string); len(warnings) != 0 {
		t.Fatalf("warnings=%v", warnings)
	}
}

func TestBeatRecutToolRebuildsFullLengthMixFromSourceAssets(t *testing.T) {
	fakeBin := t.TempDir()
	fakeAubio := filepath.Join(fakeBin, "aubiotrack")
	if err := os.WriteFile(fakeAubio, []byte("#!/bin/sh\nprintf '1.000000\\n1.333333\\n1.666667\\n2.000000\\n2.333333\\n2.666667\\n3.000000\\n3.333333\\n3.666667\\n4.000000\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeOnset := filepath.Join(fakeBin, "aubioonset")
	if err := os.WriteFile(fakeOnset, []byte("#!/bin/sh\nprintf '1.000000\\n2.333333\\n3.666667\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_full_beat_mix")
	source := filepath.Join(database.Paths.Temporary, "beat-mix-source.wav")
	if err := os.WriteFile(source, []byte("fake source"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	events := []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "mix_bgm", "job_id": "job_mix_bgm", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "background-music.wav",
			"hash": "mix_bgm_hash", "size": 11, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 4.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_full_beat_mix", Payload: map[string]any{"asset_id": "mix_bgm", "linked_at": now}},
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "mix_sfx", "job_id": "job_mix_sfx", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "fire-sfx.wav",
			"hash": "mix_sfx_hash", "size": 11, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 1.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_full_beat_mix", Payload: map[string]any{"asset_id": "mix_sfx", "linked_at": now}},
	}
	for index := 1; index <= 4; index++ {
		assetID := fmt.Sprintf("mix_video_%d", index)
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_" + assetID, "storage_mode": "reference",
				"reference_path": source, "kind": "video", "source": "local_path",
				"filename": assetID + ".mov", "hash": assetID + "_hash", "size": 11,
				"ingest_status": "ready", "usable": true,
				"probe": map[string]any{"duration_sec": 10.0, "has_audio": true},
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_full_beat_mix", Payload: map[string]any{
				"asset_id": assetID, "linked_at": now,
			}},
		)
	}
	for _, fixture := range []struct {
		id          string
		kind        string
		filename    string
		durationSec float64
	}{
		{id: "mix_short", kind: "video", filename: "short.mov", durationSec: 0.5},
		{id: "mix_zero_video", kind: "video", filename: "zero.mov", durationSec: 0},
	} {
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": fixture.id, "job_id": "job_" + fixture.id, "storage_mode": "reference",
				"reference_path": source, "kind": fixture.kind, "source": "local_path",
				"filename": fixture.filename, "hash": fixture.id + "_hash", "size": 11,
				"ingest_status": "ready", "usable": true,
				"probe": map[string]any{"duration_sec": fixture.durationSec, "has_audio": fixture.kind == "audio"},
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_full_beat_mix", Payload: map[string]any{
				"asset_id": fixture.id, "linked_at": now,
			}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	understandingResult, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{MaterialSummaries: []reducer.MaterialSummaryRow{{
			ID: "summary_mix_video_1", AssetID: "mix_video_1", Status: "ready",
			Summary: map[string]any{
				"asset_id": "mix_video_1", "version": 2,
				"segments": []map[string]any{{
					"source_start_frame": 120, "source_end_frame": 220,
					"quality": "usable", "description": "角色抬手并转身，适合作为动作切点。",
				}},
			},
		}}},
	})
	if err != nil || understandingResult.Status != reducer.StatusApplied {
		t.Fatalf("understanding status=%s err=%v", understandingResult.Status, err)
	}

	// 真实故障的起点：草稿已有素材但还没有时间线。高层卡点工具必须直接
	// 原子建片，不能被迫先走 compose_initial 再手工补 BGM/SFX。
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	ctx := rushestools.WithDraftID(t.Context(), "draft_full_beat_mix")
	nonBeatSFXStart := 40
	output, err := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", CoverEntireBGM: true, UseAllVideoAssets: true,
		VideoAssetIDs: []string{"mix_video_1", "mix_video_2", "mix_video_3", "mix_video_4"},
		SFX: &rushestools.BeatRecutSFXInput{
			AssetID: "mix_sfx", StartFrame: &nonBeatSFXStart, DurationFrames: 10,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := output.(rushestools.ToolResult)
	if toolResult.Status != "succeeded" || toolResult.Data["duration_frames"] != 120 ||
		toolResult.Data["sfx_start_frame"] != 40 ||
		toolResult.Data["used_all_video_assets"] != true {
		t.Fatalf("result=%#v", toolResult)
	}
	expectBeatMixFailed := func(name string, input rushestools.TimelineBeatRecutInput, want string) {
		t.Helper()
		failedOutput, callErr := service.ExecuteTool(ctx, "timeline.recut_to_beats", input)
		if callErr != nil {
			t.Fatalf("%s err=%v", name, callErr)
		}
		failedResult := failedOutput.(rushestools.ToolResult)
		if failedResult.Status != "failed" || !strings.Contains(failedResult.Observation, want) {
			t.Fatalf("%s result=%#v", name, failedResult)
		}
	}
	expectBeatMixFailed("unknown bgm", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "missing", TargetDurationFrames: 120,
	}, "bgm_asset_id")
	expectBeatMixFailed("negative target", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: -1,
	}, "必须为正数")
	expectBeatMixFailed("target beyond bgm", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 121,
	}, "超过 BGM")
	expectBeatMixFailed("infer unique bgm", rushestools.TimelineBeatRecutInput{
		CoverEntireBGM: true, VideoAssetIDs: []string{"missing"},
	}, "video_asset_ids")
	bgmClipID, _ := toolResult.Data["bgm_timeline_clip_id"].(string)
	expectBeatMixFailed("resolve bgm from current clip", rushestools.TimelineBeatRecutInput{
		BGMTimelineClipID: bgmClipID, TargetDurationFrames: 120,
		VideoAssetIDs: []string{"missing"},
	}, "video_asset_ids")
	expectBeatMixFailed("reject non video", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_sfx"},
	}, "video_asset_ids")
	expectBeatMixFailed("zero duration video", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_zero_video"},
	}, "没有可用于")
	expectBeatMixFailed("video too short", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_short"},
	}, "没有足够长")
	expectBeatMixFailed("missing sfx", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_video_1"},
		SFX:           &rushestools.BeatRecutSFXInput{AssetID: "missing", DurationFrames: 10},
	}, "SFX 素材")
	expectBeatMixFailed("sfx duration", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_video_1"},
		SFX:           &rushestools.BeatRecutSFXInput{AssetID: "mix_sfx", DurationFrames: 0},
	}, "duration_frames")
	expectBeatMixFailed("sfx source too short", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_video_1"},
		SFX:           &rushestools.BeatRecutSFXInput{AssetID: "mix_sfx", DurationFrames: 31},
	}, "超过素材时长")
	expectBeatMixFailed("missing sfx start", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 20,
		VideoAssetIDs: []string{"mix_video_1"},
		SFX:           &rushestools.BeatRecutSFXInput{AssetID: "mix_sfx", DurationFrames: 10},
	}, "start_frame 必须显式提供")
	validStart, loudGain := 70, 13.0
	expectBeatMixFailed("invalid sfx gain", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120,
		VideoAssetIDs: []string{"mix_video_1"},
		SFX: &rushestools.BeatRecutSFXInput{
			AssetID: "mix_sfx", StartFrame: &validStart, DurationFrames: 10, GainDB: &loudGain,
		},
	}, "gain_db")
	expectBeatMixFailed("deduplicate video ids", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 20,
		VideoAssetIDs: []string{"mix_video_1", "mix_video_1"},
		SFX:           &rushestools.BeatRecutSFXInput{AssetID: "missing", DurationFrames: 10},
	}, "SFX 素材")
	expectBeatMixFailed("use all requires one cut per video", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120, UseAllVideoAssets: true,
		VideoAssetIDs: []string{"mix_video_1", "mix_video_2", "mix_video_3"},
		CutFrames:     []int{30, 120},
	}, "cut_frames 数量")

	additional, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "mix_zero_bgm", "job_id": "job_mix_zero_bgm", "storage_mode": "reference",
			"reference_path": source, "kind": "audio", "source": "local_path", "filename": "second-music.wav",
			"hash": "mix_zero_bgm_hash", "size": 11, "ingest_status": "ready", "usable": true,
			"probe": map[string]any{"duration_sec": 0.0, "has_audio": true},
		}},
		{Type: "AssetLinked", DraftID: "draft_full_beat_mix", Payload: map[string]any{
			"asset_id": "mix_zero_bgm", "linked_at": now,
		}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || additional.Status != reducer.StatusApplied {
		t.Fatalf("additional assets status=%s err=%v", additional.Status, err)
	}
	expectBeatMixFailed("ambiguous inferred bgm", rushestools.TimelineBeatRecutInput{
		CoverEntireBGM: true,
	}, "无法唯一确定 BGM")
	expectBeatMixFailed("bgm without duration", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_zero_bgm", TargetDurationFrames: 20,
	}, "缺少可用时长")

	latest, err := timeline.Latest(t.Context(), database, "draft_full_beat_mix")
	if err != nil || latest.DurationFrames != 120 || !timeline.Validate(latest).Valid {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	if got := []int{
		latest.Tracks[0].Clips[0].TimelineEndFrame,
		latest.Tracks[0].Clips[1].TimelineEndFrame,
		latest.Tracks[0].Clips[2].TimelineEndFrame,
		latest.Tracks[0].Clips[3].TimelineEndFrame,
	}; !reflect.DeepEqual(got, []int{30, 70, 110, 120}) {
		t.Fatalf("cuts=%v", got)
	}
	if len(latest.Tracks[2].Clips) != 0 {
		t.Fatalf("beat mix should keep source audio out of the default mix: %#v", latest.Tracks[2].Clips)
	}
	if latest.Tracks[0].Clips[0].SourceStartFrame != 120 ||
		toolResult.Data["understanding_source_ranges_used"] != 1 {
		t.Fatalf("understanding-aware source selection latest=%#v result=%#v", latest.Tracks[0].Clips[0], toolResult)
	}
	if len(latest.Tracks[4].Clips) != 1 || latest.Tracks[4].Clips[0].TimelineEndFrame != 120 {
		t.Fatalf("bgm=%#v", latest.Tracks[4].Clips)
	} else if effects := latest.Tracks[4].Clips[0].Effects; len(effects) != 1 || effects[0]["kind"] != "beat_grid" {
		t.Fatalf("bgm beat metadata=%#v", effects)
	}
	if len(latest.Tracks[6].Clips) != 1 || latest.Tracks[6].Clips[0].TimelineStartFrame != 40 ||
		latest.Tracks[6].Clips[0].TimelineEndFrame != 50 || latest.Tracks[6].Clips[0].GainDB != -12 {
		t.Fatalf("sfx=%#v", latest.Tracks[6].Clips)
	}

	// 回归真实故障：模型同时传 bgm_asset_id、目标时长和显式 cut_frames 时，
	// 必须走完整源素材重建，不能误入只会裁短当前 clip 的旧路径。
	explicitOutput, explicitErr := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120, CoverEntireBGM: true,
		UseAllVideoAssets: true,
		VideoAssetIDs:     []string{"mix_video_1", "mix_video_2", "mix_video_3"},
		CutFrames:         []int{30, 70, 120},
	})
	if explicitErr != nil {
		t.Fatal(explicitErr)
	}
	explicitResult := explicitOutput.(rushestools.ToolResult)
	if explicitResult.Status != "succeeded" || !reflect.DeepEqual(explicitResult.Data["cut_frames"], []int{30, 70, 120}) {
		t.Fatalf("explicit cut rebuild=%#v", explicitResult)
	}
	explicitLatest, latestErr := timeline.Latest(t.Context(), database, "draft_full_beat_mix")
	if latestErr != nil || len(explicitLatest.Tracks[0].Clips) != 3 ||
		explicitLatest.Tracks[0].Clips[2].TimelineEndFrame != 120 {
		t.Fatalf("explicit latest=%#v err=%v", explicitLatest, latestErr)
	}

	// 切点可以多于素材数；每个素材至少出现一次，额外片段必须从同一
	// 素材的其他不重叠源区间取得。
	multiOutput, multiErr := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 120, UseAllVideoAssets: true,
		VideoAssetIDs: []string{"mix_video_1", "mix_video_2", "mix_video_3", "mix_video_4"},
		CutFrames:     []int{30, 50, 70, 90, 120},
	})
	if multiErr != nil {
		t.Fatal(multiErr)
	}
	multiResult := multiOutput.(rushestools.ToolResult)
	if multiResult.Status != "succeeded" || multiResult.Data["used_all_video_assets"] != true {
		t.Fatalf("multi-source recut=%#v", multiResult)
	}
	multiLatest, multiLatestErr := timeline.Latest(t.Context(), database, "draft_full_beat_mix")
	if multiLatestErr != nil || len(multiLatest.Tracks[0].Clips) != 5 {
		t.Fatalf("multi latest=%#v err=%v", multiLatest, multiLatestErr)
	}
	usedByAsset := map[string][]beatMixSourceRange{}
	for _, clip := range multiLatest.Tracks[0].Clips {
		candidate := beatMixSourceRange{StartFrame: clip.SourceStartFrame, EndFrame: clip.SourceEndFrame}
		if overlapsAny(candidate, usedByAsset[clip.AssetID]) {
			t.Fatalf("同一素材源区间重叠: asset=%s ranges=%#v candidate=%#v", clip.AssetID, usedByAsset[clip.AssetID], candidate)
		}
		usedByAsset[clip.AssetID] = append(usedByAsset[clip.AssetID], candidate)
	}
	if len(usedByAsset) != 4 {
		t.Fatalf("use_all 未覆盖全部素材: %#v", usedByAsset)
	}

	searchOutput, searchErr := service.ExecuteTool(ctx, "media.search_shots", rushestools.ShotSearchInput{
		Query: "角色转身 动作切点", AssetIDs: []string{"mix_video_1"}, MinDurationFrames: 30,
	})
	if searchErr != nil {
		t.Fatal(searchErr)
	}
	searchResult := searchOutput.(rushestools.ShotSearchResult)
	if len(searchResult.Shots) != 1 || searchResult.Shots[0].SourceStartFrame != 120 {
		t.Fatalf("shot search=%#v", searchResult)
	}
	shotOutput, shotErr := service.ExecuteTool(ctx, "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{
		BGMAssetID: "mix_bgm", TargetDurationFrames: 30,
		VideoAssetIDs: []string{"mix_video_1"}, CutFrames: []int{30},
		ShotIDs: []string{searchResult.Shots[0].ShotID},
	})
	if shotErr != nil || shotOutput.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("shot recut=%#v err=%v", shotOutput, shotErr)
	}
	shotLatest, shotLatestErr := timeline.Latest(t.Context(), database, "draft_full_beat_mix")
	if shotLatestErr != nil || shotLatest.Tracks[0].Clips[0].SourceStartFrame != 120 {
		t.Fatalf("shot latest=%#v err=%v", shotLatest, shotLatestErr)
	}
}

func TestBeatMixCutHelpersCoverFallbackAndBounds(t *testing.T) {
	if cuts := chooseBeatMixCuts(nil, nil, 0, 3); cuts != nil {
		t.Fatalf("invalid cuts=%v", cuts)
	}
	if cuts := chooseBeatMixCuts(nil, []int{0, 20, 20, 100}, 20, 1); !reflect.DeepEqual(cuts, []int{20}) {
		t.Fatalf("single cut=%v", cuts)
	}
	cuts := chooseBeatMixCuts([]int{0, 10, 20, 30, 40, 50, 60, 100}, nil, 100, 3)
	if len(cuts) != 3 || cuts[2] != 100 || cuts[0] >= cuts[1] {
		t.Fatalf("distributed cuts=%v", cuts)
	}
	fallbackCuts := chooseAllBeatMixCuts([]int{10}, []int{10, 20, 30, 40, 50}, 60, 4)
	if len(fallbackCuts) != 4 || fallbackCuts[3] != 60 {
		t.Fatalf("full beat fallback cuts=%v", fallbackCuts)
	}
	// 真实素材回归：三段视频总长足够覆盖 900 帧，但旧规划在真实
	// 四拍网格上会选出 [306,570,900]，最后一段 330 帧超过第三条
	// 素材的 303 帧容量。自动规划必须在真实拍点上移动前两刀。
	capacityBeatFrames := []int{
		89, 100, 110, 121, 132, 143, 154, 173, 183, 194, 204, 214, 224, 235,
		245, 255, 265, 276, 286, 296, 306, 317, 327, 340, 353, 366, 379, 392,
		405, 421, 434, 447, 460, 473, 486, 500, 513, 528, 542, 556, 570, 585,
		600, 615, 630, 645, 660, 675, 690, 705, 720, 735, 750, 765, 780, 795,
		810, 825, 840, 855, 870, 884,
	}
	capacityFourBeatFrames := []int{89, 132, 183, 224, 265, 306, 353, 405, 460, 513, 570, 630, 690, 750, 810, 870}
	capacities := []int{325, 327, 303}
	legacyCuts := chooseAllBeatMixCuts(capacityFourBeatFrames, capacityBeatFrames, 900, len(capacities))
	if !reflect.DeepEqual(legacyCuts, []int{306, 570, 900}) {
		t.Fatalf("legacy cuts=%v", legacyCuts)
	}
	if legacyCuts[2]-legacyCuts[1] <= capacities[2] {
		t.Fatalf("legacy planner unexpectedly fits capacities: cuts=%v capacities=%v", legacyCuts, capacities)
	}
	capacityCuts := chooseCapacityAwareBeatMixCuts(capacityFourBeatFrames, capacityBeatFrames, 900, capacities)
	if !reflect.DeepEqual(capacityCuts, []int{306, 630, 900}) {
		t.Fatalf("capacity-aware cuts=%v", capacityCuts)
	}
	if repeated := chooseCapacityAwareBeatMixCuts(capacityFourBeatFrames, capacityBeatFrames, 900, capacities); !reflect.DeepEqual(repeated, capacityCuts) {
		t.Fatalf("capacity-aware planner is not deterministic: first=%v repeated=%v", capacityCuts, repeated)
	}
	if len(capacityCuts) != 3 || capacityCuts[2] != 900 {
		t.Fatalf("capacity-aware cuts=%v", capacityCuts)
	}
	previous := 0
	for index, cut := range capacityCuts {
		if duration := cut - previous; duration <= 0 || duration > capacities[index] {
			t.Fatalf("capacity-aware segment=%d duration=%d cuts=%v", index, duration, capacityCuts)
		}
		if cut != 900 && !containsFrame(capacityBeatFrames, cut) {
			t.Fatalf("capacity-aware cut not on beat: %v", capacityCuts)
		}
		previous = cut
	}
	if fallback := chooseCapacityAwareBeatMixCuts([]int{296, 585}, capacityBeatFrames, 900, capacities); !reflect.DeepEqual(fallback, []int{296, 600, 900}) {
		t.Fatalf("capacity-aware full-beat fallback=%v", fallback)
	}
	if cuts, ok := distributeCapacityAwareBeatMixCuts([]int{30, 60}, 100, []int{30, 30, 30}); ok || cuts != nil {
		t.Fatalf("insufficient capacity cuts=%v ok=%v", cuts, ok)
	}
	if cuts := chooseCapacityAwareBeatMixCuts(nil, nil, 0, nil); cuts != nil {
		t.Fatalf("invalid capacity-aware cuts=%v", cuts)
	}
	if cuts, ok := distributeCapacityAwareBeatMixCuts(nil, 30, []int{30}); !ok || !reflect.DeepEqual(cuts, []int{30}) {
		t.Fatalf("single capacity cuts=%v ok=%v", cuts, ok)
	}
	if cuts, ok := distributeCapacityAwareBeatMixCuts(nil, 31, []int{30}); ok || cuts != nil {
		t.Fatalf("single short capacity cuts=%v ok=%v", cuts, ok)
	}
	if cuts, ok := distributeCapacityAwareBeatMixCuts([]int{10}, 20, []int{0, 20}); ok || cuts != nil {
		t.Fatalf("zero capacity cuts=%v ok=%v", cuts, ok)
	}
	if cuts, ok := distributeCapacityAwareBeatMixCuts([]int{10}, 20, []int{10, 10, 10}); ok || cuts != nil {
		t.Fatalf("insufficient beat candidates cuts=%v ok=%v", cuts, ok)
	}
	if candidates := beatCandidatesWithin([]int{-1, 0, 10, 10, 20, 30}, 30); !reflect.DeepEqual(candidates, []int{10, 20}) {
		t.Fatalf("candidates=%v", candidates)
	}
	if !containsFrame([]int{10, 20, 30}, 20) || containsFrame([]int{10, 20, 30}, 25) {
		t.Fatal("containsFrame bounds failed")
	}
	if absInt(-4) != 4 || absInt(4) != 4 {
		t.Fatal("absInt failed")
	}
	ranges := []beatMixSourceRange{{StartFrame: 120, EndFrame: 220}}
	usedRanges := []beatMixSourceRange{{StartFrame: 120, EndFrame: 150}}
	if start, ok := chooseUnusedBeatMixSourceStart(300, 30, ranges, usedRanges, -1, true); !ok || start != 150 {
		t.Fatalf("unused semantic gap start=%d ok=%v", start, ok)
	}
	if _, ok := chooseUnusedBeatMixSourceStart(300, 120, ranges, nil, 0, true); ok {
		t.Fatal("strict short semantic range should not fit")
	}
	if start, ok := chooseUnusedBeatMixSourceStart(300, 120, ranges, nil, 0, false); !ok || start != 0 {
		t.Fatalf("full-source fallback start=%d ok=%v", start, ok)
	}
	if _, ok := chooseUnusedBeatMixSourceStart(20, 30, nil, nil, 0, false); ok {
		t.Fatal("short full source should not fit")
	}
	coalesced := beatMixRangesFromUnderstanding([]understanding.Segment{
		{SourceStartFrame: 0, SourceEndFrame: 100, Quality: "usable", BoundaryKind: "video_start"},
		{SourceStartFrame: 100, SourceEndFrame: 200, Quality: "usable", BoundaryKind: "analysis_window"},
		{SourceStartFrame: 200, SourceEndFrame: 300, Quality: "usable", BoundaryKind: "analysis_window"},
		{SourceStartFrame: 300, SourceEndFrame: 400, Quality: "usable", BoundaryKind: "visual_cut"},
	}, 400)
	if !sourceRangeContains(coalesced, 0, 250) || sourceRangeContains(coalesced, 200, 350) {
		t.Fatalf("analysis-window coalescing=%#v", coalesced)
	}
	invalidRanges := beatMixRangesFromUnderstanding([]understanding.Segment{
		{SourceStartFrame: 0, SourceEndFrame: 50, Quality: "unusable", BoundaryKind: "video_start"},
		{SourceStartFrame: 80, SourceEndFrame: 80, Quality: "usable", BoundaryKind: "analysis_window"},
	}, 100)
	if len(invalidRanges) != 0 {
		t.Fatalf("invalid ranges=%#v", invalidRanges)
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
	layout := audioLayoutData(document)
	warnings := layout["warnings"].([]string)
	without := layout["sfx_without_bgm"].([]string)
	if len(warnings) != 2 || len(without) != 1 || without[0] != "sfx_late" {
		t.Fatalf("layout=%#v", layout)
	}
}

func TestModelFailureStillRepliesAndEndsTurn(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_model_error")
	insertAgentMessage(t, database, "draft_model_error", "user_error", "fail")
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
			case "turn_ended":
				if event["outcome"] != "failed" || !strings.Contains(completed, "本轮没有完成") {
					t.Fatalf("completed=%q event=%#v", completed, event)
				}
				messages, listErr := storage.ListMessages(t.Context(), database.Read(), "draft_model_error", 20)
				if listErr != nil || len(messages) < 2 || messages[len(messages)-1].Role != "assistant" ||
					messages[len(messages)-1].Content != completed {
					t.Fatalf("messages=%#v err=%v", messages, listErr)
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_empty_model")
	insertAgentMessage(t, database, "draft_empty_model", "user_empty_model", "不要静默")
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
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_bounded_recovery")
	insertAgentMessage(t, database, "draft_bounded_recovery", "user_bounded_recovery", "不要循环")
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
			case "turn_ended":
				if event["outcome"] != "failed" || completed != "本轮工具修复未完成，请告诉我下一步怎么处理。" {
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
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("恢复预算耗尽后没有可见终态")
		}
	}
}

func TestRepeatedFailedToolLoopStillRepliesAndEndsTurn(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_tool_loop")
	insertAgentMessage(t, database, "draft_tool_loop", "user_tool_loop", "loop")
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
	database := agentTestDatabase(t)
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
	if stringPointerValue("") != nil {
		t.Fatal("空字符串不应生成指针")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_closed")
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
		"understand.materials":        rushestools.UnderstandInput{AssetIDs: []string{"asset"}},
		"media.search_shots":          rushestools.ShotSearchInput{},
		"audio.analyze_beats":         rushestools.AudioBeatAnalysisInput{AssetID: "asset"},
		"audio.analyze_speech_pauses": rushestools.SpeechPauseAnalysisInput{AssetID: "asset"},
		"interaction.ask_user":        rushestools.AskUserInput{Question: "?", DecisionType: "critical"},
		"decision.answer":             rushestools.DecisionAnswerInput{DecisionID: "decision"},
		"plan.update":                 rushestools.PlanUpdateInput{Plan: map[string]any{"status": "closed-db"}},
		"timeline.compose_initial":    rushestools.ComposeInitialInput{},
		"timeline.apply_patch":        rushestools.TimelinePatchInput{Op: map[string]any{"kind": "noop"}},
		"timeline.apply_patches":      rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{"kind": "noop"}}},
		"timeline.recut_to_beats":     rushestools.TimelineBeatRecutInput{CutFrames: []int{30}, BGMTimelineClipID: "bgm"},
		"timeline.validate":           rushestools.TimelineValidateInput{},
		"timeline.inspect":            rushestools.TimelineInspectInput{},
		"render.preview":              rushestools.RenderPreviewInput{},
		"render.final_mp4":            rushestools.RenderFinalInput{},
		"render.status":               rushestools.RenderStatusInput{},
		"render.inspect_preview":      rushestools.RenderInspectInput{PreviewID: "preview"},
	} {
		if _, err := service.ExecuteTool(ctx, name, input); err == nil {
			t.Fatalf("closed database: %s 应失败", name)
		}
	}
	if _, _, err := service.findRenderJob(t.Context(), "render_preview", "closed", false); err == nil {
		t.Fatal("closed findRenderJob 应失败")
	}
	if _, err := service.modelMessages(ctx, "draft_closed"); err == nil {
		t.Fatal("closed modelMessages 应失败")
	}
	if _, err := service.fallbackFullMainline(ctx, "draft_closed"); err == nil {
		t.Fatal("closed fallback mainline 应失败")
	}
	if _, err := service.persistTimeline(ctx, "draft_closed", timeline.Empty("draft_closed", 1), "closed"); err == nil {
		t.Fatal("closed persist timeline 应失败")
	}
	if err := service.runTurn(t.Context(), QueueItem{
		DraftID: "draft_closed", Kind: QueueUserMessage,
		Payload: map[string]any{"content": "ordinary"},
	}); err == nil {
		t.Fatal("assistant message 持久化到关闭数据库应失败")
	}
	if cursor := service.bridgeIteration(t.Context(), 9); cursor != 9 {
		t.Fatalf("closed bridge cursor=%d", cursor)
	}
	reporter := service.toolReporter(t.Context(), "draft_closed")
	reporter("orphan", "finished", nil, nil, errors.New("tool failed"))
}

func agentTestDatabase(t *testing.T) *storage.DB {
	t.Helper()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func createAgentDraft(t *testing.T, database *storage.DB, draftID string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "DraftCreated", DraftID: draftID, Payload: map[string]any{"name": draftID},
	}}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("create status=%s err=%v", result.Status, err)
	}
}

func insertAgentMessage(t *testing.T, database *storage.DB, draftID, messageID, content string) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser, ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: "user", Kind: "user", Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("message status=%s err=%v", result.Status, err)
	}
}
