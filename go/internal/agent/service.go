package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
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
	maxReActStepsPerTurn               = maxToolRoundsPerTurn*2 + 1
	contextCompactionSummaryRuneLimit  = 4000
	contextCompactionFallbackRuneLimit = 3000
)

type Service struct {
	database         *storage.DB
	hub              *TurnStreamHub
	queue            *TurnQueue
	tools            *rushestools.Registry
	chatModel        model.ToolCallingChatModel
	react            *react.Agent
	analyzer         *understanding.Analyzer
	speechRecognizer contracts.SpeechRecognizer
	contextManager   *ContextManager
	fallbackScaffold fallbackScaffold
	cancel           context.CancelFunc
	bridgeWG         sync.WaitGroup
	bridgeMu         sync.Mutex
	bridgeInflight   map[string]struct{}
	bridgeDispatchMu sync.Mutex
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
	if database == nil {
		return nil, errors.New("agent service 缺少数据库")
	}
	ctx, cancel := context.WithCancel(parent)
	chatModel = newTimeoutRetryChatModel(chatModel)
	service := &Service{
		database: database, hub: NewTurnStreamHub(0), cancel: cancel,
		chatModel: chatModel, analyzer: understanding.NewAnalyzer(visionModel),
		contextManager: NewContextManager(database),
		bridgeInflight: map[string]struct{}{},
	}
	service.fallbackScaffold = newFallbackScaffold(service)
	registry, err := rushestools.NewRegistry(database, service)
	if err != nil {
		cancel()
		return nil, err
	}
	service.tools = registry
	recordModelToolSchemaSize(ctx, registry)
	if chatModel != nil {
		service.react, err = react.NewAgent(ctx, &react.AgentConfig{
			ToolCallingModel: chatModel,
			ToolsConfig: compose.ToolsNodeConfig{
				Tools:               registry.EinoTools(true, false),
				ExecuteSequentially: true,
				UnknownToolsHandler: unknownToolRecoveryHandler,
				ToolCallMiddlewares: []compose.ToolMiddleware{newToolRecoveryMiddleware()},
			},
			// 多主题口播可能需要 30 轮以上的模型/工具往返，因此将真实预算
			// 保留到 40 轮；最后 5 轮由 MessageModifier 注入收敛提醒。
			MaxStep:               maxReActStepsPerTurn,
			StreamToolCallChecker: FullStreamToolCallChecker,
			MessageModifier:       turnBudgetMessageModifier,
		})
		if err != nil {
			cancel()
			return nil, err
		}
	}
	service.queue = NewTurnQueue(ctx, service.runTurn)
	service.startJobObservationBridge(ctx)
	return service, nil
}

func (service *Service) Queue() *TurnQueue { return service.queue }

func (service *Service) Hub() *TurnStreamHub { return service.hub }

func (service *Service) Tools() *rushestools.Registry { return service.tools }

func (service *Service) SetSpeechRecognizer(recognizer contracts.SpeechRecognizer) {
	service.speechRecognizer = recognizer
}

func (service *Service) Close() {
	service.cancel()
	service.bridgeWG.Wait()
	service.queue.Close()
}

func (service *Service) runTurn(ctx context.Context, item QueueItem) error {
	turnID := randomID("turn")
	messageID := randomID("msg")
	service.hub.Record(item.DraftID, StreamEvent{
		"type": TurnStreamTurnStarted, "turn_id": turnID,
	})
	ctx = rushestools.WithDraftID(ctx, item.DraftID)
	if item.Kind == QueueUserMessage {
		ctx = withContextMessageBoundary(ctx, item.ItemID)
	}
	ctx = withQueueMemoryEvidence(ctx, item)
	recoveryState := newToolRecoveryState()
	ctx = withToolRecoveryState(ctx, recoveryState)
	ctx = withTurnInteractionState(ctx, newTurnInteractionState())
	turnBudget := newTurnBudgetState(maxToolRoundsPerTurn)
	ctx = withTurnBudgetState(ctx, turnBudget)
	ctx = service.withModelRetryReporting(ctx, item.DraftID)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, item.DraftID))
	content, err := service.turnContent(ctx, item, messageID)
	// 用户主动取消有两种形态：错误链里包着 context.Canceled，或 provider 在连接
	// 中断时抛出的普通传输错误（不包裹 Canceled）但 turn 上下文已被取消。两者都
	// 只落 cancelled 终态，绝不合成 turn_failure；ctx.Err() 兜住后一种，与
	// model_retry.go 的既有护栏写法一致。
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		service.recordTurnEnded(item.DraftID, "cancelled", "user_cancelled", turnBudget)
		return err
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
		content = service.terminalFailureReply(ctx, item.DraftID, messageID, err)
		outcome = "failed"
		reason = agentexec.TruncateText(err.Error(), 800)
	} else if recoveryState.recoveryExhausted() {
		// 模型在恢复预算耗尽后亲笔生成了失败说明：这条走下面的 assistant/reply
		// 保留为可见回复并注入下一轮上下文，但回合终态必须真实标记为 failed，
		// 不能把“已停止修复”记成完成。
		outcome = "failed"
		reason = agentexec.TruncateText(recoveryState.summary(), 800)
	}
	if content != "" {
		messageRole := "assistant"
		messageKind := "reply"
		switch {
		case err != nil:
			// 只有 harness 合成的终态失败文案（terminalFailureReply，恒非空）落
			// 持久系统失败消息，用户不在页面时也能事后从 DB 读回。模型在恢复预算
			// 耗尽后亲笔生成的失败说明走下面的 assistant/reply，仍注入下一轮上下文；
			// 用户主动取消走上面的 context.Canceled/ctx.Err 分支，不会到这里。
			messageRole, messageKind = "system", "turn_failure"
		case item.Kind == QueueJobObservation && service.react == nil:
			messageKind = "observation"
		}
		resultRows := reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: item.DraftID, Role: messageRole, Kind: messageKind, Content: content,
		}}
		if delivery := observationDelivery(item); delivery != nil {
			resultRows.AgentJobObservationDelivery = delivery
		}
		result, applyErr := reducer.Apply(ctx, service.database, nil, reducer.Options{
			Actor:      contracts.ActorAgent,
			ResultRows: resultRows,
		})
		if applyErr != nil {
			service.hub.Record(item.DraftID, StreamEvent{"type": TurnStreamTurnError, "message": applyErr.Error()})
			return applyErr
		}
		if result.Status != reducer.StatusApplied {
			return fmt.Errorf("assistant message reducer status: %s", result.Status)
		}
		service.hub.Record(item.DraftID, StreamEvent{
			"type": TurnStreamMessageCompleted, "message_id": messageID,
			"kind": messageKind, "content": content,
		})
	} else if delivery := observationDelivery(item); delivery != nil {
		result, applyErr := reducer.Apply(ctx, service.database, nil, reducer.Options{
			Actor:      contracts.ActorAgent,
			ResultRows: reducer.ResultRows{AgentJobObservationDelivery: delivery},
		})
		if applyErr != nil {
			return applyErr
		}
		if result.Status != reducer.StatusApplied {
			return fmt.Errorf("job observation delivery reducer status: %s", result.Status)
		}
	}
	service.recordTurnEnded(item.DraftID, outcome, reason, turnBudget)
	return nil
}

func observationDelivery(item QueueItem) *reducer.AgentJobObservationDeliveryRow {
	if item.Kind != QueueJobObservation {
		return nil
	}
	claimToken, _ := item.Payload["claim_token"].(string)
	if claimToken == "" {
		return nil
	}
	return &reducer.AgentJobObservationDeliveryRow{JobID: item.ItemID, ClaimToken: claimToken}
}

func (service *Service) recordTurnEnded(draftID, outcome string, reason any, turnBudget *turnBudgetState) {
	turnEnded := StreamEvent{"type": TurnStreamTurnEnded, "outcome": outcome, "reason": reason}
	if usage := turnBudget.usageSnapshot(); usage != nil {
		turnEnded["token_usage"] = usage
	}
	service.hub.Record(draftID, turnEnded)
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

// The only intentionally silent turn is a duplicated successful preview job
// notification whose artifact was already inspected. User turns, decision
// continuations and failed background jobs must always finish with visible text.
func (service *Service) maySilentlyFinishTurn(ctx context.Context, item QueueItem) bool {
	if item.Kind != QueueJobObservation {
		return false
	}
	event, _ := item.Payload["event"].(map[string]any)
	if agentexec.InterfaceString(event["event"]) != "JobSucceeded" {
		return false
	}
	payload, _ := event["payload"].(map[string]any)
	if agentexec.InterfaceString(payload["kind"]) != "render_preview" {
		return false
	}
	return service.previewAlreadyInspected(ctx, item.DraftID, payload["result"])
}

func (service *Service) turnContent(ctx context.Context, item QueueItem, messageID string) (string, error) {
	if item.Kind == QueueJobObservation {
		return service.continueAfterJobObservation(ctx, item, messageID)
	}
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
	for {
		message, receiveErr := stream.Recv()
		if errors.Is(receiveErr, io.EOF) {
			break
		}
		if receiveErr != nil {
			return "", receiveErr
		}
		if message == nil || message.Content == "" {
			continue
		}
		output.WriteString(message.Content)
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamTextDelta, "message_id": messageID,
			"kind": "assistant", "delta": message.Content,
		})
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

func (service *Service) continueAfterJobObservation(
	ctx context.Context,
	item QueueItem,
	messageID string,
) (string, error) {
	event, _ := item.Payload["event"].(map[string]any)
	eventType := agentexec.InterfaceString(event["event"])
	payload, _ := event["payload"].(map[string]any)
	jobID := agentexec.InterfaceString(item.Payload["job_id"])
	if value := agentexec.InterfaceString(payload["job_id"]); value != "" {
		jobID = value
	}
	kind := agentexec.InterfaceString(payload["kind"])
	if kind == "" {
		kind = "后台"
	}
	succeeded := eventType == "JobSucceeded"
	cancelled := eventType == "JobCancelled"
	terminalDetails := payload["result"]
	if cancelled {
		terminalDetails = map[string]any{"reason": payload["reason"]}
	} else if !succeeded {
		terminalDetails = payload["error"]
		if terminalDetails == nil {
			terminalDetails = payload["failure"]
		}
	}
	details := compactJSON(terminalDetails)
	if service.react == nil {
		if succeeded {
			return fmt.Sprintf("%s 任务 %s 已完成。", kind, jobID), nil
		}
		if cancelled {
			return fmt.Sprintf("后台任务已被取消：%s（job_id：%s）。", kind, jobID), nil
		}
		return fmt.Sprintf("%s 任务 %s 失败：%s", kind, jobID, details), nil
	}
	if succeeded && kind == "render_preview" && service.previewAlreadyInspected(ctx, item.DraftID, terminalDetails) {
		return "", nil
	}
	verificationReport := ""
	if succeeded && kind == "render_preview" {
		report, reportErr := service.previewVerificationReport(ctx, item.DraftID, terminalDetails)
		if reportErr != nil {
			slog.Warn("预览自动质检失败", "draft_id", item.DraftID, "error", reportErr)
			report = degradedPreviewVerificationReport(terminalDetails)
		}
		if report != nil {
			verificationReport = "\nverification_report：" + compactJSON(report)
		}
	}
	status := "成功"
	nextAction := contracts.DefaultJobContinuationHint
	if spec, exists := contracts.LookupJobKind(kind); exists && spec.ContinuationHint != "" {
		nextAction = spec.ContinuationHint
	}
	if !succeeded {
		status = "失败"
		nextAction = "先读取失败信息并诊断；能用现有工具修复时立即修复并重试，不要把失败说成完成。"
	}
	if cancelled {
		status = "已取消"
		nextAction = "明确说明后台任务已被取消；保留现有成果，不要自动重试，也不要把取消说成失败。"
	}
	prompt := fmt.Sprintf(
		"你等待的后台任务已到终态。\n任务：%s\njob_id：%s\n状态：%s\n终态详情：%s%s\n这是原任务的自动续跑，不是新的用户请求。%s 不要重复询问已经回答的问题，也不要仅回复泛化的“后台已完成”。",
		kind,
		jobID,
		status,
		details,
		verificationReport,
		nextAction,
	)
	messages, err := service.modelMessages(ctx, item.DraftID)
	if err != nil {
		return "", err
	}
	if succeeded && kind == "understand" {
		evidence, evidenceErr := service.understandJobEvidenceMessage(ctx, item.DraftID, jobID)
		if evidenceErr != nil {
			return "", evidenceErr
		}
		messages = append(messages, evidence)
	}
	messages = append(messages, schema.UserMessage(prompt))
	return service.streamAgent(ctx, item.DraftID, messageID, messages)
}

func (service *Service) previewVerificationReport(
	ctx context.Context,
	draftID string,
	result any,
) (map[string]any, error) {
	resultMap, _ := result.(map[string]any)
	previewID := agentexec.InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = agentexec.InterfaceString(resultMap["preview_id"])
	}
	if previewID == "" {
		return nil, nil
	}
	inspection, err := service.toolInspectPreview(ctx, draftID, rushestools.RenderInspectInput{PreviewID: previewID})
	if err != nil {
		return nil, err
	}
	var timelineVersion int
	if err := service.database.Read().QueryRowContext(ctx,
		"SELECT timeline_version FROM previews WHERE preview_id=? AND draft_id=?", previewID, draftID,
	).Scan(&timelineVersion); err != nil {
		return nil, err
	}
	document, err := timeline.Get(ctx, service.database, draftID, timelineVersion)
	if err != nil {
		return nil, err
	}
	contractReport, hasContract, err := service.verifyContentContract(ctx, draftID, document)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"preview_id":        previewID,
		"timeline_version":  timelineVersion,
		"render_inspection": inspection,
	}
	if hasContract {
		report["content_contract"] = contractReport
	}
	return report, nil
}

func degradedPreviewVerificationReport(result any) map[string]any {
	resultMap, _ := result.(map[string]any)
	previewID := agentexec.InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = agentexec.InterfaceString(resultMap["preview_id"])
	}
	issue := map[string]any{
		"check":      "inspection",
		"severity":   "warning",
		"error_code": "preview_inspection_unavailable",
		"message":    "自动质检暂不可用，请稍后重试。",
	}
	return map[string]any{
		"preview_id": previewID,
		"degraded":   true,
		"issues":     []map[string]any{issue},
	}
}

func (service *Service) previewAlreadyInspected(ctx context.Context, draftID string, result any) bool {
	resultMap, _ := result.(map[string]any)
	previewID := agentexec.InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = agentexec.InterfaceString(resultMap["preview_id"])
	}
	if previewID == "" {
		return false
	}
	messages, err := storage.ListMessages(ctx, service.database.Read(), draftID, 200)
	if err != nil {
		return false
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Kind != "tool" {
			continue
		}
		var record struct {
			Tool        string `json:"tool"`
			PreviewID   string `json:"preview_id"`
			ArgsSummary string `json:"args_summary"`
			Status      string `json:"status"`
		}
		if json.Unmarshal([]byte(messages[index].Content), &record) != nil ||
			record.Tool != "render.inspect_preview" || record.Status != "succeeded" {
			continue
		}
		if record.PreviewID == previewID {
			return true
		}
		// Pre-D3 traces only stored preview_id inside an untruncated args_summary.
		// New traces use the top-level field and never depend on this compatibility path.
		if record.PreviewID == "" {
			var legacyArgs struct {
				PreviewID string `json:"preview_id"`
			}
			if json.Unmarshal([]byte(record.ArgsSummary), &legacyArgs) == nil && legacyArgs.PreviewID == previewID {
				return true
			}
		}
	}
	return false
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
		return service.fallbackFullMainline(ctx, draftID)
	}
	if strings.Contains(content, "导出") {
		_, err := service.executeReported(ctx, draftID, "interaction.confirm_action", rushestools.ConfirmActionInput{
			Question: "确认导出当前已验证时间线的最终 MP4？",
			ToolName: "render.final_mp4", Arguments: map[string]any{},
		})
		if err != nil {
			return "", err
		}
		return "请在决策卡中确认是否导出最终 MP4。", nil
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
	for _, delta := range runeChunks(reply, 6) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamTextDelta, "message_id": messageID,
			"kind": "assistant", "delta": delta,
		})
	}
	return reply, nil
}

func (service *Service) modelMessages(ctx context.Context, draftID string) ([]*schema.Message, error) {
	boundary := contextMessageBoundary(ctx)
	build, err := service.contextManager.BuildThroughMessage(ctx, draftID, boundary)
	if err != nil {
		return nil, err
	}
	if build.Manifest.NeedsCompaction {
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
	response, err := service.chatModel.Generate(ctx, []*schema.Message{
		schema.SystemMessage(contextCompactionPrompt),
		schema.UserMessage(source),
	}, model.WithToolChoice(schema.ToolChoiceForbidden))
	if err != nil || response == nil || strings.TrimSpace(response.Content) == "" {
		reason := "模型返回空摘要"
		if err != nil {
			reason = agentexec.TruncateText(err.Error(), 500)
		}
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamContextCompactionFailed, "reason": reason,
			"fallback": "deterministic_bounded_summary",
		})
		return summary
	}
	return truncateRunes(strings.TrimSpace(response.Content), contextCompactionSummaryRuneLimit)
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
		id          string
		argsSummary string
		previewID   string
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
			stepID := randomID("step")
			argsSummary := compactJSON(input)
			previewID := previewIDFromToolReport(name, input)
			steps[key] = activeStep{id: stepID, argsSummary: argsSummary, previewID: previewID}
			service.hub.Record(draftID, StreamEvent{
				"type": TurnStreamToolStepStarted, "step_id": stepID, "tool": name,
				"args_summary": argsSummary,
			})
			return
		}
		step := steps[key]
		stepID := step.id
		if stepID == "" {
			stepID = randomID("step")
		}
		delete(steps, key)
		status := "succeeded"
		observation := compactJSON(output)
		if err != nil {
			status, observation = "failed", err.Error()
		} else if result, ok := output.(rushestools.ToolResult); ok &&
			(result.Status == "failed" || result.Status == "validation_failed") {
			status = "failed"
		}
		service.hub.Record(draftID, StreamEvent{
			"type": TurnStreamToolStepFinished, "step_id": stepID, "tool": name,
			"status": status, "observation": observation,
		})
		_ = service.persistToolTrace(
			context.WithoutCancel(ctx), draftID, stepID, name, status, step.argsSummary, observation,
			step.previewID,
		)
	}
}

func previewIDFromToolReport(name string, input any) string {
	if name != "render.inspect_preview" {
		return ""
	}
	switch typed := input.(type) {
	case rushestools.RenderInspectInput:
		return strings.TrimSpace(typed.PreviewID)
	case *rushestools.RenderInspectInput:
		if typed != nil {
			return strings.TrimSpace(typed.PreviewID)
		}
	case map[string]any:
		return strings.TrimSpace(agentexec.InterfaceString(typed["preview_id"]))
	}
	return ""
}

// 工具折叠区在刷新后仍需存在，因此完成态通过 Reducer 持久化为 system/tool 消息。
// 该消息只供 UI 回放，modelMessages 会过滤，避免工具 JSON 污染模型上下文。
func (service *Service) persistToolTrace(
	ctx context.Context,
	draftID, stepID, name, status, argsSummary, observation, previewID string,
) error {
	record := map[string]any{
		"step_id": stepID, "tool": name, "status": status,
		"args_summary": argsSummary, "observation": observation,
	}
	if previewID != "" {
		record["preview_id"] = previewID
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

func (service *Service) fallbackFullMainline(ctx context.Context, draftID string) (string, error) {
	listed, err := service.toolListAssets(ctx, draftID, rushestools.AssetListInput{OnlyUsable: boolPointer(true)})
	if err != nil {
		return "", err
	}
	visualAssets := make([]rushestools.AssetManifest, 0, len(listed.Assets))
	for _, asset := range listed.Assets {
		if asset.Kind == "video" || asset.Kind == "image" {
			visualAssets = append(visualAssets, asset)
		}
	}
	if len(visualAssets) == 0 {
		return "当前草稿还没有可用的视频或图片素材，请先导入素材。", nil
	}
	understandIDs := []string{}
	for _, asset := range visualAssets {
		if asset.UnderstandingStatus != "ready" {
			understandIDs = append(understandIDs, asset.AssetID)
		}
	}
	if len(understandIDs) > 0 {
		for _, assetID := range understandIDs {
			if _, err := service.executeReported(ctx, draftID, "understand.materials", rushestools.UnderstandInput{
				AssetIDs: []string{assetID}, Depth: "scan", Focus: "混剪可用画面",
			}); err != nil {
				return "", err
			}
		}
	}
	clips := make([]rushestools.ComposeClip, 0, len(visualAssets))
	for _, asset := range visualAssets {
		endFrame := asset.DurationFrames
		if endFrame <= 0 {
			endFrame = timeline.DefaultFPS
		}
		endFrame = min(endFrame, 5*timeline.DefaultFPS)
		clips = append(clips, rushestools.ComposeClip{
			AssetID: asset.AssetID, SourceStartFrame: 0, SourceEndFrame: endFrame, Role: "b_roll",
		})
	}
	if _, err := service.executeReported(ctx, draftID, "timeline.compose_initial", rushestools.ComposeInitialInput{Clips: clips}); err != nil {
		return "", err
	}
	if _, err := service.executeReported(ctx, draftID, "render.preview", rushestools.RenderPreviewInput{}); err != nil {
		return "", err
	}
	return "已完成素材理解与初版时间线，并开始渲染预览。", nil
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
	output, err := service.executeReported(ctx, item.DraftID, name, input)
	if err != nil {
		return "", err
	}
	if result, ok := output.(rushestools.ToolResult); ok && result.Observation != "" {
		return result.Observation, nil
	}
	return "已按你的确认继续执行。", nil
}

func randomID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(data)
}

func boolPointer(value bool) *bool { return &value }

var _ rushestools.Executor = (*Service)(nil)
