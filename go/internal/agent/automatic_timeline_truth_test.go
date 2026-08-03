package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// A successful atomic edit must reach the model unchanged. Full content validation
// belongs to the Stop Gate, not the post-tool middleware path.
func TestAtomicMutationReceiptDoesNotContainAutomaticTimelineCheck(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_no_post_mutation_check"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedTimelineVersion(service, t.Context(), draftID, document, "fixture", nil); err != nil {
		t.Fatal(err)
	}

	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	ctx = rushestools.WithDraftID(ctx, draftID)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	result, err := service.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_track_state", "track_id": "visual_base", "muted": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	data, _ := payload["data"].(map[string]any)
	if _, exists := data["automatic_timeline_check"]; exists {
		t.Fatalf("atomic receipt leaked final validation: %s", encoded)
	}
}

func TestStopGateChecksLatestVersionOnceAndReturnsCompactBlock(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_stop_gate_block"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

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
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedTimelineVersion(service, t.Context(), draftID, document, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}

	truth := newTerminalTimelineTruthState()
	truth.recordMutationTimelineID(latest.TimelineID)
	state := newAutomaticPreviewQAState()
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withTerminalTimelineTruthState(ctx, truth)
	ctx = withAutomaticPreviewQAState(ctx, state)
	candidate := schema.AssistantMessage("已完成，可交付。", nil)
	messages := []*schema.Message{schema.UserMessage("剪辑到 120 帧")}

	shouldRun, err := service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || !shouldRun {
		t.Fatalf("first stop should_run=%v err=%v", shouldRun, err)
	}
	feedback, err := service.runAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if feedback.Extra["context_phase"] != "stop_gate_feedback" {
		t.Fatalf("feedback=%#v", feedback)
	}
	var envelope map[string]any
	if err = json.Unmarshal([]byte(stopGateJSON(feedback.Content)), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["decision"] != "block" || envelope["result_ref"] != "validation:"+latest.TimelineID {
		t.Fatalf("envelope=%#v", envelope)
	}
	if issues, _ := envelope["issues"].([]any); len(issues) == 0 || len(issues) > 3 {
		t.Fatalf("issues=%#v", issues)
	}

	// The same version and fingerprint is not injected again. The candidate is
	// replaced at the service boundary with an honest not_completed summary.
	shouldRun, err = service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || shouldRun {
		t.Fatalf("duplicate stop should_run=%v err=%v", shouldRun, err)
	}
	if override := state.takeFinalOverride(); override == "" {
		t.Fatal("duplicate blocker must produce honest not_completed override")
	}
}

func TestStopGateContinuationBudgetIsThreeAndDuplicateDoesNotReinject(t *testing.T) {
	state := newAutomaticPreviewQAState()
	for index := 1; index <= maxAutomaticPreviewQAPassesPerTurn; index++ {
		duplicate, exhausted := state.registerBlocker(fmt.Sprintf("fingerprint-%d", index))
		if duplicate || exhausted {
			t.Fatalf("continuation %d duplicate=%v exhausted=%v", index, duplicate, exhausted)
		}
	}
	if duplicate, exhausted := state.registerBlocker("fingerprint-1"); !duplicate || exhausted {
		t.Fatalf("duplicate=%v exhausted=%v", duplicate, exhausted)
	}
	if duplicate, exhausted := state.registerBlocker("fingerprint-4"); duplicate || !exhausted {
		t.Fatalf("fourth unique duplicate=%v exhausted=%v", duplicate, exhausted)
	}
}

func TestStopGateLatestTimelineReadErrorBecomesHookError(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_stop_gate_read_error"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	state := newAutomaticPreviewQAState()
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	ctx = withAutomaticPreviewQAState(ctx, state)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	messages := []*schema.Message{schema.UserMessage("请生成预览并质检。")}
	candidate := schema.AssistantMessage("已完成，可交付。", nil)
	shouldRun, err := service.shouldRunAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || !shouldRun {
		t.Fatalf("storage failure should_run=%v err=%v", shouldRun, err)
	}
	feedback, err := service.runAutomaticPreviewQA(ctx, messages, candidate)
	if err != nil || feedback == nil || !strings.Contains(feedback.Content, "stop_gate_hook_error") {
		t.Fatalf("hook feedback=%#v err=%v", feedback, err)
	}
	started, hookError := 0, 0
	for _, event := range service.Hub().Snapshot(draftID) {
		if event["type"] == TurnStreamStopGateStarted {
			started++
		}
		if event["type"] == TurnStreamStopGateFinished && event["status"] == "hook_error" {
			hookError++
		}
	}
	if started != 1 || hookError != 1 {
		t.Fatalf("storage failure gate started=%d hook_error=%d", started, hookError)
	}
}

func TestStopGateTracePersistenceFailureCannotEmitPassed(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_stop_gate_persist_error"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 60,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedTimelineVersion(service, t.Context(), draftID, document, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	if err = database.Write().Close(); err != nil {
		t.Fatal(err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	_, _, err = service.executeAutomaticTimelineCheck(ctx, draftID, document.TimelineID, false)
	if err == nil {
		t.Fatal("Stop Gate trace persistence failure must fail closed")
	}
	passed, hookError := 0, 0
	for _, event := range service.Hub().Snapshot(draftID) {
		if event["type"] != TurnStreamStopGateFinished {
			continue
		}
		if event["status"] == "passed" {
			passed++
		}
		if event["status"] == "hook_error" {
			hookError++
		}
	}
	if passed != 0 || hookError != 1 {
		t.Fatalf("persist failure passed=%d hook_error=%d", passed, hookError)
	}
}

func TestStopGatePrioritizesStructuralIssuesBeforeContractFailures(t *testing.T) {
	issues := stopGateActionableIssues(rushestools.ToolResult{Data: map[string]any{
		"contract_failures": []map[string]any{
			{"check": "duration", "message": "时长不符"},
			{"check": "must_keep", "message": "缺少必保留内容"},
			{"check": "beat", "message": "节拍不符"},
		},
		"validation_report": map[string]any{"issues": []map[string]any{
			{"code": "invalid_document", "message": "时间线结构无效"},
		}},
	}})
	if len(issues) != 4 || issues[0]["code"] != "invalid_document" {
		t.Fatalf("prioritized issues=%#v", issues)
	}
}

func TestStopGateStateAndFeedbackHelpers(t *testing.T) {
	var nilState *automaticPreviewQAState
	nilState.setPending(stopGatePending{Decision: "block"})
	nilState.setPreviewBlocker(stopGatePending{Decision: "block"})
	nilState.clearPreviewBlocker("draft:v1")
	nilState.setFinalOverride("ignored")
	if pending := nilState.takePending(); pending.Decision != "" {
		t.Fatalf("nil pending=%#v", pending)
	}
	if blocker := nilState.previewBlockerFor("draft:v1"); blocker.Decision != "" {
		t.Fatalf("nil blocker=%#v", blocker)
	}
	if override := nilState.takeFinalOverride(); override != "" {
		t.Fatalf("nil override=%q", override)
	}
	if duplicate, exhausted := nilState.registerBlocker("fingerprint"); duplicate || exhausted {
		t.Fatalf("nil blocker registration duplicate=%v exhausted=%v", duplicate, exhausted)
	}

	state := newAutomaticPreviewQAState()
	pending := stopGatePending{
		Decision: "hook_error", TimelineID: "draft:v1", HookError: "checker unavailable",
		Issues:          []map[string]any{{"code": "gap", "message": "时间线存在空洞"}},
		RemainingIssues: 2, ResultRef: "validation:draft:v1", Duplicate: true,
	}
	state.setPending(pending)
	if got := state.takePending(); got.TimelineID != pending.TimelineID || state.takePending().Decision != "" {
		t.Fatalf("pending lifecycle=%#v", got)
	}
	state.setPreviewBlocker(pending)
	if got := state.previewBlockerFor("draft:v2"); got.Decision != "" {
		t.Fatalf("mismatched preview blocker=%#v", got)
	}
	if got := state.previewBlockerFor("draft:v1"); got.Decision != "hook_error" {
		t.Fatalf("preview blocker=%#v", got)
	}
	state.clearPreviewBlocker("draft:v2")
	if got := state.previewBlockerFor("draft:v1"); got.Decision == "" {
		t.Fatal("mismatched clear removed preview blocker")
	}
	state.clearPreviewBlocker("draft:v1")
	if got := state.previewBlockerFor("draft:v1"); got.Decision != "" {
		t.Fatalf("preview blocker not cleared: %#v", got)
	}
	state.setFinalOverride("   ")
	state.setFinalOverride("not_completed")
	if got := state.takeFinalOverride(); got != "not_completed" || state.takeFinalOverride() != "" {
		t.Fatalf("final override=%q", got)
	}

	first := stopGatePreviewFingerprint(pending, "validation_failed")
	if first == "" || first != stopGatePreviewFingerprint(pending, "validation_failed") ||
		first == stopGatePreviewFingerprint(pending, "failed") {
		t.Fatalf("preview fingerprints are not stable: %q", first)
	}
	feedback := stopGateFeedbackMessage(pending)
	if !strings.Contains(feedback.Content, `"error_code":"stop_gate_hook_error"`) ||
		!strings.Contains(feedback.Content, `"deduplicated":true`) {
		t.Fatalf("hook feedback=%s", feedback.Content)
	}
	pending.Exhausted = true
	if exhausted := stopGateFeedbackMessage(pending); !strings.Contains(exhausted.Content, "continuation 已耗尽") {
		t.Fatalf("exhausted feedback=%s", exhausted.Content)
	}
	if reply := stopGateNotCompletedReply(pending); !strings.Contains(reply, "终验程序未能完成") ||
		!strings.Contains(reply, "checker unavailable") {
		t.Fatalf("not_completed reply=%q", reply)
	}

	issues := previewReportActionableIssues(PreviewQAReport{
		Issues: []map[string]any{{"code": "black", "message": "存在黑帧"}},
		Errors: []map[string]string{{"error_code": "preview_error", "message": "decode failed"}},
	})
	if len(issues) != 2 || issues[0]["recovery"] == "" {
		t.Fatalf("preview issues=%#v", issues)
	}
	if summary := previewReportActionableIssues(PreviewQAReport{Summary: "预览未通过"}); len(summary) != 1 {
		t.Fatalf("summary issues=%#v", summary)
	}

	if terminalCandidateClaimsDeliverable(nil) ||
		terminalCandidateClaimsDeliverable(schema.AssistantMessage("尚未完成", nil)) ||
		!terminalCandidateClaimsDeliverable(schema.AssistantMessage("READY", nil)) ||
		terminalCandidateAdmitsNotCompleted(nil) ||
		!terminalCandidateAdmitsNotCompleted(schema.AssistantMessage("not_completed", nil)) {
		t.Fatal("terminal boundary helper mismatch")
	}
}

func TestTerminalFallbackChecksLatestUnverifiedMutation(t *testing.T) {
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_terminal_stop_fallback"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "visual", AssetKind: "video", SourceEndFrame: 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedTimelineVersion(service, t.Context(), draftID, document, "fixture", nil); err != nil {
		t.Fatal(err)
	}
	truth := newTerminalTimelineTruthState()
	truth.recordMutationTimelineID(draftID + ":v1")
	ctx := withTerminalTimelineTruthState(rushestools.WithDraftID(t.Context(), draftID), truth)
	if err = service.ensureTerminalTimelineTruth(ctx, draftID); err != nil {
		t.Fatal(err)
	}
	got := truth.snapshot()
	if got.checkTimelineID != draftID+":v1" || got.checkSequence != got.mutationSequence ||
		got.checkStatus != string(rushestools.StatusSucceeded) {
		t.Fatalf("truth=%#v", got)
	}
}

func stopGateJSON(content string) string {
	const marker = "【StopGateFeedback｜Harness 终验反馈】\n"
	content = content[len(marker):]
	for index, character := range content {
		if character == '\n' {
			return content[:index]
		}
	}
	return content
}
