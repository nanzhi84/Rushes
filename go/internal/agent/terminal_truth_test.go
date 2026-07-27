package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type terminalTruthScriptModel struct {
	mu                 sync.Mutex
	round              int
	mode               string
	mutationTimelineID string
}

func (script *terminalTruthScriptModel) WithTools(
	[]*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	return script, nil
}

func (script *terminalTruthScriptModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	script.mu.Lock()
	defer script.mu.Unlock()
	script.round++
	switch script.round {
	case 1:
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "mutation",
			Function: schema.FunctionCall{
				Name: "timeline.update",
				Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_v1_001",` +
					`"edge":"end","timeline_frame":45}`,
			},
		}}), nil
	case 2:
		result, err := latestTerminalTruthToolResult(messages)
		if err != nil {
			return nil, err
		}
		if result.Status != string(rushestools.StatusSucceeded) {
			return nil, fmt.Errorf("mutation result=%#v", result)
		}
		if script.mode == "missing" {
			return schema.AssistantMessage("已经全部完成。", nil), nil
		}
		if script.mode == "provider_error" {
			return nil, errors.New("provider failed after timeline mutation")
		}
		script.mutationTimelineID = result.Data["timeline_id"].(string)
		arguments, _ := json.Marshal(rushestools.TimelineInspectInput{
			TimelineID: script.mutationTimelineID,
		})
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "inspect",
			Function: schema.FunctionCall{
				Name: "timeline.inspect", Arguments: string(arguments),
			},
		}}), nil
	case 3:
		result, err := latestTerminalTruthToolResult(messages)
		if err != nil {
			return nil, err
		}
		if result.Status != string(rushestools.StatusSucceeded) {
			return nil, fmt.Errorf("inspect result=%#v", result)
		}
		timelineID := script.mutationTimelineID
		if script.mode == "stale" {
			timelineID = strings.TrimSuffix(timelineID, ":v2") + ":v1"
		}
		arguments, _ := json.Marshal(rushestools.TimelineCheckInput{TimelineID: timelineID})
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "check",
			Function: schema.FunctionCall{
				Name: "timeline.check", Arguments: string(arguments),
			},
		}}), nil
	case 4:
		result, err := latestTerminalTruthToolResult(messages)
		if err != nil {
			return nil, err
		}
		if result.Status != string(rushestools.StatusSucceeded) {
			return nil, fmt.Errorf("check result=%#v", result)
		}
		return schema.AssistantMessage("已经全部完成。", nil), nil
	default:
		return nil, fmt.Errorf("unexpected model round %d", script.round)
	}
}

func (script *terminalTruthScriptModel) Stream(
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

func latestTerminalTruthToolResult(messages []*schema.Message) (rushestools.ToolResult, error) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != schema.Tool {
			continue
		}
		var result rushestools.ToolResult
		if err := json.Unmarshal([]byte(messages[index].Content), &result); err != nil {
			return rushestools.ToolResult{}, err
		}
		return result, nil
	}
	return rushestools.ToolResult{}, fmt.Errorf("missing tool result")
}

func TestTerminalTimelineTruthRequiresLatestSuccessfulCheckBeforeReply(t *testing.T) {
	for _, test := range []struct {
		mode        string
		wantOutcome string
		wantContent string
		wantKind    string
	}{
		{mode: "missing", wantOutcome: "failed", wantContent: "尚未对这个最新版本执行成功", wantKind: "turn_failure"},
		{mode: "provider_error", wantOutcome: "failed", wantContent: "尚未对这个最新版本执行成功", wantKind: "turn_failure"},
		{mode: "stale", wantOutcome: "failed", wantContent: "最后成功检查的是", wantKind: "turn_failure"},
		{mode: "passing", wantOutcome: "finished", wantContent: "已经全部完成。", wantKind: "reply"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			draftID := "draft_terminal_truth_" + test.mode
			database := agenttest.AgentTestDatabase(t)
			agenttest.CreateAgentDraft(t, database, draftID)
			service, err := NewService(t.Context(), database, &terminalTruthScriptModel{mode: test.mode})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(service.Close)
			document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
				AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if result, seedErr := seedTimelineVersion(
				service, t.Context(), draftID, document, "terminal_truth_fixture", nil,
			); seedErr != nil || result.Status != string(rushestools.StatusSucceeded) {
				t.Fatalf("seed=%#v err=%v", result, seedErr)
			}
			agenttest.InsertAgentMessage(t, database, draftID, "user_terminal_truth", "裁到45帧")
			_, stream, unsubscribe := service.Hub().Subscribe(draftID)
			defer unsubscribe()
			if !service.Queue().EnqueueUserMessage(draftID, "user_terminal_truth", "裁到45帧") {
				t.Fatal("enqueue failed")
			}
			service.Queue().JoinDraft(draftID)

			var deltas strings.Builder
			completed := ""
			kind := ""
			outcome := ""
			deadline := time.After(5 * time.Second)
			for outcome == "" {
				select {
				case event := <-stream:
					switch event["type"] {
					case TurnStreamTextDelta:
						deltas.WriteString(event["delta"].(string))
					case TurnStreamMessageCompleted:
						completed, _ = event["content"].(string)
						kind, _ = event["kind"].(string)
					case TurnStreamTurnEnded:
						outcome, _ = event["outcome"].(string)
					}
				case <-deadline:
					t.Fatal("waiting for turn end timed out")
				}
			}
			if outcome != test.wantOutcome || kind != test.wantKind ||
				!strings.Contains(completed, test.wantContent) || deltas.String() != completed {
				t.Fatalf(
					"outcome=%q kind=%q deltas=%q completed=%q",
					outcome, kind, deltas.String(), completed,
				)
			}
			if test.mode != "passing" && strings.Contains(completed, "已经全部完成") {
				t.Fatalf("unverified success text leaked: %q", completed)
			}
			latest, err := timeline.Latest(t.Context(), database, draftID)
			if err != nil || latest.Version != 2 {
				t.Fatalf("latest=%#v err=%v", latest, err)
			}
			messages, err := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
			if err != nil || messages[len(messages)-1].Kind != test.wantKind {
				t.Fatalf("messages=%#v err=%v", messages, err)
			}
		})
	}
}

func TestTerminalTruthTrackerInvalidatesEarlierCheckAfterAnotherMutation(t *testing.T) {
	state := newTerminalTimelineTruthState()
	result := func(timelineID string) rushestools.ToolResult {
		return rushestools.ToolResult{
			Status: string(rushestools.StatusSucceeded), Data: map[string]any{"timeline_id": timelineID},
		}
	}
	state.recordToolResult("timeline.update", "succeeded", result("draft:v2"))
	state.recordToolResult("timeline.check", "succeeded", result("draft:v2"))
	state.recordToolResult("timeline.delete", "succeeded", result("draft:v3"))
	snapshot := state.snapshot()
	if snapshot.mutationTimelineID != "draft:v3" || snapshot.mutationSequence != 2 ||
		snapshot.checkTimelineID != "draft:v2" || snapshot.checkSequence != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestTerminalTruthRejectsMutationSuccessWithoutVersionProof(t *testing.T) {
	state := newTerminalTimelineTruthState()
	state.recordToolResult("timeline.update", "succeeded", rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded),
	})
	snapshot := state.snapshot()
	if snapshot.mutationSequence != 0 || !snapshot.mutationProofInvalid {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	err := (&Service{}).terminalReplyGuard(withTerminalTimelineTruthState(t.Context(), state), "draft")
	var guardErr *terminalReplyGuardError
	if !errors.As(err, &guardErr) || guardErr.kind != "timeline_mutation_unverified" {
		t.Fatalf("err=%#v", err)
	}
	if failure := terminalFailureReply(t.Context(), err); !strings.Contains(failure, "已拒绝成功声明") {
		t.Fatalf("failure=%q", failure)
	}
}

func TestTerminalTruthRejectsCheckSuccessWithoutVersionProof(t *testing.T) {
	for _, timelineID := range []string{"", "draft", "draft:vbad"} {
		t.Run(timelineID, func(t *testing.T) {
			state := newTerminalTimelineTruthState()
			state.recordToolResult("timeline.check", "succeeded", rushestools.ToolResult{
				Status: string(rushestools.StatusSucceeded),
				Data:   map[string]any{"timeline_id": timelineID},
			})
			snapshot := state.snapshot()
			if snapshot.checkSequence != 0 || snapshot.checkTimelineID != "" || !snapshot.checkProofInvalid {
				t.Fatalf("snapshot=%#v", snapshot)
			}
			err := (&Service{}).terminalReplyGuard(withTerminalTimelineTruthState(t.Context(), state), "draft")
			var guardErr *terminalReplyGuardError
			if !errors.As(err, &guardErr) || guardErr.kind != "timeline_check_unverified" {
				t.Fatalf("err=%#v", err)
			}
			if failure := terminalFailureReply(t.Context(), err); !strings.Contains(failure, "已拒绝成功声明") {
				t.Fatalf("failure=%q", failure)
			}
		})
	}
}

func TestConfirmedTimelineMutationAutomaticallyChecksCommittedVersion(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_confirmed_mutation_truth"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service, t.Context(), draftID, document, "confirmed_truth_fixture", nil); err != nil {
		t.Fatal(err)
	}
	truthState := newTerminalTimelineTruthState()
	ctx := withTerminalTimelineTruthState(rushestools.WithDraftID(t.Context(), draftID), truthState)
	content, err := service.replayPendingTool(ctx, QueueItem{DraftID: draftID, Payload: map[string]any{
		"pending_tool_call": map[string]any{
			"tool_name": "timeline.update",
			"arguments": map[string]any{
				"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
				"edge": "end", "timeline_frame": float64(45),
			},
		},
		"answer": map[string]any{"option_id": "confirm"},
	}})
	if err != nil || content == "" {
		t.Fatalf("replay content=%q err=%v", content, err)
	}
	snapshot := truthState.snapshot()
	if snapshot.mutationTimelineID == "" || snapshot.checkTimelineID != snapshot.mutationTimelineID ||
		snapshot.checkSequence != snapshot.mutationSequence {
		t.Fatalf("确认重放后必须自动检查同一版本：%#v", snapshot)
	}
}

func TestConfirmedToolRejectsStructuredAndMalformedFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		output    any
	}{
		{name: "memory_remove_validation_failed", tool: "memory.remove", output: rushestools.ToolResult{
			Status: string(rushestools.StatusValidationFailed), Observation: "目标片段已经过期",
		}},
		{name: "timeline_failed", tool: "timeline.update", output: rushestools.ToolResult{
			Status: string(rushestools.StatusFailed), Observation: "前置条件不满足",
		}},
		{name: "memory_remove_wrong_result_type", tool: "memory.remove", output: map[string]any{"status": "succeeded"}},
		{name: "memory_remove_unrelated_side_effects", tool: "memory.remove",
			arguments: map[string]any{"keys": []any{"missing"}},
			output: rushestools.ToolResult{Status: string(rushestools.StatusSucceeded), Data: map[string]any{
				"removed_keys": []string{}, "written_keys": []string{"other"}, "evicted_keys": []string{"important"},
			}}},
		{name: "timeline_check_contradictory_contract", tool: "timeline.check",
			arguments: map[string]any{},
			output: rushestools.ToolResult{Status: string(rushestools.StatusSucceeded), Data: map[string]any{
				"timeline_id": "draft:v2",
				"validation_report": map[string]any{
					"valid": true, "structural_valid": true, "content_contract_valid": true,
					"checks": []any{}, "issues": []any{},
				},
				"content_contract":  map[string]any{"pass": false},
				"contract_failures": []any{map[string]any{"pass": false}},
			}}},
		{name: "timeline_check_structural_issue_in_success", tool: "timeline.check",
			arguments: map[string]any{},
			output: rushestools.ToolResult{Status: string(rushestools.StatusSucceeded), Data: map[string]any{
				"timeline_id": "draft:v2",
				"validation_report": map[string]any{
					"valid": true, "structural_valid": true, "content_contract_valid": true,
					"checks": []any{"schema"},
					"issues": []any{map[string]any{"code": "invalid_document", "message": "invalid"}},
				},
			}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := requireConfirmedToolSuccess(test.tool, test.arguments, test.output, "draft")
			var guardErr *terminalReplyGuardError
			if !errors.As(err, &guardErr) || guardErr.kind != "tool_failure_unresolved" {
				t.Fatalf("err=%#v", err)
			}
			failure := terminalFailureReply(t.Context(), err)
			if !strings.Contains(failure, "本轮没有完成") || !strings.Contains(failure, "拒绝未验收的成功声明") {
				t.Fatalf("failure=%q", failure)
			}
		})
	}
}

func TestConfirmedRenderAcceptsQueuedAsSuccessfulDispatch(t *testing.T) {
	renderArguments := map[string]any{"kind": "final", "timeline_id": "draft:v2", "orientation": "portrait"}
	result, err := requireConfirmedToolSuccess("render.start", renderArguments, rushestools.ToolResult{
		Status: string(rushestools.StatusQueued), Observation: "render_final 任务已排队",
		Data: map[string]any{
			"job_id": "job_queued", "job_status": "pending", "timeline_version": 2,
			"render_kind": "final", "timeline_id": "draft:v2", "orientation": "portrait",
		},
	}, "draft")
	if err != nil || result.Status != string(rushestools.StatusQueued) {
		t.Fatalf("queued render result=%#v err=%v", result, err)
	}
	if _, err := requireConfirmedToolSuccess("timeline.update", nil, rushestools.ToolResult{
		Status: string(rushestools.StatusQueued),
	}, "draft"); err == nil {
		t.Fatal("queued 只能作为 render.start 的正常终态")
	}
	for name, data := range map[string]map[string]any{
		"missing_job_id":       {"job_status": "pending", "timeline_version": 2, "render_kind": "final"},
		"empty_job_id":         {"job_id": " ", "job_status": "pending", "timeline_version": 2, "render_kind": "final"},
		"invalid_job_status":   {"job_id": "job_queued", "job_status": "succeeded", "timeline_version": 2, "render_kind": "final"},
		"missing_timeline_ver": {"job_id": "job_queued", "job_status": "pending", "render_kind": "final"},
		"wrong_timeline_ver":   {"job_id": "job_queued", "job_status": "pending", "timeline_version": 1, "render_kind": "final"},
		"wrong_render_kind":    {"job_id": "job_queued", "job_status": "pending", "timeline_version": 2, "render_kind": "preview"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := requireConfirmedToolSuccess("render.start", renderArguments, rushestools.ToolResult{
				Status: string(rushestools.StatusQueued), Data: data,
			}, "draft"); err == nil {
				t.Fatal("incomplete render dispatch proof was accepted")
			}
		})
	}
}

func TestCheckOnlyTerminalTruthRejectsVersionAdvance(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_check_only_truth"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	first, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service, t.Context(), draftID, first, "check_only_v1", nil); err != nil {
		t.Fatal(err)
	}
	state := newTerminalTimelineTruthState()
	state.recordToolResult("timeline.check", "succeeded", rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded),
		Data:   map[string]any{"timeline_id": draftID + ":v1"},
	})
	second := first
	second.Version = 2
	second.TimelineID = draftID + ":v2"
	if _, err := seedTimelineVersion(service, t.Context(), draftID, second, "check_only_v2", nil); err != nil {
		t.Fatal(err)
	}
	ctx := withTerminalTimelineTruthState(t.Context(), state)
	err = service.terminalReplyGuard(ctx, draftID)
	var guardErr *terminalReplyGuardError
	if !errors.As(err, &guardErr) || guardErr.kind != "timeline_latest_changed" ||
		guardErr.mutationTimelineID != draftID+":v1" || guardErr.latestTimelineID != draftID+":v2" {
		t.Fatalf("check-only guard err=%#v", err)
	}
	if expected := terminalExpectedTimelineID(state.snapshot()); expected != draftID+":v1" {
		t.Fatalf("expected timeline=%q", expected)
	}
}

func TestFinalReplyCommitRejectsStaleTimelineInsideReducerTransaction(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_atomic_terminal_truth"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "talk", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service, t.Context(), draftID, document, "atomic_truth_fixture", nil); err != nil {
		t.Fatal(err)
	}

	err = service.commitFinalReply(
		t.Context(), QueueItem{DraftID: draftID}, "stale_success", "assistant", "reply",
		"已经全部完成。", draftID+":v0",
	)
	var guardErr *terminalReplyGuardError
	if !errors.As(err, &guardErr) || guardErr.kind != "timeline_latest_changed" ||
		guardErr.latestTimelineID != draftID+":v1" {
		t.Fatalf("stale commit err=%#v", err)
	}
	messages, listErr := storage.ListMessages(t.Context(), database.Read(), draftID, 20)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, message := range messages {
		if message.ID == "stale_success" {
			t.Fatalf("版本条件失败时成功消息不得提交：%#v", message)
		}
	}
}
