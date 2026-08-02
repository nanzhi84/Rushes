package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestCommittedTimelineMutationResultBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		committed bool
	}{
		{name: "malformed", raw: "{"},
		{name: "invalid timeline id", raw: `{"status":"succeeded","data":{"timeline_id":"draft"}}`},
		{name: "succeeded", raw: `{"status":"succeeded","data":{"timeline_id":"draft:v2"}}`, committed: true},
		{name: "validation unchanged", raw: `{"status":"validation_failed","data":{"timeline_id":"draft:v2","current_timeline_unchanged":true}}`},
		{name: "validation advanced", raw: `{"status":"validation_failed","data":{"timeline_id":"draft:v2","before_version":1,"after_version":2}}`, committed: true},
		{name: "validation not advanced", raw: `{"status":"validation_failed","data":{"timeline_id":"draft:v2","before_version":2,"after_version":2}}`},
		{name: "failed", raw: `{"status":"failed","data":{"timeline_id":"draft:v2"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, committed := committedTimelineMutationResult(test.raw)
			if committed != test.committed {
				t.Fatalf("committed=%v want=%v raw=%s", committed, test.committed, test.raw)
			}
		})
	}
}

func TestAttachAutomaticTimelineCheckEvidenceOnHarnessFailure(t *testing.T) {
	encoded := attachAutomaticTimelineCheckEvidence(
		rushestools.ToolResult{Status: string(rushestools.StatusSucceeded)},
		rushestools.ToolResult{},
		errors.New("timeline check unavailable"),
	)
	var mutation rushestools.ToolResult
	if err := json.Unmarshal([]byte(encoded), &mutation); err != nil {
		t.Fatal(err)
	}
	if mutation.Observation == "" || mutation.Data == nil {
		t.Fatalf("mutation=%#v", mutation)
	}
	checkBytes, err := json.Marshal(mutation.Data[automaticTimelineCheckDataKey])
	if err != nil {
		t.Fatal(err)
	}
	var check rushestools.ToolResult
	if err := json.Unmarshal(checkBytes, &check); err != nil {
		t.Fatal(err)
	}
	if check.Status != string(rushestools.StatusFailed) ||
		check.Data["error_code"] != string(rushestools.ErrCodeToolExecutionError) ||
		!strings.Contains(check.Observation, "timeline check unavailable") {
		t.Fatalf("check=%#v", check)
	}
}

type automaticValidationFailureModel struct {
	mu      sync.Mutex
	calls   int
	draftID string
}

func (stub *automaticValidationFailureModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return stub, nil
}

func (stub *automaticValidationFailureModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	view, err := dynamicPreviewQACurrentView(messages)
	if err != nil {
		return nil, err
	}
	version, _ := agentexec.NumericValue(view["version"])

	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls++
	switch stub.calls {
	case 1:
		return automaticTrackMuteCall("auto_truth_mutation_1", true), nil
	case 2, 3:
		wantTimelineID := fmt.Sprintf("%s:v%d", stub.draftID, stub.calls)
		if int(version) != stub.calls {
			return nil, fmt.Errorf("CurrentTimelineView version=%v want=%d", version, stub.calls)
		}
		check, checkErr := latestAutomaticTimelineCheck(messages)
		if checkErr != nil || check.Status != string(rushestools.StatusValidationFailed) ||
			agentexec.InterfaceString(check.Data["timeline_id"]) != wantTimelineID {
			return nil, fmt.Errorf("automatic check=%#v err=%v want=%s", check, checkErr, wantTimelineID)
		}
		if stub.calls == 2 {
			return automaticTrackMuteCall("auto_truth_mutation_2", false), nil
		}
		return schema.AssistantMessage("时间线写入已保留，但内容合同仍未通过；没有把校验失败说成完成。", nil), nil
	default:
		return nil, fmt.Errorf("unexpected provider call %d", stub.calls)
	}
}

func (stub *automaticValidationFailureModel) Stream(
	ctx context.Context,
	messages []*schema.Message,
	options ...model.Option,
) (*schema.StreamReader[*schema.Message], error) {
	message, err := stub.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

func automaticTrackMuteCall(callID string, muted bool) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: callID,
		Function: schema.FunctionCall{
			Name: "timeline.update",
			Arguments: fmt.Sprintf(
				`{"kind":"set_track_state","track_id":"bgm","muted":%t}`, muted,
			),
		},
	}})
}

func latestAutomaticTimelineCheck(messages []*schema.Message) (rushestools.ToolResult, error) {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message == nil || message.Role != schema.Tool || message.ToolName != "timeline.update" {
			continue
		}
		var mutation rushestools.ToolResult
		if err := json.Unmarshal([]byte(message.Content), &mutation); err != nil {
			return rushestools.ToolResult{}, err
		}
		encoded, _ := json.Marshal(mutation.Data[automaticTimelineCheckDataKey])
		var check rushestools.ToolResult
		if err := json.Unmarshal(encoded, &check); err != nil {
			return rushestools.ToolResult{}, err
		}
		return check, nil
	}
	return rushestools.ToolResult{}, fmt.Errorf("missing automatic timeline check evidence")
}

func TestAutomaticValidationFailurePreservesMutationAndDoesNotPoisonNextEdit(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_auto_truth_validation_failure"
	agenttest.CreateAgentDraft(t, database, draftID)
	provider := &automaticValidationFailureModel{draftID: draftID}
	service, err := NewService(t.Context(), database, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	toolCtx := rushestools.WithDraftID(t.Context(), draftID)
	planned, err := service.ExecuteTool(toolCtx, "plan.update", rushestools.PlanUpdateInput{
		Plan: map[string]any{"goal": "补足到 120 帧"},
		Contract: &rushestools.ContentPlanContract{
			TargetDurationFrames: 120,
		},
	})
	if err != nil || planned.(rushestools.ToolResult).Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("plan=%#v err=%v", planned, err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if seeded, seedErr := seedTimelineVersion(
		service, t.Context(), draftID, document, "auto_truth_fixture", nil,
	); seedErr != nil || seeded.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed=%#v err=%v", seeded, seedErr)
	}

	recovery := newToolRecoveryState()
	truth := newTerminalTimelineTruthState()
	ctx := withToolRecoveryState(t.Context(), recovery)
	ctx = withTurnBudgetState(ctx, newTurnBudgetState(maxToolRoundsPerTurn))
	ctx = withTestTurnLeaseSession(t, service, ctx, draftID)
	ctx = withModelToolSurfaceSession(ctx)
	ctx = withTerminalTimelineTruthState(ctx, truth)
	ctx = agentexec.WithTurnInteractionState(ctx, agentexec.NewTurnInteractionState(service.indexedResources))
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, draftID))
	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("切换主视觉轨状态，并根据自动检查证据继续处理。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Content == "" || recovery.unresolved() {
		t.Fatalf("response=%#v recovery=%s", response, recovery.summary())
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.TimelineID != draftID+":v3" {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	snapshot := truth.snapshot()
	if snapshot.mutationTimelineID != draftID+":v3" ||
		snapshot.checkTimelineID != draftID+":v3" ||
		snapshot.checkStatus != string(rushestools.StatusValidationFailed) ||
		snapshot.checkSequence != snapshot.mutationSequence {
		t.Fatalf("truth=%#v", snapshot)
	}
	messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if err != nil {
		t.Fatal(err)
	}
	harnessTraces := 0
	for _, message := range messages {
		if message.Kind != "tool" {
			continue
		}
		var trace map[string]any
		if json.Unmarshal([]byte(message.Content), &trace) == nil &&
			trace["tool"] == "timeline.check" && trace["harness_owned"] == true &&
			trace["duration_ms"] != nil && trace["progress"] != nil {
			harnessTraces++
		}
	}
	if harnessTraces != 2 {
		t.Fatalf("persisted harness traces=%d messages=%#v", harnessTraces, messages)
	}
}

func TestTerminalFallbackAutomaticallyChecksLatestUnverifiedMutation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_terminal_auto_truth_fallback"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, composeErr := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 30,
	}})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if seeded, seedErr := seedTimelineVersion(
		service, t.Context(), draftID, document, "auto_truth_fallback", nil,
	); seedErr != nil || seeded.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("seed=%#v err=%v", seeded, seedErr)
	}
	truth := newTerminalTimelineTruthState()
	truth.recordMutationTimelineID(draftID + ":v1")
	ctx := withTerminalTimelineTruthState(
		rushestools.WithDraftID(t.Context(), draftID), truth,
	)
	if err := service.ensureTerminalTimelineTruth(ctx, draftID); err != nil {
		t.Fatal(err)
	}
	snapshot := truth.snapshot()
	if snapshot.checkTimelineID != draftID+":v1" ||
		snapshot.checkSequence != snapshot.mutationSequence ||
		snapshot.checkStatus != string(rushestools.StatusSucceeded) {
		t.Fatalf("truth=%#v", snapshot)
	}
}
