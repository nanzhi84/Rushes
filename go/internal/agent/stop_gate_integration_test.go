package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type catalogStopGateScript struct {
	mu        sync.Mutex
	round     int
	bound     []string
	boundRuns [][]string
}

func (script *catalogStopGateScript) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.bound = script.bound[:0]
	for _, info := range infos {
		script.bound = append(script.bound, info.Name)
	}
	script.boundRuns = append(script.boundRuns, append([]string(nil), script.bound...))
	return script, nil
}

func (script *catalogStopGateScript) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.round++
	if !hasCatalogWithoutSchemas(messages) {
		return nil, errors.New("provider 未收到不含 schema 的 Model Action Catalog")
	}
	call := func(id, name, arguments string) *schema.Message {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: id, Function: schema.FunctionCall{Name: name, Arguments: arguments},
		}})
	}
	switch script.round {
	case 1:
		if !reflect.DeepEqual(script.bound, []string{"tool.load"}) {
			return nil, fmt.Errorf("initial bound=%v", script.bound)
		}
		return call("load-update", "tool.load", `{"tool_names":["timeline.update"]}`), nil
	case 2:
		if !reflect.DeepEqual(script.bound, []string{"tool.load", "timeline.update"}) {
			return nil, fmt.Errorf("after first load bound=%v", script.bound)
		}
		return call("speed-up", "timeline.update", `{"kind":"set_playback_rate","timeline_clip_id":"clip_v1_001","playback_rate":2}`), nil
	case 3:
		if err := requireLatestSucceededTool(messages, "timeline.update"); err != nil {
			return nil, err
		}
		return call("restore-speed", "timeline.update", `{"kind":"set_playback_rate","timeline_clip_id":"clip_v1_001","playback_rate":1}`), nil
	case 4:
		if err := requireLatestSucceededTool(messages, "timeline.update"); err != nil {
			return nil, err
		}
		return schema.AssistantMessage("时间线调整结束。", nil), nil
	case 5:
		if !hasStopGateBlock(messages, "target_duration") {
			return nil, errors.New("Stop Gate validation_failed 未回灌模型")
		}
		if slices.Contains(script.bound, "timeline.insert") {
			return nil, fmt.Errorf("timeline.insert 在显式加载前已绑定: %v", script.bound)
		}
		return call("load-insert", "tool.load", `{"tool_names":["timeline.insert"]}`), nil
	case 6:
		if !reflect.DeepEqual(script.bound, []string{"tool.load", "timeline.insert", "timeline.update"}) {
			return nil, fmt.Errorf("additive bound=%v", script.bound)
		}
		if err := requireLatestSucceededTool(messages, "tool.load"); err != nil {
			return nil, err
		}
		return call("repair-duration", "timeline.insert", `{"kind":"insert_clip","asset_id":"visual","source_start_frame":60,"source_end_frame":120}`), nil
	case 7:
		if err := requireLatestSucceededTool(messages, "timeline.insert"); err != nil {
			return nil, err
		}
		return schema.AssistantMessage("时间线现为目标 120 帧。", nil), nil
	default:
		return nil, fmt.Errorf("unexpected provider round %d", script.round)
	}
}

func (script *catalogStopGateScript) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := script.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func hasCatalogWithoutSchemas(messages []*schema.Message) bool {
	for _, message := range messages {
		if message == nil || message.Role != schema.System ||
			message.Extra["context_phase"] != "model_action_catalog" {
			continue
		}
		return strings.Contains(message.Content, `"name":"timeline.insert"`) &&
			strings.Contains(message.Content, "Harness Automatic Capabilities") &&
			!strings.Contains(message.Content, `"properties"`) &&
			!strings.Contains(message.Content, `"parameters"`)
	}
	return false
}

func hasStopGateBlock(messages []*schema.Message, issue string) bool {
	for _, message := range messages {
		if message == nil || message.Role != schema.System ||
			message.Extra["context_phase"] != "stop_gate_feedback" {
			continue
		}
		return strings.Contains(message.Content, `"decision":"block"`) &&
			strings.Contains(message.Content, issue) &&
			strings.Contains(message.Content, `"result_ref":"validation:`)
	}
	return false
}

func requireLatestSucceededTool(messages []*schema.Message, name string) error {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.Tool {
			continue
		}
		if message.ToolName != name {
			return fmt.Errorf("latest tool=%s want=%s", message.ToolName, name)
		}
		var receipt struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(message.Content), &receipt) != nil ||
			receipt.Status != string(rushestools.StatusSucceeded) {
			return fmt.Errorf("%s receipt=%s", name, message.Content)
		}
		return nil
	}
	return fmt.Errorf("missing tool receipt %s", name)
}

func TestCatalogLoadMultipleEditsStopBlockLoadRepairAndPass(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_catalog_stop_gate_integration"
	agenttest.CreateAgentDraft(t, database, draftID)
	script := &catalogStopGateScript{}
	service, err := NewService(t.Context(), database, script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	assetResult, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{Type: "AssetImported", Payload: map[string]any{
			"asset_id": "visual", "job_id": "job_visual", "kind": "video",
			"filename": "visual.mp4", "usable": true,
			"probe": map[string]any{"duration_sec": 4.0, "has_audio": false},
		}},
		{Type: "AssetLinked", DraftID: draftID, Payload: map[string]any{"asset_id": "visual"}},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || assetResult.Status != reducer.StatusApplied {
		t.Fatalf("asset status=%s err=%v", assetResult.Status, err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceStartFrame: 0, SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, seedErr := seedTimelineVersion(service, t.Context(), draftID, document, "fixture", nil); seedErr != nil ||
		result.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed=%#v err=%v", result, seedErr)
	}
	if _, err = service.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"plan.update",
		rushestools.PlanUpdateInput{
			Plan:     map[string]any{"goal": "120 frames"},
			Contract: &rushestools.ContentPlanContract{TargetDurationFrames: 120},
		},
	); err != nil {
		t.Fatal(err)
	}

	_, events, unsubscribe := service.Hub().Subscribe(draftID)
	defer unsubscribe()
	agenttest.InsertAgentMessage(t, database, draftID, "user_catalog_stop", "调整两次后补到 120 帧")
	if !service.Queue().EnqueueUserMessage(draftID, "user_catalog_stop", "调整两次后补到 120 帧") {
		t.Fatal("enqueue failed")
	}
	service.Queue().JoinDraft(draftID)

	stopStatuses := []string{}
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case event := <-events:
			if event["type"] == TurnStreamStopGateFinished {
				stopStatuses = append(stopStatuses, fmt.Sprint(event["status"]))
			}
			if event["type"] == TurnStreamTurnEnded {
				break collect
			}
		case <-deadline:
			t.Fatal("等待 Stop Gate 事件超时")
		}
	}
	if !reflect.DeepEqual(stopStatuses, []string{"blocked", "passed"}) {
		debugMessages, _ := storage.ListMessages(t.Context(), database.Read(), draftID, 30)
		t.Fatalf("stop statuses=%v round=%d messages=%#v", stopStatuses, script.round, debugMessages)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.DurationFrames != 120 || latest.Version != 4 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 30)
	if err != nil {
		t.Fatal(err)
	}
	final := messages[len(messages)-1]
	if final.Kind != "reply" || final.Content != "时间线现为目标 120 帧。" {
		t.Fatalf("final=%#v", final)
	}
	toolCounts := map[string]int{}
	for _, message := range messages {
		if message.Kind != "tool" {
			continue
		}
		var trace struct {
			Tool string `json:"tool"`
		}
		if json.Unmarshal([]byte(message.Content), &trace) == nil {
			toolCounts[trace.Tool]++
		}
	}
	for name, count := range map[string]int{
		"tool.load": 2, "timeline.update": 2, "timeline.insert": 1, "stop.gate": 2,
	} {
		if toolCounts[name] != count {
			t.Fatalf("tool trace %s=%d want=%d all=%v", name, toolCounts[name], count, toolCounts)
		}
	}
	var leases int
	if err := database.Read().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM agent_edit_leases WHERE draft_id=?", draftID,
	).Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("remaining leases=%d err=%v", leases, err)
	}
}
