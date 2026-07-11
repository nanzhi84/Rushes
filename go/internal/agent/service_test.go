package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
)

type serviceToolModel struct {
	mu    sync.Mutex
	calls int
	tools []*schema.ToolInfo
}

type failingServiceModel struct{}

func (modelValue *failingServiceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return modelValue, nil
}

func (*failingServiceModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("model failed")
}

func (*failingServiceModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("model failed")
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
	if !service.UsesEino() {
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
	if err != nil || len(messages) != 2 || messages[1].Content != "EINO-SERVICE-OK" {
		t.Fatalf("messages=%#v err=%v", messages, err)
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
	_, stream, unsubscribe := service.Hub().Subscribe("draft_cancel")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_cancel", "msg", "E2E_BLOCK_UNTIL_CANCEL")
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
				if event["content"] != "后台任务已完成，我已读取结果并继续推进。" {
					t.Fatalf("event=%#v", event)
				}
				return
			}
		case <-deadline:
			t.Fatal("job observation 未唤醒 Agent")
		}
	}
}

func TestUnderstandingMiniLoopCancellationKeepsCompletedSummaryAndResetsPendingAsset(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_understand_cancel")
	source := filepath.Join(database.Paths.Temporary, "understand-cancel.mp4")
	if _, err := media.RunCommand(t.Context(), "ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", source); err != nil {
		t.Fatal(err)
	}
	events := []contracts.Event{}
	for index, assetID := range []string{"ready_asset", "slow_asset"} {
		events = append(events,
			contracts.Event{Type: "AssetImported", Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_asset_" + assetID,
				"storage_mode": "reference", "reference_path": source, "kind": "video",
				"source": "local_path", "filename": assetID + ".mp4", "hash": assetID,
				"size": index + 1, "probe": map[string]any{"duration_sec": 1}, "ingest_status": "ready",
			}},
			contracts.Event{Type: "AssetLinked", DraftID: "draft_understand_cancel", Payload: map[string]any{"asset_id": assetID}},
		)
	}
	result, err := reducer.Apply(t.Context(), database, events, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("assets status=%s err=%v", result.Status, err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, stream, unsubscribe := service.Hub().Subscribe("draft_understand_cancel")
	defer unsubscribe()
	service.Queue().EnqueueUserMessage("draft_understand_cancel", "message", "E2E_CANCEL_UNDERSTANDING")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-stream:
			completed, _ := event["completed"].(int)
			if event["type"] == "subagent_progress" && completed == 1 {
				if !service.Queue().RequestStop("draft_understand_cancel") {
					t.Fatal("理解进行中取消失败")
				}
				service.Queue().JoinDraft("draft_understand_cancel")
				ready, _ := storage.GetAsset(t.Context(), database.Read(), "ready_asset")
				slow, _ := storage.GetAsset(t.Context(), database.Read(), "slow_asset")
				if ready.UnderstandingStatus != "ready" || slow.UnderstandingStatus != "none" {
					t.Fatalf("ready=%s slow=%s", ready.UnderstandingStatus, slow.UnderstandingStatus)
				}
				if _, err := storage.LatestMaterialSummary(t.Context(), database.Read(), "ready_asset"); err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatal("等待理解 1/2 超时")
		}
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
	for _, focus := range []string{"首次", "更深入"} {
		if _, err := service.ExecuteTool(ctx, "understand.materials", rushestools.UnderstandInput{
			AssetIDs: []string{"asset_repeat"}, Focus: focus,
		}); err != nil {
			t.Fatalf("focus=%s err=%v", focus, err)
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
			AssetID: "asset_timeline", SourceStart: 0, SourceEnd: 2, Role: "a_roll",
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
	inspected, err := service.ExecuteTool(ctx, "timeline.inspect", rushestools.TimelineInspectInput{})
	if err != nil || inspected.(rushestools.ToolResult).Observation == "" {
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
	var renderJobs int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM jobs WHERE kind='render_preview' AND status='pending'").Scan(&renderJobs); err != nil || renderJobs != 1 {
		t.Fatalf("render jobs=%d err=%v", renderJobs, err)
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE jobs SET status='succeeded' WHERE job_id=?", firstRender.Data["job_id"]); err != nil {
		t.Fatal(err)
	}
	completedRenderRaw, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	completedRender := completedRenderRaw.(rushestools.ToolResult)
	if completedRender.Status != "succeeded" || completedRender.Data["job_id"] != firstRender.Data["job_id"] {
		t.Fatalf("completed render idempotency=%#v", completedRender)
	}
	if _, err := service.ExecuteTool(ctx, "timeline.restore_version", rushestools.TimelineRestoreInput{SourceVersion: 1}); err != nil {
		t.Fatal(err)
	}
	draft, _ = storage.GetDraft(t.Context(), database.Read(), "draft_timeline_tools")
	if draft.TimelineCurrentVersion == nil || *draft.TimelineCurrentVersion != 1 || draft.TimelineValidated {
		t.Fatalf("draft after restore=%#v", draft)
	}
	if _, err := service.ExecuteTool(ctx, "render.preview", rushestools.RenderPreviewInput{}); err == nil {
		t.Fatal("未验证时间线不应进入渲染队列")
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
	if service.Tools() == nil {
		t.Fatal("registry missing")
	}
	if _, err := service.ExecuteTool(ctx, "timeline.validate", rushestools.TimelineValidateInput{}); err != nil {
		t.Fatal(err)
	}
	if inspected, err := service.ExecuteTool(ctx, "timeline.inspect", rushestools.TimelineInspectInput{Version: 1}); err != nil || inspected.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	status, err := service.ExecuteTool(ctx, "render.status", rushestools.RenderStatusInput{})
	if err != nil || status.(rushestools.ToolResult).Data["running_jobs"] == nil {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	allowFreeText, blocking := false, false
	waiting, err := service.ExecuteTool(ctx, "interaction.ask_user", rushestools.AskUserInput{
		Question: "继续？", Options: []rushestools.DecisionOptionInput{{OptionID: "yes", Label: "继续"}},
		AllowFreeText: &allowFreeText, Blocking: &blocking,
	})
	if err != nil || waiting.(rushestools.ToolResult).Status != "waiting" {
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
		PreviewID: "preview_inspect", Checks: []string{"decode", "duration", "resolution"},
	})
	if err != nil || preview.(rushestools.PreviewInspectionResult).Summary == "" {
		t.Fatalf("preview=%#v err=%v", preview, err)
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
	for _, name := range []string{
		"asset.list_assets", "understand.materials", "timeline.compose_initial", "timeline.apply_patch",
		"timeline.validate", "timeline.inspect", "timeline.restore_version", "render.preview",
		"render.final_mp4", "render.status", "render.inspect_preview",
	} {
		if _, err := replayInput(name, map[string]any{}); err != nil && name != "understand.materials" {
			t.Fatalf("replay %s: %v", name, err)
		}
	}
	if _, err := replayInput("missing", map[string]any{}); err == nil {
		t.Fatal("unknown replay should fail")
	}
	for _, value := range []any{
		&rushestools.AssetListInput{}, &rushestools.UnderstandInput{}, &rushestools.ComposeInitialInput{},
		&rushestools.TimelinePatchInput{}, &rushestools.TimelineInspectInput{}, &rushestools.TimelineRestoreInput{},
		&rushestools.RenderInspectInput{}, "unchanged",
	} {
		_ = reflectValue(value)
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
		"understand.materials":     rushestools.UnderstandInput{},
		"timeline.apply_patch":     rushestools.TimelinePatchInput{},
		"timeline.validate":        rushestools.TimelineValidateInput{},
		"timeline.inspect":         rushestools.TimelineInspectInput{},
		"timeline.restore_version": rushestools.TimelineRestoreInput{SourceVersion: 99},
		"render.preview":           rushestools.RenderPreviewInput{},
		"render.final_mp4":         rushestools.RenderFinalInput{},
		"decision.answer":          rushestools.DecisionAnswerInput{DecisionID: "missing"},
	} {
		if _, err := service.ExecuteTool(ctx, name, input); err == nil {
			t.Fatalf("%s should fail", name)
		}
	}
	invalid := timeline.Empty("draft_failures", 1)
	invalid.FPS = 0
	invalid.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "bad", TrackID: "visual_base", AssetID: "a", TimelineEnd: 1, SourceEnd: 1,
	}}
	result, err := service.persistTimeline(ctx, "draft_failures", invalid, nil, "invalid")
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
}

func TestModelFailureEmitsTurnError(t *testing.T) {
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
	for {
		select {
		case event := <-stream:
			if event["type"] == "turn_error" {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("turn_error missing")
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
		"asset.list_assets":        rushestools.AssetListInput{},
		"understand.materials":     rushestools.UnderstandInput{AssetIDs: []string{"asset"}},
		"interaction.ask_user":     rushestools.AskUserInput{Question: "?"},
		"decision.answer":          rushestools.DecisionAnswerInput{DecisionID: "decision"},
		"timeline.compose_initial": rushestools.ComposeInitialInput{},
		"timeline.apply_patch":     rushestools.TimelinePatchInput{Op: map[string]any{"kind": "noop"}},
		"timeline.validate":        rushestools.TimelineValidateInput{},
		"timeline.inspect":         rushestools.TimelineInspectInput{},
		"timeline.restore_version": rushestools.TimelineRestoreInput{SourceVersion: 1},
		"render.preview":           rushestools.RenderPreviewInput{},
		"render.final_mp4":         rushestools.RenderFinalInput{},
		"render.status":            rushestools.RenderStatusInput{},
		"render.inspect_preview":   rushestools.RenderInspectInput{PreviewID: "preview"},
	} {
		if _, err := service.ExecuteTool(ctx, name, input); err == nil {
			t.Fatalf("closed database: %s 应失败", name)
		}
	}
	if _, _, err := service.findRenderJob(t.Context(), "render_preview", "closed"); err == nil {
		t.Fatal("closed findRenderJob 应失败")
	}
	if _, err := service.modelMessages(ctx, "draft_closed"); err == nil {
		t.Fatal("closed modelMessages 应失败")
	}
	if _, err := service.fallbackFullMainline(ctx, "draft_closed"); err == nil {
		t.Fatal("closed fallback mainline 应失败")
	}
	if _, err := service.persistTimeline(ctx, "draft_closed", timeline.Empty("draft_closed", 1), nil, "closed"); err == nil {
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
	reporter := service.toolReporter("draft_closed")
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
