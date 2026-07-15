package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

const (
	// Eino 的 ReAct 图会把一次模型节点和一次工具节点分别计为一个 step。
	// 预留最后一次模型节点，确保执行完 30 次工具后仍能生成面向用户的终态回复。
	maxToolExecutionsPerTurn = 30
	maxReActStepsPerTurn     = maxToolExecutionsPerTurn*2 + 1
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
	cancel           context.CancelFunc
	bridgeWG         sync.WaitGroup
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
	}
	registry, err := rushestools.NewRegistry(database, service)
	if err != nil {
		cancel()
		return nil, err
	}
	service.tools = registry
	if chatModel != nil {
		service.react, err = react.NewAgent(ctx, &react.AgentConfig{
			ToolCallingModel: chatModel,
			ToolsConfig: compose.ToolsNodeConfig{
				Tools:               registry.EinoTools(true, false),
				ExecuteSequentially: true,
				UnknownToolsHandler: unknownToolRecoveryHandler,
				ToolCallMiddlewares: []compose.ToolMiddleware{newToolRecoveryMiddleware()},
			},
			// 一次真实剪辑常见链路会经历 list → understand → search → recut →
			// validate → render → inspect。这里按产品语义允许最多 30 次工具执行，
			// 再换算成 Eino 的模型/工具双节点计步预算。
			MaxStep:               maxReActStepsPerTurn,
			StreamToolCallChecker: FullStreamToolCallChecker,
			MessageModifier: func(_ context.Context, messages []*schema.Message) []*schema.Message {
				return append([]*schema.Message{schema.SystemMessage(systemPrompt)}, messages...)
			},
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
		"type": "turn_started", "turn_id": turnID,
	})
	ctx = rushestools.WithDraftID(ctx, item.DraftID)
	if item.Kind == QueueUserMessage {
		ctx = withContextMessageBoundary(ctx, item.ItemID)
	}
	recoveryState := newToolRecoveryState()
	ctx = withToolRecoveryState(ctx, recoveryState)
	ctx = service.withModelRetryReporting(ctx, item.DraftID)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, item.DraftID))
	content, err := service.turnContent(ctx, item, messageID)
	if errors.Is(err, context.Canceled) {
		service.hub.Record(item.DraftID, StreamEvent{
			"type": "turn_ended", "outcome": "cancelled", "reason": "user_cancelled",
		})
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
		reason = truncateText(err.Error(), 800)
	} else if recoveryState.recoveryExhausted() {
		// 模型可能在收到 exhausted=true 后正确生成了失败说明。保留这条可见
		// 回复，但终态必须真实标记为 failed，不能把“已停止修复”记成完成。
		outcome = "failed"
		reason = truncateText(recoveryState.summary(), 800)
	}
	if content != "" {
		messageKind := "reply"
		if item.Kind == QueueJobObservation && service.react == nil {
			messageKind = "observation"
		}
		result, applyErr := reducer.Apply(ctx, service.database, nil, reducer.Options{
			Actor: contracts.ActorAgent,
			ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
				ID: messageID, DraftID: item.DraftID, Role: "assistant", Kind: messageKind, Content: content,
			}},
		})
		if applyErr != nil {
			service.hub.Record(item.DraftID, StreamEvent{"type": "turn_error", "message": applyErr.Error()})
			return applyErr
		}
		if result.Status != reducer.StatusApplied {
			return fmt.Errorf("assistant message reducer status: %s", result.Status)
		}
		service.hub.Record(item.DraftID, StreamEvent{
			"type": "message_completed", "message_id": messageID,
			"kind": messageKind, "content": content,
		})
	}
	service.hub.Record(item.DraftID, StreamEvent{
		"type": "turn_ended", "outcome": outcome, "reason": reason,
	})
	return nil
}

func (service *Service) withModelRetryReporting(ctx context.Context, draftID string) context.Context {
	return withModelRetryReporter(ctx, func(notice modelRetryNotice) {
		service.hub.Record(draftID, StreamEvent{
			"type": "model_retry", "attempt": notice.Attempt,
			"max_retries": notice.MaxRetries, "reason": "模型响应超时",
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
	if interfaceString(event["event"]) != "JobSucceeded" {
		return false
	}
	payload, _ := event["payload"].(map[string]any)
	if interfaceString(payload["kind"]) != "render_preview" {
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
			"type": "text_delta", "message_id": messageID,
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
	decisionID := interfaceString(item.Payload["decision_id"])
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
	eventType := interfaceString(event["event"])
	payload, _ := event["payload"].(map[string]any)
	jobID := interfaceString(item.Payload["job_id"])
	if value := interfaceString(payload["job_id"]); value != "" {
		jobID = value
	}
	kind := interfaceString(payload["kind"])
	if kind == "" {
		kind = "后台"
	}
	succeeded := eventType == "JobSucceeded"
	terminalDetails := payload["result"]
	if !succeeded {
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
		return fmt.Sprintf("%s 任务 %s 失败：%s", kind, jobID, details), nil
	}
	if succeeded && kind == "render_preview" && service.previewAlreadyInspected(ctx, item.DraftID, terminalDetails) {
		return "", nil
	}
	status := "成功"
	nextAction := "读取真实产物；如果是预览则调用 render.inspect_preview 做质检，然后继续原任务。"
	if !succeeded {
		status = "失败"
		nextAction = "先读取失败信息并诊断；能用现有工具修复时立即修复并重试，不要把失败说成完成。"
	}
	prompt := fmt.Sprintf(
		"你等待的后台任务已到终态。\n任务：%s\njob_id：%s\n状态：%s\n终态详情：%s\n这是原任务的自动续跑，不是新的用户请求。%s 不要重复询问已经回答的问题，也不要仅回复泛化的“后台已完成”。",
		kind,
		jobID,
		status,
		details,
		nextAction,
	)
	messages, err := service.modelMessages(ctx, item.DraftID)
	if err != nil {
		return "", err
	}
	messages = append(messages, schema.UserMessage(prompt))
	return service.streamAgent(ctx, item.DraftID, messageID, messages)
}

func (service *Service) previewAlreadyInspected(ctx context.Context, draftID string, result any) bool {
	resultMap, _ := result.(map[string]any)
	previewID := interfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = interfaceString(resultMap["preview_id"])
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
			ArgsSummary string `json:"args_summary"`
			Status      string `json:"status"`
		}
		if json.Unmarshal([]byte(messages[index].Content), &record) != nil ||
			record.Tool != "render.inspect_preview" || record.Status != "succeeded" {
			continue
		}
		var args struct {
			PreviewID string `json:"preview_id"`
		}
		if json.Unmarshal([]byte(record.ArgsSummary), &args) == nil && args.PreviewID == previewID {
			return true
		}
	}
	return false
}

func decisionContinuationPrompt(decision storage.Decision, answer map[string]any) string {
	optionID := interfaceString(answer["option_id"])
	freeText := interfaceString(answer["free_text"])
	label := ""
	for _, option := range decision.Options {
		if interfaceString(option["option_id"]) == optionID {
			label = interfaceString(option["label"])
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
	if strings.Contains(content, "E2E_BLOCK_UNTIL_CANCEL") {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if strings.Contains(content, "E2E_CANCEL_UNDERSTANDING") {
		listed, err := service.toolListAssets(ctx, draftID, rushestools.AssetListInput{OnlyUsable: boolPointer(true)})
		if err != nil {
			return "", err
		}
		assetIDs := make([]string, 0, len(listed.Assets))
		for _, asset := range listed.Assets {
			assetIDs = append(assetIDs, asset.AssetID)
		}
		reporter := service.toolReporter(ctx, draftID)
		input := rushestools.UnderstandInput{AssetIDs: assetIDs, Depth: "deep", Focus: "e2e_cancel"}
		reporter("understand.materials", "started", input, nil, nil)
		output, executeErr := service.ExecuteTool(ctx, "understand.materials", input)
		reporter("understand.materials", "finished", input, output, executeErr)
		if executeErr != nil {
			return "", executeErr
		}
		return "素材理解已完成。", nil
	}
	if strings.Contains(content, "E2E_FULL_MAINLINE") || strings.Contains(content, "混剪") {
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
			Question: "请选择剪辑节奏",
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
			"type": "text_delta", "message_id": messageID,
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
	summary := deterministicContextSummary(source)
	if service.chatModel != nil {
		response, err := service.chatModel.Generate(ctx, []*schema.Message{
			schema.SystemMessage(contextCompactionPrompt),
			schema.UserMessage(source),
		}, model.WithToolChoice(schema.ToolChoiceForbidden))
		if err == nil && response != nil && strings.TrimSpace(response.Content) != "" {
			summary = truncateRunes(strings.TrimSpace(response.Content), 12000)
		}
	}
	return service.contextManager.ReplaceHistory(ctx, draftID, build, summary, through)
}

func deterministicContextSummary(source string) string {
	return "自动语义压缩不可用时保留的有界历史交接；其中状态描述可能过期，" +
		"必须以当前 WorldState 为准。\n" + truncateRunes(strings.TrimSpace(source), 8000)
}

const contextCompactionPrompt = `你是 Rushes 的上下文压缩器。禁止调用工具，只输出简体中文交接摘要。
摘要必须可替换被压缩的历史，并严格分为：
1. 当前创作目标与用户明确偏好；
2. 已确认的关键决定与约束；
3. 已完成进展（只写语义结论，不复制整条时间线）；
4. 未完成事项和下一步；
5. 仍需保留的关键 ID、错误证据或用户纠正。
不要把历史回复里的素材、时间线、响度或节拍判断写成当前事实；这些客观信息会由最新 WorldState 单独注入。删除寒暄、重复工具日志、已被用户推翻的判断和冗余过程。`

func (service *Service) toolReporter(ctx context.Context, draftID string) rushestools.Reporter {
	type activeStep struct {
		id          string
		argsSummary string
	}
	var mu sync.Mutex
	steps := map[string]activeStep{}
	return func(name, phase string, input, output any, err error) {
		mu.Lock()
		defer mu.Unlock()
		if phase == "started" {
			stepID := randomID("step")
			argsSummary := compactJSON(input)
			steps[name] = activeStep{id: stepID, argsSummary: argsSummary}
			service.hub.Record(draftID, StreamEvent{
				"type": "tool_step_started", "step_id": stepID, "tool": name,
				"args_summary": argsSummary,
			})
			return
		}
		step := steps[name]
		stepID := step.id
		if stepID == "" {
			stepID = randomID("step")
		}
		delete(steps, name)
		status := "succeeded"
		observation := compactJSON(output)
		if err != nil {
			status, observation = "failed", err.Error()
		} else if result, ok := output.(rushestools.ToolResult); ok &&
			(result.Status == "failed" || result.Status == "validation_failed") {
			status = "failed"
		}
		service.hub.Record(draftID, StreamEvent{
			"type": "tool_step_finished", "step_id": stepID, "tool": name,
			"status": status, "observation": observation,
		})
		_ = service.persistToolTrace(
			context.WithoutCancel(ctx), draftID, stepID, name, status, step.argsSummary, observation,
		)
	}
}

// 工具折叠区在刷新后仍需存在，因此完成态通过 Reducer 持久化为 system/tool 消息。
// 该消息只供 UI 回放，modelMessages 会过滤，避免工具 JSON 污染模型上下文。
func (service *Service) persistToolTrace(
	ctx context.Context,
	draftID, stepID, name, status, argsSummary, observation string,
) error {
	content, err := json.Marshal(map[string]any{
		"step_id": stepID, "tool": name, "status": status,
		"args_summary": argsSummary, "observation": observation,
	})
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
	reporter(name, "started", input, nil, nil)
	output, err := service.ExecuteTool(ctx, name, input)
	reporter(name, "finished", input, output, err)
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
		if _, err := service.executeReported(ctx, draftID, "understand.materials", rushestools.UnderstandInput{
			AssetIDs: understandIDs, Depth: "scan", Focus: "混剪可用画面",
		}); err != nil {
			return "", err
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
	optionID := interfaceString(answer["option_id"])
	if optionID == "cancel" || optionID == "no" {
		return "已取消这项操作。", nil
	}
	name, _ := pending["tool_name"].(string)
	arguments, _ := pending["arguments"].(map[string]any)
	input, err := replayInput(name, arguments)
	if err != nil {
		return "", err
	}
	output, err := service.executeReported(ctx, item.DraftID, name, input)
	if err != nil {
		return "", err
	}
	if result, ok := output.(rushestools.ToolResult); ok && result.Observation != "" {
		return result.Observation, nil
	}
	return "已按你的确认继续执行。", nil
}

func replayInput(name string, arguments map[string]any) (any, error) {
	decode := func(target any) (any, error) {
		data, err := json.Marshal(arguments)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, target); err != nil {
			return nil, err
		}
		return reflectValue(target), nil
	}
	switch name {
	case "asset.list_assets":
		return decode(&rushestools.AssetListInput{})
	case "understand.materials":
		return decode(&rushestools.UnderstandInput{})
	case "media.search_shots":
		return decode(&rushestools.ShotSearchInput{})
	case "audio.analyze_beats":
		return decode(&rushestools.AudioBeatAnalysisInput{})
	case "audio.analyze_speech_pauses":
		return decode(&rushestools.SpeechPauseAnalysisInput{})
	case "timeline.compose_initial":
		return decode(&rushestools.ComposeInitialInput{})
	case "timeline.apply_patch":
		return decode(&rushestools.TimelinePatchInput{})
	case "timeline.apply_patches":
		return decode(&rushestools.TimelinePatchBatchInput{})
	case "timeline.recut_to_beats":
		return decode(&rushestools.TimelineBeatRecutInput{})
	case "timeline.validate":
		return rushestools.TimelineValidateInput{}, nil
	case "timeline.inspect":
		return decode(&rushestools.TimelineInspectInput{})
	case "render.preview":
		return rushestools.RenderPreviewInput{}, nil
	case "render.final_mp4":
		return rushestools.RenderFinalInput{}, nil
	case "render.status":
		return rushestools.RenderStatusInput{}, nil
	case "render.inspect_preview":
		return decode(&rushestools.RenderInspectInput{})
	default:
		return nil, fmt.Errorf("无法重放未注册工具: %s", name)
	}
}

func reflectValue(value any) any {
	switch typed := value.(type) {
	case *rushestools.AssetListInput:
		return *typed
	case *rushestools.UnderstandInput:
		return *typed
	case *rushestools.ShotSearchInput:
		return *typed
	case *rushestools.AudioBeatAnalysisInput:
		return *typed
	case *rushestools.SpeechPauseAnalysisInput:
		return *typed
	case *rushestools.ComposeInitialInput:
		return *typed
	case *rushestools.TimelinePatchInput:
		return *typed
	case *rushestools.TimelinePatchBatchInput:
		return *typed
	case *rushestools.TimelineBeatRecutInput:
		return *typed
	case *rushestools.TimelineInspectInput:
		return *typed
	case *rushestools.RenderInspectInput:
		return *typed
	default:
		return value
	}
}

func interfaceString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case *string:
		if typed != nil {
			return *typed
		}
	}
	return ""
}

func randomID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(data)
}

func boolPointer(value bool) *bool { return &value }

const systemPrompt = `你是 Rushes 本地视频剪辑 Agent，负责真正修改当前草稿，而不是只给建议。

上下文协议：系统规则定义能力和安全边界；最新用户消息定义当前创作意图并可纠正旧结论；WorldState 参考快照应用随后 RFC 7396 增量后，才是素材、时间线、任务和错误的唯一客观事实；压缩交接与历史回复仅用于延续目标和决定，绝不能覆盖 WorldState。

工作规则：
1. 只使用已注册工具推进“理解素材、编辑时间线、验证、预览、质检、导出”；不要编造文件、素材、时间线、job 或渲染结果。
2. 用户目标明确时直接执行，不要反复提问。只有缺少会显著改变成片的阻塞性选择时才调用 interaction.ask_user；用户回答后必须自动继续原任务。
3. 先用 asset.list_assets 读取 asset_id、kind、rel_dir、duration_frames、timeline_fps、suggested_role 和 suggested_visual_role。video/image 才能进入主视觉；audio 按 suggested_role 放入 bgm 或 sfx 轨。口播任务中优先使用用户目录与理解结果给出的 a_roll/b_roll 角色；不确定时调用 understand.materials，不要只凭泛化常识猜。BGM 和 SFX 必须保持不同轨道：BGM 承载连续音乐，SFX 是叠加在 BGM 上的短时点缀，不得默认接在 BGM 结束后替代音乐。
4. 每回合常驻的 material_catalog 是完整精简素材目录，不是全部镜头明细。用户要求卡点、混剪、跟拍或节奏剪辑时，先对 BGM 调用 audio.analyze_beats；它返回拍点和按时间顺序的压缩波形。waveform.sample_frames[i] 是第 i 个 RMS 窗口的时间线起始帧，waveform.samples[i] 是该窗口的 0–100 原始响度值；你应结合用户意图自主判断音乐动态，不得把 beat/strong/downbeat 自动等同于高潮或好剪辑，也不得沿用先前回复声称的高潮边界代替本轮原始波形。缺少可检索镜头的素材先调用 understand.materials（默认复用缓存，除非用户明确要求不得 force_refresh），再根据创作意图调用 media.search_shots 按需检索镜头，把选中的有序 shot_ids 和真实节拍 cut_frames 交给 timeline.recut_to_beats。不要只按 asset_id 或素材开头盲选。严格区分 analysis_window 与真实切镜：只有 boundary_kind=visual_cut 且 boundary_verified=true 才能称为已验证切镜。cut_frames 可以多于素材数；同一素材可使用多个不重叠镜头。use_all_video_assets=true 表示每个素材至少出现一次，不是恰好一次。cover_entire_bgm=true 覆盖整首音乐；SFX 必须独立分轨。失败时根据 observation 修正同一高层调用，不得降级成 compose_initial 或几十个低层补丁。
5. 需要读取当前时间线时调用 timeline.inspect 取得真实 clip/track 明细，不能猜 timeline_clip_id；草稿尚无时间线时该工具会安全返回 timeline_exists=false 和空轨道，此时再通过 asset.list_assets 与相应理解/检索工具选定素材，用 compose_initial 或对应高层创建工具建立时间线。已有 trim_clip、split_clip、trim_clip_edge、move_clip、reorder_clip、set_clip_fades 和 compose_initial 可完成一般素材裁剪、重组与音频淡入淡出。trim_clip_edge 的位置字段是 timeline_frame；move_clip/reorder_clip 才使用 target_frame。非卡点场景需要修改 2 个及以上 clip 时，必须用 timeline.apply_patches 原子批量提交；整批替换主视频时，把新主视频 insert_clip 和旧主视频 delete_clip 放在同一次调用，工具会安全重排并保护未被直接编辑的 BGM/SFX。不要先单独删空主视频轨，也不要逐个调用 timeline.apply_patch 浪费步数。
6. 时间线唯一精确坐标是整数帧。所有 compose 和 patch 参数都使用 *_frame，绝不使用 *_s 秒字段。补丁 op 必须是包含 kind 的扁平对象。
7. 每次修改时间线后调用 timeline.validate，并读取 data.beat_alignment：结构校验通过只代表时间线无缺口/重叠，不等于已经卡点；beat_grid_present=false 时不得向用户宣称卡点成功，应重新调用 timeline.recut_to_beats 修复。浏览器会用编辑代理自动即时预览，不要为每次移动、裁剪或分割调用 render.preview；只有用户明确要求生成可分享预览或需要离线画质质检时才调用 render.preview，并在后台成功后调用 render.inspect_preview。最终导出使用 render.final_mp4，它会读取原素材而不是编辑代理。
8. 口播任务在 WorldState 已有时间线时先用 timeline.inspect 确认 A-roll 主视频片段；没有时间线时先从 asset.list_assets 选择 A-roll 并 compose_initial，随后再调用 speech.inspect。该工具会持久化逐句 ASR/SRT、气口和相似台词证据；可像 grep 一样用 query 或源帧范围继续深入读取。初次全片读取最多请求 24 个 pause（默认值）；它们已在全片范围按可安全删除时长排序，并非时间轴前 24 个，因此不要为了“看完整”把 max_pauses 提高到 100。只有需要检查较短停顿时才按源帧窗口继续读取。必须结合 previous_context、next_context、joined_context 自主审阅靠前的长气口；similar_pairs 既可能是单句对，也可能用 earlier/later 的起止 utterance_id 表示连续台词块；intra_utterance_repetitions 显示同一句内的相邻重复词或两个不重叠重复短语；short_speech_fragments 是内部停顿附近必须明确处理的语音片段。其中 kind=restart_prefix_before_repeated_take 时，fragment.text 是重新接入此前台词前的未对齐前缀；kind=earlier_take_before_repeated_phrase_restart 时，fragment.text 覆盖较早共同短语、随后分叉尾部直到重启停顿，是一遍完整的较早说法候选，不能只删尾部却留下前半句。restart_anchor_text 与 matched_earlier_text 给出两次说法的对应关系。它们都只是客观文本证据，必须结合两侧原文自主决定删除或有理由地保留。发现一句内有卡壳、重复词、半句重说或需要精确剪口时，必须对该源帧窗口再次调用 speech.inspect(include_words=true)，读取词级 word_id 后再决定连续 remove_word_ranges，不能因为整句粒度不够就保留明显错误或删除整句。常驻 material_catalog 只保存索引状态，不塞完整转写。你必须阅读台词后自行判断口误、语义重复和应保留表达；similarity 与 pause 都只是证据。需要配画面时，围绕具体台词语义调用 media.search_shots，并设置 semantic_roles=["b_roll"]；若 understanding_candidates 中出现文件名更直接匹配台词的未理解素材，先用其 asset_id 调用 understand.materials，再以同一语义重搜，不要从较弱的旧候选中硬选。B-roll 镜头短于整句时，不要硬覆盖整句或放弃正确素材；优先用已看到的逐句转写在对应 start/end_utterance_id 内原样摘录唯一的连续短语 anchor_text，工具会确定性解析词级帧范围；若短语不唯一或需要进一步看词边界，再调用 speech.inspect(include_words=true) 并改用 start/end_word_id。最后把选定的 utterance_id/anchor_text 或 word_id 范围、pause_decisions、repetition_decisions、short_fragment_decisions、shot_id 及语义覆盖范围一次传给 timeline.edit_talking_head；它只做稳定 ID/原文短语解析、精确帧映射、原声联动、B-roll 独立叠加和合法性校验。audio.analyze_speech_pauses 仅用于不需要 ASR 的轻量静音扫描。
8a. preserve_speech_fragment_ids 不是绕过口播证据校验的默认值。保留 kind=restart_prefix_before_repeated_take 或 earlier_take_before_repeated_phrase_restart 的候选时，先把 previous_context、joined_context、matched_earlier_text 连起来读；必须同时提供 preserve_speech_fragment_reasons，至少 20 字且原样引用 fragment.text 与 restart_anchor_text，明确解释拼接后为何语法和语义完整；不能只写“正常”“衔接”或“保留”。不确定时继续用词级 speech.inspect 获取上下文。
8b. speech.inspect 的宽范围结果会把 intra_utterance_repetitions、short_speech_fragments 和 pauses 放在完整 utterances 之前，作为可继续按源帧深入检索的客观索引；不要因为长转写在后面就漏掉前面的重复证据。timeline.edit_talking_head 成功后，最终回复中的删除数量只能引用该次成功结果的 effective removed_*_count；尤其气口按 removed_pause_range_count 统计独立生效区间，不能按提交的 pause_id 数量或失败调用累计。
8c. intra_utterance_repetitions 已自带 repetition_id 和前后两段精确 word_id，会把 adjacent_word_repeat 优先放在最前面；这些证据中“压压”可能是卡壳，“11”可能是数字拆词，“跷跷”可能是正常叠词，必须结合 context_text 自主判断，不能按规则自动删除，也不能漏读。首次调用 timeline.edit_talking_head 时，对每个候选一次性提交 repetition_decisions，action 只能是 remove_earlier、remove_later 或 preserve；只有需要更宽上下文时才继续 speech.inspect，不要为了取得这些已有 word_id 重复查询。对 short_speech_fragments 中每个候选也一次性提交 short_fragment_decisions，action 只能是 remove 或 preserve。两类 remove 都会由工具解析候选自带的精确词范围。
8e. 只要本轮包含口播删剪，就必须对 speech.inspect 中可见的显著气口一次性提交 pause_decisions，action 只能是 remove 或 preserve，并结合 previous_context、next_context、joined_context 自主说明。工具只要求每个显著候选有明确决定并校验删除后不会留下孤立语音；它不会按时长自动判断内容。不要在一次安全校验失败后把所有 pause_id 撤回并宣布完成。
	8d. 提交口播编辑前交叉检查：B-roll 的 utterance/anchor_text 或 word_id 必须属于本次保留的台词，不能同时出现在 remove_utterance_ids、remove_word_ranges 或 remove 决定中。对用户逐项点名的 B-roll 主题，先做一张内部对照：原始主题、同词检索 query、选中镜头 filename/description、原文 anchor_text 必须表达同一概念；保留主题中的限定词，不能把“键盘背光”偷换成“键盘/键帽/同色系”等相邻概念。每个 B-roll 锚点至少覆盖 15 帧；不足时在同一保留语义内选择更完整的连续短语，不能扩到无关整句硬凑时长。用户明确要求的每个 B-roll 主题都必须保留在同一次高层调用里；高层调用失败时修正返回的具体关系，禁止删除 B-roll 参数后把删词成功伪装成任务完成，也禁止退化成 timeline.apply_patch 猜 timeline_start_frame。若高层结果的 b_roll_assignment_count 少于用户要求，继续修复而不是宣布完成。
	8e. timeline.edit_talking_head 成功结果里的 unreviewed_*_candidates 是未处理的客观内容证据，不是失败。用户要求全量剪口播时应继续自主审阅这些候选；用户明确只改一个气口、卡壳或 B-roll 窗口时，不要为了目标外候选补齐无关 preserve 决定。工具只用稳定 ID、范围、孤立碎片和时间线不变量约束合法性，不替你判断内容好坏。
9. 工具返回 failed、validation_failed 或 harness_recovery 时，先阅读 observation、data.recovery 与 data.harness_recovery，再修改参数或调用 inspect/list 获取新证据。绝不能原样重复同一工具名和参数，也不得通过删掉用户明确要求的功能参数绕过失败；harness_recovery.exhausted=true 时必须停止工具调用，明确回复用户本轮未完成、最后错误和可继续的下一步。
10. 每回合开头的【WorldState 参考快照】与可选【WorldState 当前增量】共同组成唯一客观状态；recent_edit_history 只解释近期编辑意图，不能覆盖当前时间线。用户后续反馈可以否定先前的节奏或镜头判断；此时重新读取最新工具证据，不复用先前回复中的结论。接受反馈时从当前状态继续，不要从头重做；除非用户明确要求，不删除既有素材、时间线或已完成理解。`

var _ rushestools.Executor = (*Service)(nil)
