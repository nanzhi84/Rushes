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

type Service struct {
	database *storage.DB
	hub      *TurnStreamHub
	queue    *TurnQueue
	tools    *rushestools.Registry
	react    *react.Agent
	analyzer *understanding.Analyzer
	cancel   context.CancelFunc
	bridgeWG sync.WaitGroup
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
	service := &Service{
		database: database, hub: NewTurnStreamHub(0), cancel: cancel,
		analyzer: understanding.NewAnalyzer(visionModel),
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
			},
			MaxStep:               12,
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

func (service *Service) UsesEino() bool { return service.react != nil }

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
	ctx = rushestools.WithReporter(ctx, service.toolReporter(item.DraftID))
	content, err := service.turnContent(ctx, item, messageID)
	if errors.Is(err, context.Canceled) {
		service.hub.Record(item.DraftID, StreamEvent{
			"type": "turn_ended", "outcome": "cancelled", "reason": "user_cancelled",
		})
		return err
	}
	if err != nil {
		service.hub.Record(item.DraftID, StreamEvent{"type": "turn_error", "message": err.Error()})
		return err
	}
	if content != "" {
		result, applyErr := reducer.Apply(ctx, service.database, nil, reducer.Options{
			Actor: contracts.ActorAgent,
			ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
				ID: messageID, DraftID: item.DraftID, Role: "assistant", Kind: "reply", Content: content,
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
			"kind": "reply", "content": content,
		})
	}
	service.hub.Record(item.DraftID, StreamEvent{
		"type": "turn_ended", "outcome": "finished", "reason": nil,
	})
	return nil
}

func (service *Service) turnContent(ctx context.Context, item QueueItem, messageID string) (string, error) {
	if item.Kind == QueueJobObservation {
		return "后台任务已完成，我已读取结果并继续推进。", nil
	}
	if item.Kind == QueueUIObservation {
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
		service.hub.Record(item.DraftID, StreamEvent{
			"type": "text_delta", "message_id": messageID,
			"kind": "assistant", "delta": message.Content,
		})
	}
	return output.String(), nil
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
		reporter := service.toolReporter(draftID)
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
	rows, err := storage.ListMessages(ctx, service.database.Read(), draftID, 200)
	if err != nil {
		return nil, err
	}
	messages := make([]*schema.Message, 0, len(rows))
	for _, row := range rows {
		if row.Role == "assistant" {
			messages = append(messages, schema.AssistantMessage(row.Content, nil))
		} else {
			messages = append(messages, schema.UserMessage(row.Content))
		}
	}
	return messages, nil
}

func (service *Service) toolReporter(draftID string) rushestools.Reporter {
	var mu sync.Mutex
	steps := map[string]string{}
	return func(name, phase string, input, output any, err error) {
		mu.Lock()
		defer mu.Unlock()
		if phase == "started" {
			stepID := randomID("step")
			steps[name] = stepID
			service.hub.Record(draftID, StreamEvent{
				"type": "tool_step_started", "step_id": stepID, "tool": name,
				"args_summary": compactJSON(input),
			})
			return
		}
		stepID := steps[name]
		if stepID == "" {
			stepID = randomID("step")
		}
		delete(steps, name)
		status := "succeeded"
		observation := compactJSON(output)
		if err != nil {
			status, observation = "failed", err.Error()
		}
		service.hub.Record(draftID, StreamEvent{
			"type": "tool_step_finished", "step_id": stepID, "tool": name,
			"status": status, "observation": observation,
		})
	}
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
	reporter := service.toolReporter(draftID)
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
	if len(listed.Assets) == 0 {
		return "当前草稿还没有可用素材，请先导入素材。", nil
	}
	understandIDs := []string{}
	for _, asset := range listed.Assets {
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
	clips := make([]rushestools.ComposeClip, 0, len(listed.Assets))
	for _, asset := range listed.Assets {
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
	case "timeline.compose_initial":
		return decode(&rushestools.ComposeInitialInput{})
	case "timeline.apply_patch":
		return decode(&rushestools.TimelinePatchInput{})
	case "timeline.validate":
		return rushestools.TimelineValidateInput{}, nil
	case "timeline.inspect":
		return decode(&rushestools.TimelineInspectInput{})
	case "timeline.restore_version":
		return decode(&rushestools.TimelineRestoreInput{})
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
	case *rushestools.ComposeInitialInput:
		return *typed
	case *rushestools.TimelinePatchInput:
		return *typed
	case *rushestools.TimelineInspectInput:
		return *typed
	case *rushestools.TimelineRestoreInput:
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

const systemPrompt = `你是 Rushes 本地视频剪辑 Agent。只使用已注册工具推进“导入、理解、时间线、预览、导出”主线；需要用户选择时调用 interaction.ask_user；不要编造文件、素材、时间线或渲染结果。时间线的唯一精确坐标是整数帧：先从 asset.list_assets 读取 duration_frames 与 timeline_fps，所有 compose 和 patch 参数使用 *_frame，绝不使用 *_s 秒字段。`

var _ rushestools.Executor = (*Service)(nil)
