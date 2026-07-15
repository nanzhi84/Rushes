package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type timelineOpReactRepairModel struct {
	mu                 sync.Mutex
	calls              int
	applyPatchBound    bool
	sawExpectedSchema  bool
	sawCorrectExample  bool
	sawHarnessRecovery bool
	sawSucceededResult bool
}

func (modelValue *timelineOpReactRepairModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	for _, info := range infos {
		if info.Name == "timeline.apply_patch" {
			modelValue.applyPatchBound = true
			break
		}
	}
	return modelValue, nil
}

func (modelValue *timelineOpReactRepairModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	modelValue.calls++
	switch modelValue.calls {
	case 1:
		if !modelValue.applyPatchBound {
			return nil, errors.New("timeline.apply_patch 未绑定")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "trim_edge_wrong_field",
			Function: schema.FunctionCall{
				Name:      "timeline.apply_patch",
				Arguments: `{"op":{"kind":"trim_clip_edge","timeline_clip_id":"clip_v1_001","edge":"end","target_frame":45}}`,
			},
		}}), nil
	case 2:
		payload, err := timelineOpReactToolPayload(messages)
		if err != nil {
			return nil, err
		}
		if payload["status"] != "failed" {
			return nil, fmt.Errorf("错误字段调用未返回 structured failure: %#v", payload)
		}
		data, ok := payload["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("错误字段调用缺少 data: %#v", payload)
		}
		expectedSchema, schemaOK := data["expected_schema"].(map[string]any)
		properties, propertiesOK := expectedSchema["properties"].(map[string]any)
		if !schemaOK || !propertiesOK || properties["timeline_frame"] == nil ||
			properties["target_frame"] != nil {
			return nil, fmt.Errorf("expected_schema 未给出正确字段: %#v", data["expected_schema"])
		}
		modelValue.sawExpectedSchema = true
		correctExample, exampleOK := data["correct_example"].(map[string]any)
		if !exampleOK || correctExample["kind"] != "trim_clip_edge" ||
			correctExample["timeline_frame"] == nil || correctExample["target_frame"] != nil {
			return nil, fmt.Errorf("correct_example 无法指导修复: %#v", data["correct_example"])
		}
		modelValue.sawCorrectExample = true
		harness, harnessOK := data["harness_recovery"].(map[string]any)
		if !harnessOK || harness["automatic_retries"] != float64(0) ||
			harness["remaining_model_repairs"] != float64(maxModelRepairAttempts) ||
			harness["exhausted"] != false {
			return nil, fmt.Errorf("harness_recovery 未保留: %#v", data["harness_recovery"])
		}
		modelValue.sawHarnessRecovery = true
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "trim_edge_corrected_field",
			Function: schema.FunctionCall{
				Name:      "timeline.apply_patch",
				Arguments: `{"op":{"kind":"trim_clip_edge","timeline_clip_id":"clip_v1_001","edge":"end","timeline_frame":45}}`,
			},
		}}), nil
	case 3:
		payload, err := timelineOpReactToolPayload(messages)
		if err != nil {
			return nil, err
		}
		if payload["status"] != "succeeded" {
			return nil, fmt.Errorf("修正后的工具调用未成功: %#v", payload)
		}
		modelValue.sawSucceededResult = true
		return schema.AssistantMessage("已根据字段目录修正补丁并完成裁剪。", nil), nil
	default:
		return nil, fmt.Errorf("脚本模型收到额外的第 %d 次调用", modelValue.calls)
	}
}

func (modelValue *timelineOpReactRepairModel) Stream(
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

func (modelValue *timelineOpReactRepairModel) snapshot() (int, bool, bool, bool, bool) {
	modelValue.mu.Lock()
	defer modelValue.mu.Unlock()
	return modelValue.calls,
		modelValue.sawExpectedSchema,
		modelValue.sawCorrectExample,
		modelValue.sawHarnessRecovery,
		modelValue.sawSucceededResult
}

func TestReactAgentRepairsTimelineOpFromJITFieldFailure(t *testing.T) {
	const draftID = "draft_timeline_op_react_repair"
	database := agentTestDatabase(t)
	createAgentDraft(t, database, draftID)
	modelValue := &timelineOpReactRepairModel{}
	service, err := NewService(t.Context(), database, modelValue)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := timeline.ComposeInitial(draftID, 1, []timeline.Selection{{
		AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixtureResult, err := service.persistTimeline(t.Context(), draftID, document, "react_repair_fixture")
	if err != nil || fixtureResult.Status != "succeeded" {
		t.Fatalf("fixture result=%#v err=%v", fixtureResult, err)
	}

	recoveryState := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), recoveryState)
	ctx = withTurnBudgetState(ctx, newTurnBudgetState(maxToolRoundsPerTurn))
	ctx = rushestools.WithDraftID(ctx, draftID)
	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("把主视频片段结尾裁到第 45 帧。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Content != "已根据字段目录修正补丁并完成裁剪。" {
		t.Fatalf("response=%#v", response)
	}
	calls, sawSchema, sawExample, sawHarness, sawSuccess := modelValue.snapshot()
	if calls != 3 || !sawSchema || !sawExample || !sawHarness || !sawSuccess {
		t.Fatalf(
			"calls=%d schema=%v example=%v harness=%v success=%v",
			calls, sawSchema, sawExample, sawHarness, sawSuccess,
		)
	}
	if recoveryState.unresolved() || recoveryState.recoveryExhausted() || recoveryState.summary() != "" {
		t.Fatalf("成功修复后 recovery state 未清空: %s", recoveryState.summary())
	}

	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 || latest.DurationFrames != 45 ||
		len(latest.Tracks[0].Clips) != 1 || latest.Tracks[0].Clips[0].TimelineEndFrame != 45 {
		t.Fatalf("latest=%#v", latest)
	}
	var editBatchCount int
	if err := database.Read().QueryRowContext(
		t.Context(),
		"SELECT COUNT(*) FROM timeline_edit_batches WHERE draft_id=?",
		draftID,
	).Scan(&editBatchCount); err != nil {
		t.Fatal(err)
	}
	if editBatchCount != 1 {
		t.Fatalf("只应持久化修正后的成功补丁一次，edit batches=%d", editBatchCount)
	}
}

func timelineOpReactToolPayload(messages []*schema.Message) (map[string]any, error) {
	if len(messages) == 0 || messages[len(messages)-1].Role != schema.Tool {
		return nil, errors.New("工具 observation 未回灌模型")
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(messages[len(messages)-1].Content), &payload); err != nil {
		return nil, fmt.Errorf("工具 observation 不是合法 JSON: %w", err)
	}
	return payload, nil
}
