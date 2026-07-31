package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agent"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
	"github.com/nanzhi84/Rushes/go/internal/worker"
)

const (
	firstVisionMarker  = "VISION_RESULT_7F3A"
	secondVisionMarker = "VISION_RESULT_9C2D"
)

type understandBridgeChatModel struct {
	mu                sync.Mutex
	toolBound         bool
	toolCalls         int
	modelCalls        int
	sawTerminalResult bool
	toolResult        string
}

func (modelValue *understandBridgeChatModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	for _, info := range infos {
		if info.Name == "media.detect_shots" {
			modelValue.toolBound = true
			break
		}
	}
	return modelValue, nil
}

func (modelValue *understandBridgeChatModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.modelCalls++

	if toolResult := lastMessageContent(messages, schema.Tool); toolResult != "" {
		if !strings.Contains(toolResult, `"status":"succeeded"`) ||
			!strings.Contains(toolResult, firstVisionMarker) {
			return nil, errors.New("单素材检测没有在当前 ReAct turn 返回终态持久化摘要")
		}
		modelValue.sawTerminalResult = true
		modelValue.toolResult = toolResult
		return schema.AssistantMessage("已依据真实素材理解继续处理："+firstVisionMarker+"。", nil), nil
	}

	if !modelValue.toolBound {
		return nil, errors.New("media.detect_shots 未绑定到 ReAct 模型")
	}
	modelValue.toolCalls++
	if modelValue.toolCalls != 1 {
		return nil, errors.New("初始回合重复调用 media.detect_shots")
	}
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: "call_detect_shots",
		Function: schema.FunctionCall{
			Name:      "media.detect_shots",
			Arguments: `{"asset_id":"asset_visual_one","depth":"deep","focus":"主体与构图"}`,
		},
	}}), nil
}

func (modelValue *understandBridgeChatModel) Stream(
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

func (modelValue *understandBridgeChatModel) snapshot() (
	toolCalls int,
	modelCalls int,
	sawTerminal bool,
	toolResult string,
) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	return modelValue.toolCalls, modelValue.modelCalls, modelValue.sawTerminalResult, modelValue.toolResult
}

type markerVisionModel struct {
	mu    sync.Mutex
	calls int
}

func (modelValue *markerVisionModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (modelValue *markerVisionModel) Generate(
	_ context.Context,
	_ []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	switch modelValue.calls {
	case 1:
		return schema.AssistantMessage(
			`{"overall":"第一份素材的独特视觉结论：`+firstVisionMarker+`",`+
				`"semantic_role":"b_roll","segments":[{"id":"s000",`+
				`"description":"蓝色画面，`+firstVisionMarker+`","tags":["蓝色"],`+
				`"quality":"usable","subjects":[],"actions":[],"setting":[],"shot_scale":"全景",`+
				`"composition":"纯色","lighting":[],"mood":[],"edit_hints":[]}]}`,
			nil,
		), nil
	case 2:
		return schema.AssistantMessage(
			`{"overall":"第二份素材的独特视觉结论：`+secondVisionMarker+`",`+
				`"semantic_role":"b_roll","segments":[{"id":"s000",`+
				`"description":"红色画面，`+secondVisionMarker+`","tags":["红色"],`+
				`"quality":"usable","subjects":[],"actions":[],"setting":[],"shot_scale":"全景",`+
				`"composition":"纯色","lighting":[],"mood":[],"edit_hints":[]}]}`,
			nil,
		), nil
	default:
		return nil, errors.New("VLM 收到超出双素材范围的额外调用")
	}
}

func (modelValue *markerVisionModel) Stream(
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

func (modelValue *markerVisionModel) callCount() int {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	return modelValue.calls
}

func TestAsyncUnderstandWorkerCompletesInSameTurn(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	firstPath := filepath.Join(database.Paths.Temporary, "source-one.mp4")
	secondPath := filepath.Join(database.Paths.Temporary, "source-two.mp4")
	assertMarkersAbsent(t, "素材文件名", firstPath+"\n"+secondPath)
	writeVideo(t, firstPath, "blue")
	writeVideo(t, secondPath, "red")
	createUnderstandFixture(t, database, firstPath, secondPath)

	userContent := "请深度检测第一份视频素材，并根据真实视觉结论继续当前任务。"
	assertMarkersAbsent(t, "用户请求", userContent)
	persistMessage(t, database, "draft_understand_bridge", "user_understand_bridge", "user", "user", userContent)

	visionModel := &markerVisionModel{}
	registry := worker.NewRegistry()
	if err := worker.RegisterUnderstand(
		registry,
		database,
		understanding.NewAnalyzer(visionModel),
	); err != nil {
		t.Fatal(err)
	}
	runner, err := worker.NewRunner(worker.RunnerConfig{
		Database: database,
		Registry: registry,
		WorkerID: "integration_understand_worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	t.Cleanup(cancelWorker)
	workerDone := make(chan error, 1)
	go func() {
		for {
			worked, runErr := runner.RunOnce(workerCtx)
			if runErr != nil {
				workerDone <- runErr
				return
			}
			if worked {
				workerDone <- nil
				return
			}
			select {
			case <-workerCtx.Done():
				workerDone <- workerCtx.Err()
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	chatModel := &understandBridgeChatModel{}
	service, err := agent.NewService(t.Context(), database, chatModel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if !service.Queue().EnqueueUserMessage(
		"draft_understand_bridge", "user_understand_bridge", userContent,
	) {
		t.Fatal("初始用户回合未入队")
	}
	service.Queue().JoinDraft("draft_understand_bridge")
	select {
	case runErr := <-workerDone:
		if runErr != nil {
			t.Fatalf("understand worker: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("understand worker 未完成")
	}

	var jobID, status, payloadJSON, resultJSON string
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT job_id,status,payload_json,result_json FROM jobs
		WHERE kind='understand' AND requested_by_draft_id='draft_understand_bridge'`,
	).Scan(&jobID, &status, &payloadJSON, &resultJSON); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || jobID == "" || !strings.Contains(resultJSON, `"status":"succeeded"`) {
		t.Fatalf("understand job 未成功: job_id=%q status=%q result=%s", jobID, status, resultJSON)
	}
	for _, fragment := range []string{"asset_visual_one", `"depth":"deep"`} {
		if !strings.Contains(payloadJSON, fragment) {
			t.Fatalf("understand job payload 缺少 %q: %s", fragment, payloadJSON)
		}
	}
	assertMarkersAbsent(t, "jobs.payload_json", payloadJSON)
	assertMarkersAbsent(t, "jobs.result_json", resultJSON)
	if visionModel.callCount() != 1 {
		t.Fatalf("VLM calls=%d want=1", visionModel.callCount())
	}
	assertStoredMarkerSummary(t, database, "asset_visual_one", firstVisionMarker, secondVisionMarker)

	toolCalls, modelCalls, sawTerminal, toolResult := chatModel.snapshot()
	if toolCalls != 1 || modelCalls != 2 || !sawTerminal {
		t.Fatalf("same-turn ReAct 异常: tool_calls=%d model_calls=%d terminal=%v", toolCalls, modelCalls, sawTerminal)
	}
	if strings.Contains(toolResult, `"status":"queued"`) ||
		strings.Contains(toolResult, "自动续跑") ||
		!strings.Contains(toolResult, firstVisionMarker) {
		t.Fatalf("工具回灌不是同 turn 终态摘要: %s", toolResult)
	}
	finalReply := waitForMarkerReply(t, database, "draft_understand_bridge", 2*time.Second)
	if !strings.Contains(finalReply, firstVisionMarker) {
		t.Fatalf("最终回复未引用持久化摘要: %q", finalReply)
	}
	var observations int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_job_observations",
	).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("same-turn 不得创建 synthetic observation: count=%d err=%v", observations, err)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), "draft_understand_bridge", 100)
	if err != nil {
		t.Fatal(err)
	}
	markerReplies := 0
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "你等待的后台任务已到终态") {
			t.Fatalf("出现 synthetic user prompt: %#v", message)
		}
		if message.Role == "assistant" && strings.Contains(message.Content, firstVisionMarker) {
			markerReplies++
		}
	}
	if markerReplies != 1 {
		t.Fatalf("最终 marker 回复持久化次数=%d want=1; messages=%#v", markerReplies, messages)
	}
}

func createUnderstandFixture(t *testing.T, database *storage.DB, firstPath, secondPath string) {
	t.Helper()
	events := []contracts.Event{{
		Type: "DraftCreated", DraftID: "draft_understand_bridge",
		Payload: map[string]any{"name": "异步素材理解集成测试"},
	}}
	for index, fixture := range []struct {
		assetID string
		path    string
		name    string
		hash    string
	}{
		{assetID: "asset_visual_one", path: firstPath, name: "source-one.mp4", hash: "video-hash-one"},
		{assetID: "asset_visual_two", path: secondPath, name: "source-two.mp4", hash: "video-hash-two"},
	} {
		info, err := os.Stat(fixture.path)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": fixture.assetID, "job_id": "fixture_import_" + fixture.assetID,
				"storage_mode": "reference", "reference_path": fixture.path,
				"kind": "video", "source": "local_path", "filename": fixture.name,
				"hash": fixture.hash, "mtime": info.ModTime().UnixNano(), "size": info.Size(),
				"ingest_status": "ready", "usable": true,
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_understand_bridge", Payload: map[string]any{
				"asset_id": fixture.assetID, "note": index,
			}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("fixture reducer status=%s err=%v", result.Status, err)
	}
}

func persistMessage(
	t *testing.T,
	database *storage.DB,
	draftID, messageID, role, kind, content string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, nil, reducer.Options{
		Actor: contracts.ActorUser,
		ResultRows: reducer.ResultRows{Message: &reducer.MessageRow{
			ID: messageID, DraftID: draftID, Role: role, Kind: kind, Content: content,
		}},
	})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("message reducer status=%s err=%v", result.Status, err)
	}
}

func writeVideo(t *testing.T, path, fill string) {
	t.Helper()
	if _, err := media.RunCommand(
		t.Context(), "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c="+fill+":s=64x64:r=5:d=0.4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path,
	); err != nil {
		t.Fatal(err)
	}
}

func waitForMarkerReply(
	t *testing.T,
	database *storage.DB,
	draftID string,
	timeout time.Duration,
) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Role == "assistant" && strings.Contains(message.Content, firstVisionMarker) {
				return message.Content
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("等待 understand JobSucceeded 桥接续跑超时")
	return ""
}

func assertStoredMarkerSummary(
	t *testing.T,
	database *storage.DB,
	assetID, expectedMarker, otherMarker string,
) {
	t.Helper()
	summary, err := storage.BestMaterialSummary(t.Context(), database.Read(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	overall, _ := summary["overall"].(string)
	if !strings.Contains(overall, expectedMarker) || strings.Contains(overall, otherMarker) {
		t.Fatalf("素材 %s 的持久化摘要未保持独特 marker: %q", assetID, overall)
	}
}

func lastMessageContent(messages []*schema.Message, role schema.RoleType) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == role {
			return messages[index].Content
		}
	}
	return ""
}

func assertMarkersAbsent(t *testing.T, label, value string) {
	t.Helper()
	for _, marker := range []string{firstVisionMarker, secondVisionMarker} {
		if strings.Contains(value, marker) {
			t.Fatalf("%s 不应包含 VLM marker %q: %s", label, marker, value)
		}
	}
}
