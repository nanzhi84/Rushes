package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const (
	// Eino 的 ReAct 图会把一次模型节点和一次工具节点分别计为一个 step。
	// 单个工具节点会执行该 assistant 消息中的全部 tool_calls，因此这里限制
	// 的是模型与工具的往返轮数。预留最后一次模型节点生成终态回复。
	maxToolRoundsPerTurn               = 40
	contextCompactionSummaryRuneLimit  = 4000
	contextCompactionFallbackRuneLimit = 3000
)

var maxReActStepsPerTurn = reactStepsForToolRounds(maxToolRoundsPerTurn)

// reactStepsForToolRounds 把“模型→工具”往返预算换成 Eino 图节点预算，并预留最后
// 一个模型节点生成终态回复。图结构若改变，守卫测试会要求同步修改这条换算。
func reactStepsForToolRounds(toolRounds int) int {
	return max(0, toolRounds)*2 + 1
}

type Service struct {
	database         *storage.DB
	hub              *TurnStreamHub
	queue            *TurnQueue
	tools            *rushestools.Registry
	executor         *agentexec.Executor
	chatModel        model.ToolCallingChatModel
	react            *concurrentReactAgent
	analyzer         *understanding.Analyzer
	speechRecognizer contracts.SpeechRecognizer
	contextManager   *ContextManager
	indexedResources *agentexec.IndexedResourceCoordinator
	fallbackScaffold fallbackScaffold
	ctx              context.Context
	cancel           context.CancelFunc
}

func NewService(
	parent context.Context,
	database *storage.DB,
	chatModel model.ToolCallingChatModel,
) (*Service, error) {
	return NewServiceWithModels(parent, database, chatModel, nil)
}

func NewServiceWithModels(
	parent context.Context,
	database *storage.DB,
	chatModel model.ToolCallingChatModel,
	visionModel model.ToolCallingChatModel,
) (*Service, error) {
	return newServiceWithModels(parent, database, chatModel, visionModel)
}

// NewServiceWithModelsForStartup 与普通构造共享同一行为；后台 job 不再创建 synthetic
// Agent turn，启动对账只补驱真实 user/decision 回合。
func NewServiceWithModelsForStartup(
	parent context.Context,
	database *storage.DB,
	chatModel model.ToolCallingChatModel,
	visionModel model.ToolCallingChatModel,
) (*Service, error) {
	return newServiceWithModels(parent, database, chatModel, visionModel)
}

func newServiceWithModels(
	parent context.Context,
	database *storage.DB,
	chatModel model.ToolCallingChatModel,
	visionModel model.ToolCallingChatModel,
) (*Service, error) {
	if database == nil {
		return nil, errors.New("agent service 缺少数据库")
	}
	ctx, cancel := context.WithCancel(parent)
	if err := expireStaleAgentEditLeases(ctx, database); err != nil {
		cancel()
		return nil, fmt.Errorf("清理过期 Agent edit lease: %w", err)
	}
	chatModel = newTimeoutRetryChatModel(chatModel)
	service := &Service{
		database: database, hub: NewTurnStreamHub(0), ctx: ctx, cancel: cancel,
		chatModel: chatModel, analyzer: understanding.NewAnalyzer(visionModel),
		contextManager:   NewContextManager(database),
		indexedResources: agentexec.NewIndexedResourceCoordinator(),
	}
	if err := service.interruptStaleAgentTurnRuns(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("标记重启中断的 Agent turn: %w", err)
	}
	service.executor = agentexec.New(database, service.analyzer, nil, func(draftID string, event map[string]any) {
		service.hub.Record(draftID, event)
	})
	service.executor.SetSameTurnWaitObserver(recordSameTurnToolWait)
	service.fallbackScaffold = newFallbackScaffold(service)
	registry, err := rushestools.NewRegistry(database, service)
	if err != nil {
		cancel()
		return nil, err
	}
	service.tools = registry
	registry.UseAdmission(modelToolSurfaceInterceptor)
	// G2：破坏性工具（当前为 memory.remove）在模型主路径上必须先经
	// interaction.confirm_action 确认；确认后的重放走直连路径、绕过本拦截器（#103 G2）。
	registry.Use(destructiveConfirmationInterceptor)
	recordModelToolCatalog(ctx, registry)
	if chatModel != nil {
		// #103 G3b/#141：react 图把单个 ToolsNode 换成按 Registry Spec 逐消息路由的
		// toolRouter（纯读和资源隔离 detector 并行，edit/control 与重复资源串行）。
		// 模型侧 H5 直通模型 / StreamToolCallChecker / H1b MessageModifier / MaxStep 全部原样保留。
		service.react, err = newConcurrentReactAgent(
			ctx,
			&dynamicToolSurfaceModel{inner: chatModel, registry: registry},
			compose.ToolsNodeConfig{
				// ToolsNode 持有 Registry 全量目录用于实际分发；dynamicToolSurfaceModel
				// 每次 provider 调用只绑定当前状态/阶段允许的子集，并由 interceptor
				// 阻止模型绕过未披露能力。
				Tools:               registry.EinoTools(true, false),
				UnknownToolsHandler: unknownToolRecoveryHandler,
				ToolCallMiddlewares: []compose.ToolMiddleware{newToolRecoveryMiddleware(
					retrySafeFromEffect(registry.Effect), registry.ModelReceiptPolicy,
				)},
			},
			registry.Spec,
			// 多主题口播可能需要 30 轮以上的模型/工具往返，因此将真实预算保留到 40 轮；
			// 最后 5 轮由 MessageModifier 注入收敛提醒。
			maxReActStepsPerTurn,
			FullStreamToolCallChecker,
			turnBudgetMessageModifier,
		)
		if err != nil {
			cancel()
			return nil, err
		}
	}
	service.queue = NewTurnQueue(ctx, service.runTurn)
	return service, nil
}

func (service *Service) Queue() *TurnQueue { return service.queue }

func (service *Service) Hub() *TurnStreamHub { return service.hub }

func (service *Service) Tools() *rushestools.Registry { return service.tools }

func (service *Service) SetSpeechRecognizer(recognizer contracts.SpeechRecognizer) {
	service.speechRecognizer = recognizer
	service.executor.SetSpeechRecognizer(recognizer)
}

func (service *Service) Close() {
	service.cancel()
	service.queue.Close()
}

func (service *Service) runTurn(ctx context.Context, item QueueItem) error {
	turnID := agentexec.RandomID("turn")
	messageID := agentexec.RandomID("msg")
	service.hub.Record(item.DraftID, StreamEvent{
		"type": TurnStreamTurnStarted, "turn_id": turnID,
	})
	startedAt := time.Now()
	slog.Info("turn_started", "turn_id", turnID, "draft_id", item.DraftID, "kind", string(item.Kind))
	ctx = rushestools.WithDraftID(ctx, item.DraftID)
	ctx = rushestools.WithTurnIdentity(ctx, turnID, item.ItemID)
	ctx, cancelForLease := context.WithCancelCause(ctx)
	defer cancelForLease(nil)
	leaseSession := newTimelineEditLeaseSession(
		service.database, item.DraftID, turnID, cancelForLease,
	)
	ctx = withTimelineEditLeaseSession(ctx, leaseSession)
	ctx = rushestools.WithTimelineWriteAdmission(
		ctx, turnID, leaseSession.token, leaseSession.markLost,
	)
	turnBudget := newTurnBudgetState(maxToolRoundsPerTurn)
	if err := service.startAgentTurnRun(ctx, turnID, item); err != nil {
		leaseSession.close()
		// turn_started 已经对客户端可见；用户可能正好在持久化 run marker
		// 前点击停止。即使取消让 reducer 立即返回，也必须配对一个 cancelled
		// turn_ended，不能让 UI 永久停在运行中。
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			service.recordTurnEnded(
				item.DraftID, turnID, startedAt, "cancelled", "user_cancelled",
				turnBudget, false,
			)
		}
		return err
	}
	turnRunStatus := "failed"
	defer func() {
		leaseSession.close()
		if finishErr := service.finishAgentTurnRun(ctx, turnID, turnRunStatus); finishErr != nil {
			slog.Error("持久化 Agent turn 终态失败", "turn_id", turnID, "error", finishErr)
		}
	}()
	if item.Kind == QueueUserMessage {
		ctx = withContextMessageBoundary(ctx, item.ItemID)
	}
	ctx = withQueueMemoryEvidence(ctx, item)
	ctx = withModelToolSurfaceSession(ctx)
	ctx, injectedMemory := withInjectedMemoryCollector(ctx)
	recoveryState := newToolRecoveryState()
	ctx = withToolRecoveryState(ctx, recoveryState)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	ctx = agentexec.WithTurnInteractionState(
		ctx,
		agentexec.NewTurnInteractionState(service.indexedResources),
	)
	ctx = agentexec.WithDurableTerminalCommit(ctx, func(commit func() (bool, error)) (bool, error) {
		return service.queue.CommitCurrentDurableTerminal(item, commit)
	})
	ctx = withTurnBudgetState(ctx, turnBudget)
	finishCancelled := func(turnErr error) error {
		turnRunStatus = "cancelled"
		service.recordTurnEnded(
			item.DraftID, turnID, startedAt, "cancelled", "user_cancelled", turnBudget, false,
		)
		if turnErr != nil {
			return turnErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return context.Canceled
	}
	ctx = service.withModelRetryReporting(ctx, item.DraftID)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, item.DraftID))
	content, err := service.turnContent(ctx, item, messageID)
	leaseCause := context.Cause(ctx)
	if errors.Is(err, storage.ErrAgentEditLeaseLost) ||
		errors.Is(leaseCause, storage.ErrAgentEditLeaseLost) {
		turnRunStatus = "lease_lost"
		if leaseCause == nil {
			leaseCause = storage.ErrAgentEditLeaseLost
		}
		service.recordTurnEnded(
			item.DraftID, turnID, startedAt, "failed", "agent_edit_lease_lost", turnBudget, false,
		)
		return leaseCause
	}
	// 用户主动取消有两种形态：错误链里包着 context.Canceled，或 provider 在连接
	// 中断时抛出的普通传输错误（不包裹 Canceled）但 turn 上下文已被取消。两者都
	// 只落 cancelled 终态，绝不合成 turn_failure；ctx.Err() 兜住后一种，与
	// model_retry.go 的既有护栏写法一致。
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return finishCancelled(err)
	}
	if guardErr := service.terminalReplyGuard(ctx, item.DraftID); guardErr != nil {
		// 即便 provider 在成功写入或工具调用之后又返回普通错误，也必须优先暴露
		// harness 已记录的真实终态，不能让错误路径绕过同版本检查或待确认策略门禁。
		err = guardErr
		content = ""
	}
	// H7:终态回复质检——夹带自我怀疑/中途推翻等过程性语句时,要求模型重述一次(最多 1 次)。
	reflectionRestated := false
	if err == nil && content != "" {
		content, reflectionRestated = service.qualityCheckedFinalReply(ctx, item.DraftID, messageID, content)
	}
	if ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), storage.ErrAgentEditLeaseLost) {
			turnRunStatus = "lease_lost"
			service.recordTurnEnded(
				item.DraftID, turnID, startedAt, "failed", "agent_edit_lease_lost", turnBudget, false,
			)
			return context.Cause(ctx)
		}
		// 取消可能落在初次检查之后的 terminal guard / reflection 窗口。此时尚未提交
		// durable terminal，必须仍以 cancelled 收口，不能进入 turn_failure 或 turn_error。
		return finishCancelled(ctx.Err())
	}
	outcome := "finished"
	var reason any
	if err != nil || (content == "" && !service.maySilentlyFinishTurn(ctx, item)) {
		if err == nil {
			if recoveryState.unresolved() {
				err = errors.New("模型在工具失败后没有生成最终回复")
			} else {
				err = errors.New("模型没有生成最终回复")
			}
		}
		content = terminalFailureReply(ctx, err)
		outcome = "failed"
		reason = agentexec.TruncateText(err.Error(), 800)
	}
	if content != "" {
		messageRole := "assistant"
		messageKind := "reply"
		switch {
		case err != nil:
			// harness 合成的终态失败文案（terminalFailureReply，恒非空）落持久系统
			// 失败消息，用户不在页面时也能事后读回；用户主动取消走上面的分支。
			messageRole, messageKind = "system", "turn_failure"
		}
		expectedTimelineID := ""
		if err == nil {
			expectedTimelineID = terminalExpectedTimelineID(
				terminalTimelineTruthFromContext(ctx).snapshot(),
			)
		}
		applyErr := service.commitFinalReply(
			ctx, item, messageID, messageRole, messageKind, content, expectedTimelineID,
		)
		var latestChanged *terminalReplyGuardError
		if errors.As(applyErr, &latestChanged) && latestChanged.kind == "timeline_latest_changed" {
			err = latestChanged
			content = terminalFailureReply(ctx, err)
			outcome = "failed"
			reason = agentexec.TruncateText(err.Error(), 800)
			messageRole, messageKind = "system", "turn_failure"
			applyErr = service.commitFinalReply(
				ctx, item, messageID, messageRole, messageKind, content, "",
			)
		}
		if applyErr != nil {
			if errors.Is(applyErr, context.Canceled) || ctx.Err() != nil {
				return finishCancelled(applyErr)
			}
			service.hub.Record(item.DraftID, StreamEvent{"type": TurnStreamTurnError, "message": applyErr.Error()})
			return applyErr
		}
		service.emitAssistantReply(item.DraftID, messageID, content)
		service.hub.Record(item.DraftID, StreamEvent{
			"type": TurnStreamMessageCompleted, "message_id": messageID,
			"kind": messageKind, "content": content,
		})
	}
	if outcome == "finished" {
		service.touchInjectedMemories(ctx, item.DraftID, injectedMemory.snapshot())
	}
	service.recordTurnEnded(item.DraftID, turnID, startedAt, outcome, reason, turnBudget, reflectionRestated)
	turnRunStatus = outcome
	return nil
}

func (service *Service) commitFinalReply(
	ctx context.Context,
	item QueueItem,
	messageID, role, kind, content, expectedTimelineID string,
) error {
	resultRows := reducer.ResultRows{Message: &reducer.MessageRow{
		ID: messageID, DraftID: item.DraftID, Role: role, Kind: kind, Content: content,
	}}
	var result reducer.Result
	_, applyErr := agentexec.CommitDurableTerminal(ctx, func() (bool, error) {
		var err error
		options := reducer.Options{Actor: contracts.ActorAgent, ResultRows: resultRows}
		if expectedTimelineID != "" {
			options.Validate = terminalTimelineLatestValidation(item.DraftID, expectedTimelineID)
		}
		result, err = reducer.Apply(ctx, service.database, nil, options)
		return err == nil && result.Status == reducer.StatusApplied, err
	})
	if applyErr != nil {
		return applyErr
	}
	if result.Status == reducer.StatusValidationFailed && expectedTimelineID != "" {
		return service.timelineLatestChangedError(ctx, item.DraftID, expectedTimelineID)
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("assistant message reducer status: %s", result.Status)
	}
	return nil
}

// lateToolCallDedupKey 为「终态直通后晚到的 tool_call」生成去重键：优先用 call ID，缺失时
// 退回流式分片索引，再退回函数名，保证同一 call 的多个流片只计一次（#95 H5，H-B P2）。
func lateToolCallDedupKey(call schema.ToolCall) string {
	if call.ID != "" {
		return call.ID
	}
	if call.Index != nil {
		return fmt.Sprintf("idx:%d", *call.Index)
	}
	return "name:" + call.Function.Name
}

func (service *Service) recordTurnEnded(
	draftID, turnID string, startedAt time.Time, outcome string, reason any,
	turnBudget *turnBudgetState, reflectionRestated bool,
) {
	turnEnded := StreamEvent{"type": TurnStreamTurnEnded, "outcome": outcome, "reason": reason}
	if usage := turnBudget.usageSnapshot(); usage != nil {
		turnEnded["token_usage"] = usage
	}
	// H7:重述率进 H3 度量——本回合终态回复被质检重述过时打标,便于聚合。正常回合不带此字段。
	if reflectionRestated {
		turnEnded["reflection_restated"] = true
	}
	service.hub.Record(draftID, turnEnded)

	// H3 度量 + 结构化落盘：SSE 事件本身不动，以下都是附加侧信道（回合时长/结局分类、
	// 累计 token 供缓存命中率、turn_ended 的 token 快照进结构化日志）。
	durationMS := time.Since(startedAt).Milliseconds()
	metricTurnDurationMS.Observe(durationMS)
	recordTurnOutcome(outcome)
	if reflectionRestated {
		metricReflectionRestated.Inc()
	}
	modelCalls, promptTokens, cachedTokens := turnBudget.telemetrySnapshot()
	metricPromptTokensTotal.Add(int64(promptTokens))
	metricCachedPromptTokensTotal.Add(int64(cachedTokens))
	slog.Info("turn_ended",
		"turn_id", turnID, "draft_id", draftID, "outcome", outcome,
		"duration_ms", durationMS, "model_calls", modelCalls,
		"tool_rounds", max(0, modelCalls-1),
		"prompt_tokens", promptTokens, "cached_prompt_tokens", cachedTokens,
		"reflection_restated", reflectionRestated,
	)
}

// touchInjectedMemories 在回合成功收尾后刷新本回合注入的用户记忆 last_used_at。
// 走 reducer 单写路径；best-effort，失败只记日志、不影响回合结局。用 WithoutCancel
// 隔离回合结束后可能到来的取消，避免这笔记账被顺带杀掉。
func (service *Service) touchInjectedMemories(ctx context.Context, draftID string, keys []string) {
	if len(keys) == 0 {
		return
	}
	if _, err := reducer.Apply(context.WithoutCancel(ctx), service.database, nil, reducer.Options{
		Actor:      contracts.ActorAgent,
		ResultRows: reducer.ResultRows{UserMemoryTouchKeys: keys},
	}); err != nil {
		slog.Warn("刷新用户记忆 last_used_at 失败", "draft_id", draftID, "error", err)
	}
}

func (service *Service) withModelRetryReporting(ctx context.Context, draftID string) context.Context {
	return withModelRetryReporter(ctx, func(notice modelRetryNotice) {
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamModelRetry, "attempt": notice.Attempt,
			"max_retries": notice.MaxRetries, "reason": notice.Reason,
			"next_delay_ms": notice.Delay.Milliseconds(),
		})
	})
}

func (service *Service) maySilentlyFinishTurn(ctx context.Context, item QueueItem) bool {
	return false
}

func (service *Service) turnContent(ctx context.Context, item QueueItem, messageID string) (string, error) {
	if item.Kind == QueueUIObservation {
		if observationType, _ := item.Payload["observation_type"].(string); observationType == "decision_answered" {
			pending, _ := item.Payload["pending_tool_call"].(map[string]any)
			if pending == nil {
				return service.continueAfterDecision(ctx, item, messageID)
			}
		}
		return service.replayPendingTool(ctx, item)
	}
	content, _ := item.Payload["content"].(string)
	if service.react == nil {
		return service.fallbackTurn(ctx, item.DraftID, messageID, content)
	}
	messages, err := service.modelMessages(ctx, item.DraftID)
	if err != nil {
		return "", err
	}
	return service.streamAgent(ctx, item.DraftID, messageID, messages)
}

func (service *Service) streamAgent(
	ctx context.Context,
	draftID, messageID string,
	messages []*schema.Message,
) (string, error) {
	stream, err := service.react.Stream(ctx, messages)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var output strings.Builder
	var roundUsage *schema.TokenUsage
	seenLateToolCalls := map[string]struct{}{}
	for {
		message, receiveErr := stream.Recv()
		if errors.Is(receiveErr, io.EOF) {
			break
		}
		if receiveErr != nil {
			return "", receiveErr
		}
		if message == nil {
			continue
		}
		// 模型正文之后若又出现 tool_call 分片，说明供应商流违反了「工具轮不在
		// tool_call 前吐正文」的假设（见 stream_checker.go classifyModelChunk）。此调用不会被
		// 执行，但回复仍在本地缓冲区，终态门禁通过前不会暴露给用户；同时保留告警与计数，
		// 让该假设在真实模型上可证伪、坏了能经 H3 聚合发现（#95 H5，决策 2 观测保护）。
		if len(message.ToolCalls) > 0 {
			// 按 tool-call 去重计数：流式里同一个 call 会分多片抵达，只应计一次（H-B P2）；
			// ID 缺失时退回 index/函数名做去重键。
			for _, call := range message.ToolCalls {
				key := lateToolCallDedupKey(call)
				if _, seen := seenLateToolCalls[key]; seen {
					continue
				}
				seenLateToolCalls[key] = struct{}{}
				passthroughLateToolCallCount.Add(1)
				metricPassthroughLateToolCalls.Inc()
				slog.Warn("终态轮直通后出现未执行的 tool_call，模型可能在正文之后才发起工具调用",
					"draft_id", draftID, "message_id", messageID,
					"tool_name", call.Function.Name, "tool_call_id", call.ID)
			}
		}
		// 末片携带的 Usage 随流抵达，取最新一份（供应商通常在末片给全量），
		// 回合读尽后记一次账。正文只写入本地缓冲区，由 runTurn 通过终态门禁后统一发送。
		if usage := messageTokenUsage(message); usage != nil {
			roundUsage = usage
		}
		if message.Content == "" {
			continue
		}
		output.WriteString(message.Content)
	}
	recordTokenUsage(ctx, roundUsage)
	if len(seenLateToolCalls) > 0 {
		return "", &terminalReplyGuardError{kind: "terminal_late_tool_call"}
	}
	return output.String(), nil
}

func (service *Service) continueAfterDecision(
	ctx context.Context,
	item QueueItem,
	messageID string,
) (string, error) {
	decisionID := agentexec.InterfaceString(item.Payload["decision_id"])
	if decisionID == "" {
		return "", errors.New("决策回答缺少 decision_id")
	}
	decision, err := storage.GetDecision(ctx, service.database.Read(), decisionID)
	if err != nil {
		return "", err
	}
	if decision.DraftID == nil || *decision.DraftID != item.DraftID {
		return "", errors.New("决策与当前草稿不匹配")
	}
	answer, _ := item.Payload["answer"].(map[string]any)
	if answer == nil {
		answer = decision.Answer
	}
	prompt := decisionContinuationPrompt(decision, answer)
	if service.react == nil {
		return service.fallbackTurn(ctx, item.DraftID, messageID, prompt)
	}
	messages, err := service.modelMessages(ctx, item.DraftID)
	if err != nil {
		return "", err
	}
	messages = append(messages, schema.UserMessage(prompt))
	return service.streamAgent(ctx, item.DraftID, messageID, messages)
}

func decisionContinuationPrompt(decision storage.Decision, answer map[string]any) string {
	optionID := agentexec.InterfaceString(answer["option_id"])
	freeText := agentexec.InterfaceString(answer["free_text"])
	label := ""
	for _, option := range decision.Options {
		if agentexec.InterfaceString(option["option_id"]) == optionID {
			label = agentexec.InterfaceString(option["label"])
			break
		}
	}
	answerParts := make([]string, 0, 2)
	if label != "" {
		answerParts = append(answerParts, fmt.Sprintf("%s（option_id: %s）", label, optionID))
	} else if optionID != "" {
		answerParts = append(answerParts, fmt.Sprintf("option_id: %s", optionID))
	}
	if freeText != "" {
		answerParts = append(answerParts, "补充说明："+freeText)
	}
	if len(answerParts) == 0 {
		answerParts = append(answerParts, "用户已提交回答")
	}
	return fmt.Sprintf(
		"用户刚刚回答了你此前提出的选择题。\n问题：%s\n回答：%s\n这是同一条任务的继续，不是新的请求。请立即根据这个回答继续执行剩余工作；不要重复提出已经回答的问题。需要工具时继续调用工具，直到任务完成或确实还缺少新的阻塞性信息。",
		decision.Question,
		strings.Join(answerParts, "；"),
	)
}

func (service *Service) fallbackTurn(
	ctx context.Context,
	draftID, messageID, content string,
) (string, error) {
	if service.fallbackScaffold != nil {
		reply, handled, err := service.fallbackScaffold.TryHandle(ctx, draftID, messageID, content)
		if handled || err != nil {
			return reply, err
		}
	}
	if strings.Contains(content, "混剪") {
		reply, err := service.fallbackMainline(ctx, draftID)
		if err != nil || !requestsUserFinalExport(content) {
			return reply, err
		}
		return reply + " " + userFinalExportGuidance, nil
	}
	if requestsUserFinalExport(content) {
		if _, err := timeline.Latest(ctx, service.database, draftID); errors.Is(err, storage.ErrNotFound) {
			return "当前草稿还没有可导出的时间线。", nil
		} else if err != nil {
			return "", err
		}
		return "编辑结果已准备好。" + userFinalExportGuidance, nil
	}
	if strings.Contains(content, "ASK_USER") {
		_, err := service.ExecuteTool(ctx, "interaction.ask_user", rushestools.AskUserInput{
			Question:     "当前素材无法判断整体节奏方向，请选择这次成片的核心节奏。",
			DecisionType: "critical",
			Options: []rushestools.DecisionOptionInput{
				{OptionID: "fast", Label: "快节奏"}, {OptionID: "calm", Label: "舒缓"},
			},
		})
		if err != nil {
			return "", err
		}
	}
	reply := "未配置模型密钥：已记录你的需求，并保持本地编辑链路可用。"
	return reply, nil
}

const userFinalExportGuidance = "最终视频只能由你明确触发：请在编辑器右侧的“导出”区域选择规格并点击“导出视频”，完成后可直接下载。"

func (service *Service) modelMessages(ctx context.Context, draftID string) ([]*schema.Message, error) {
	boundary := contextMessageBoundary(ctx)
	build, err := service.contextManager.BuildThroughMessage(ctx, draftID, boundary)
	if err != nil {
		return nil, err
	}
	if build.Manifest.NeedsCompaction {
		// H3：压缩触发计数 + 触发时的历史 token（供阈值校准，H-B P2）。
		metricCompactionTriggered.Inc()
		metricCompactionTriggerTokens.Observe(int64(build.Manifest.HistoryTokens))
		if err := service.compactModelContext(ctx, draftID, build, true); err != nil {
			return nil, err
		}
		build, err = service.contextManager.BuildThroughMessage(ctx, draftID, boundary)
		if err != nil {
			return nil, err
		}
	}
	return build.Messages, nil
}

func (service *Service) compactModelContext(
	ctx context.Context,
	draftID string,
	build ContextBuild,
	preservePendingUser bool,
) error {
	source, through, ok := build.CompactionSource(preservePendingUser)
	if !ok {
		return nil
	}
	summary := service.contextSummary(ctx, draftID, source)
	return service.contextManager.ReplaceHistory(ctx, draftID, build, summary, through)
}

func (service *Service) contextSummary(ctx context.Context, draftID, source string) string {
	summary := deterministicContextSummary(source)
	if service.chatModel == nil {
		return summary
	}
	response, err := generateWithCurrentTimelineView(ctx, service.chatModel, []*schema.Message{
		schema.SystemMessage(contextCompactionPrompt),
		schema.UserMessage(source),
	}, model.WithToolChoice(schema.ToolChoiceForbidden))
	if err != nil || response == nil || strings.TrimSpace(response.Content) == "" {
		reason := "模型返回空摘要"
		if err != nil {
			reason = agentexec.TruncateText(err.Error(), 500)
		}
		metricCompactionDegraded.Inc()
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamContextCompactionFailed, "reason": reason,
			"fallback": "deterministic_bounded_summary",
		})
		return summary
	}
	return agentexec.TruncateRunes(strings.TrimSpace(response.Content), contextCompactionSummaryRuneLimit)
}

func deterministicContextSummary(source string) string {
	return "自动语义压缩不可用时保留的有界历史交接；其中状态描述可能过期，" +
		"必须以当前 WorldState 为准。\n" + tailRunes(strings.TrimSpace(source), contextCompactionFallbackRuneLimit)
}

func tailRunes(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 {
		return ""
	}
	if len(runes) <= limit {
		return value
	}
	return string(runes[len(runes)-limit:])
}

const contextCompactionPrompt = `你是 Rushes 的上下文压缩器。禁止调用工具，只输出简体中文交接摘要。
摘要必须可替换被压缩的历史，并严格分为：
1. 当前创作目标与用户明确偏好；
2. 已确认的关键决定与约束；
3. 已完成进展（只写语义结论，不复制整条时间线）；
4. 未完成事项和下一步；
5. 仍需保留的关键 ID、错误证据或用户纠正。
draft.content_plan 已持久保存的决定不要重复写入摘要；只保留计划外的新决定或冲突。
user_memories 已持久保存的偏好不要重复写入摘要；只保留尚未固化的新偏好，并提示下回合固化。
不要把历史回复里的素材、时间线、响度或节拍判断写成当前事实；这些客观信息会由最新 WorldState 单独注入。删除寒暄、重复工具日志、已被用户推翻的判断和冗余过程。`

func (service *Service) toolReporter(ctx context.Context, draftID string) rushestools.Reporter {
	type activeStep struct {
		id           string
		argsSummary  string
		previewID    string
		previewCheck string
	}
	var mu sync.Mutex
	steps := map[string]activeStep{}
	return func(reportCtx context.Context, name, phase string, input, output any, err error) {
		mu.Lock()
		defer mu.Unlock()
		key := rushestools.ToolCallID(reportCtx)
		if key == "" {
			key = name
		}
		if phase == "started" {
			stepID := agentexec.RandomID("step")
			argsSummary := compactJSON(input)
			previewID := previewIDFromToolReport(name, input)
			previewCheck := previewCheckFromToolReport(name, input)
			steps[key] = activeStep{
				id: stepID, argsSummary: argsSummary,
				previewID: previewID, previewCheck: previewCheck,
			}
			service.hub.Record(draftID, StreamEvent{
				"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": name,
				"args_summary": argsSummary,
			})
			return
		}
		step := steps[key]
		stepID := step.id
		if stepID == "" {
			stepID = agentexec.RandomID("step")
		}
		delete(steps, key)
		status := "succeeded"
		observation := compactJSON(output)
		if err != nil {
			status, observation = "failed", err.Error()
		} else if structuredToolOutputFailed(output) {
			status = "failed"
		}
		truthContext := reportCtx
		if truthContext == nil {
			truthContext = ctx
		}
		if truthState := terminalTimelineTruthFromContext(truthContext); truthState != nil {
			truthState.recordToolResult(name, status, output)
		}
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": name,
			"status": status, "observation": observation,
		})
		_ = service.persistToolTrace(
			context.WithoutCancel(ctx), draftID, stepID, name, status, step.argsSummary, observation,
			step.previewID, step.previewCheck,
		)
	}
}

func structuredToolOutputFailed(output any) bool {
	encoded, err := json.Marshal(output)
	return err == nil && isStructuredToolFailure(string(encoded))
}

func previewIDFromToolReport(name string, input any) string {
	if name != "preview.check" {
		return ""
	}
	switch typed := input.(type) {
	case rushestools.PreviewCheckInput:
		return strings.TrimSpace(typed.PreviewID)
	case *rushestools.PreviewCheckInput:
		if typed != nil {
			return strings.TrimSpace(typed.PreviewID)
		}
	case map[string]any:
		return strings.TrimSpace(agentexec.InterfaceString(typed["preview_id"]))
	}
	return ""
}

func previewCheckFromToolReport(name string, input any) string {
	if name != "preview.check" {
		return ""
	}
	switch typed := input.(type) {
	case rushestools.PreviewCheckInput:
		return strings.TrimSpace(typed.Check)
	case *rushestools.PreviewCheckInput:
		if typed != nil {
			return strings.TrimSpace(typed.Check)
		}
	case map[string]any:
		return strings.TrimSpace(agentexec.InterfaceString(typed["check"]))
	}
	return ""
}

// 工具折叠区在刷新后仍需存在，因此完成态通过 Reducer 持久化为 system/tool 消息。
// 该消息只供 UI 回放，modelMessages 会过滤，避免工具 JSON 污染模型上下文。
func (service *Service) persistToolTrace(
	ctx context.Context,
	draftID, stepID, name, status, argsSummary, observation, previewID, previewCheck string,
) error {
	record := map[string]any{
		"step_id": stepID, "tool": name, "status": status,
		"args_summary": argsSummary, "observation": observation,
	}
	if previewID != "" {
		record["preview_id"] = previewID
	}
	if previewCheck != "" {
		record["preview_check"] = previewCheck
	}
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: stepID, DraftID: draftID, Role: "system", Kind: "tool", Content: string(content),
		}},
	})
	if err != nil {
		return err
	}
	if result.Status != reducer.StatusApplied {
		return fmt.Errorf("tool trace reducer status: %s", result.Status)
	}
	return nil
}

func runeChunks(value string, size int) []string {
	if size <= 0 {
		size = 1
	}
	runes := []rune(value)
	chunks := []string{}
	for start := 0; start < len(runes); start += size {
		chunks = append(chunks, string(runes[start:min(start+size, len(runes))]))
	}
	return chunks
}

func compactJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	if len(data) > 240 {
		data = append(data[:237], '.', '.', '.')
		for !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	return string(data)
}

func (service *Service) executeReported(
	ctx context.Context,
	draftID, name string,
	input any,
) (any, error) {
	reporter := service.toolReporter(ctx, draftID)
	reporter(ctx, name, "started", input, nil, nil)
	output, err := service.ExecuteTool(ctx, name, input)
	reporter(ctx, name, "finished", input, output, err)
	return output, err
}

func (service *Service) replayPendingTool(ctx context.Context, item QueueItem) (string, error) {
	pending, _ := item.Payload["pending_tool_call"].(map[string]any)
	answer, _ := item.Payload["answer"].(map[string]any)
	if pending == nil {
		return "已收到你的选择，我会按这个决定继续。", nil
	}
	optionID := agentexec.InterfaceString(answer["option_id"])
	if optionID != "confirm" {
		return "已取消这项操作。", nil
	}
	name, _ := pending["tool_name"].(string)
	arguments, _ := pending["arguments"].(map[string]any)
	if err := service.tools.ValidateConfirmation(ctx, name, arguments); err != nil {
		return "", fmt.Errorf("确认工具重放校验失败: %w", err)
	}
	input, err := service.tools.DecodeInput(name, arguments)
	if err != nil {
		return "", err
	}
	ctx = agentexec.WithConfirmedToolReplay(ctx)
	result, err := service.executeConfirmedReported(
		ctx, item.DraftID, name, input, arguments,
	)
	if err != nil {
		return "", err
	}
	if isTerminalTimelineMutation(name) {
		timelineID := agentexec.InterfaceString(result.Data["timeline_id"])
		if timelineID == "" {
			return "", &terminalReplyGuardError{kind: "timeline_check_missing"}
		}
		_, checkErr := service.executeConfirmedReported(
			ctx,
			item.DraftID,
			"timeline.check",
			rushestools.TimelineCheckInput{TimelineID: timelineID},
			map[string]any{"timeline_id": timelineID},
		)
		if checkErr != nil {
			return "", &terminalReplyGuardError{
				kind: "timeline_check_missing", mutationTimelineID: timelineID,
			}
		}
	}
	if result.Observation != "" {
		return result.Observation, nil
	}
	return "已按你的确认继续执行。", nil
}

func (service *Service) executeConfirmedReported(
	ctx context.Context,
	draftID, name string,
	input any,
	arguments map[string]any,
) (rushestools.ToolResult, error) {
	reporter := service.toolReporter(ctx, draftID)
	reporter(ctx, name, "started", input, nil, nil)
	output, err := service.ExecuteTool(ctx, name, input)
	if err != nil {
		reporter(ctx, name, "finished", input, nil, err)
		return rushestools.ToolResult{}, err
	}
	result, proofErr := requireConfirmedToolSuccess(name, arguments, output, draftID)
	if proofErr != nil {
		reporter(ctx, name, "finished", input, nil, proofErr)
		return rushestools.ToolResult{}, proofErr
	}
	reporter(ctx, name, "finished", input, result, nil)
	return result, nil
}

func requireConfirmedToolSuccess(
	name string,
	arguments map[string]any,
	output any,
	draftID string,
) (rushestools.ToolResult, error) {
	result, ok := terminalTruthToolResult(output)
	if !ok {
		return rushestools.ToolResult{}, &terminalReplyGuardError{
			kind: "tool_policy_unresolved", details: "确认重放的 " + name + " 未返回有效 ToolResult",
		}
	}
	// 该路径直接同步调用 Service.ExecuteTool，返回值与当前 typed input 同栈绑定；
	// 不经过模型工具中间件，因此把这次直接执行作为完整请求 proof。
	if !confirmedToolResultSuccessWithExecutionProof(name, arguments, result, draftID, true) {
		details := result.Observation
		if details == "" {
			details = "status=" + result.Status
		}
		return rushestools.ToolResult{}, &terminalReplyGuardError{
			kind: "tool_policy_unresolved", details: "确认重放的 " + name + " 失败：" + details,
		}
	}
	return result, nil
}

var _ rushestools.Executor = (*Service)(nil)

// fallbackMainline 是引擎侧对领域「混剪主线」的薄委托:注入 executeReported 上报器,
// 编排序列本体在 agentexec.Executor.FallbackMainline。
func (service *Service) fallbackMainline(ctx context.Context, draftID string) (string, error) {
	return service.executor.FallbackMainline(ctx, draftID, func(c context.Context, name string, input any) (any, error) {
		return service.executeReported(c, draftID, name, input)
	})
}
