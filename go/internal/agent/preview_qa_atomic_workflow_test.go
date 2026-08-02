package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/media"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

type dynamicPreviewQAReactModel struct {
	mu              sync.Mutex
	draftID         string
	timelineID      string
	previewID       string
	bound           []string
	surfaces        [][]string
	versions        []int
	leaseSeen       bool
	calls           int
	inspectAfterFix bool
}

func (stub *dynamicPreviewQAReactModel) WithTools(
	infos []*schema.ToolInfo,
) (model.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	stub.mu.Lock()
	stub.bound = names
	stub.mu.Unlock()
	return stub, nil
}

func (stub *dynamicPreviewQAReactModel) Generate(
	_ context.Context,
	messages []*schema.Message,
	_ ...model.Option,
) (*schema.Message, error) {
	view, err := dynamicPreviewQACurrentView(messages)
	if err != nil {
		return nil, err
	}
	version, ok := agentexec.NumericValue(view["version"])
	if !ok {
		return nil, fmt.Errorf("CurrentTimelineView 缺少 version: %#v", view)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.calls++
	bound := append([]string(nil), stub.bound...)
	stub.surfaces = append(stub.surfaces, bound)
	stub.versions = append(stub.versions, int(version))
	stub.leaseSeen = stub.leaseSeen || strings.TrimSpace(
		agentexec.InterfaceString(view["edit_lease_turn_id"]),
	) != ""

	switch stub.calls {
	case 1:
		if !containsName(bound, "preview.generate") ||
			containsName(bound, "preview.check") || containsName(bound, "timeline.update") {
			return nil, fmt.Errorf("preview 生成阶段工具面=%v", bound)
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "dynamic_preview_generate",
			Function: schema.FunctionCall{
				Name: "preview.generate", Arguments: fmt.Sprintf(
					`{"timeline_id":%q,"orientation":"auto"}`, stub.timelineID,
				),
			},
		}}), nil
	case 2:
		if !containsName(bound, "preview.check") ||
			containsName(bound, "preview.generate") || containsName(bound, "timeline.update") {
			return nil, fmt.Errorf("preview QA 阶段工具面=%v", bound)
		}
		if !hasSuccessfulDynamicPreviewToolResult(messages, "preview.generate", 1) {
			return nil, errors.New("preview.generate 终态结果未回灌同一 ReAct transcript")
		}
		checks := []string{"black", "freeze", "silence"}
		calls := make([]schema.ToolCall, 0, len(checks))
		for _, check := range checks {
			calls = append(calls, schema.ToolCall{
				ID: "dynamic_preview_check_" + check,
				Function: schema.FunctionCall{
					Name: "preview.check", Arguments: fmt.Sprintf(
						`{"preview_id":%q,"check":%q}`, stub.previewID, check,
					),
				},
			})
		}
		return schema.AssistantMessage("", calls), nil
	case 3:
		if !containsName(bound, "timeline.update") ||
			containsName(bound, "preview.check") || containsName(bound, "preview.generate") {
			return nil, fmt.Errorf("preview QA 后未回到原子编辑面: %v", bound)
		}
		if !hasSuccessfulDynamicPreviewToolResult(messages, "preview.check", 3) {
			return nil, errors.New("preview.check 终态证据未全部回灌同一 ReAct transcript")
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "dynamic_preview_fix",
			Function: schema.FunctionCall{
				Name: "timeline.update",
				Arguments: `{"kind":"trim_clip","timeline_clip_id":"clip_v1_001",` +
					`"source_start_frame":15,"source_end_frame":60}`,
			},
		}}), nil
	case 4:
		if int(version) != 2 || !dynamicPreviewQAClipStartsAt(view, "clip_v1_001", 15) {
			return nil, fmt.Errorf("mutation 后 provider 未看到 v2 真实片段状态: %#v", view)
		}
		if !hasSuccessfulDynamicPreviewToolResult(messages, "timeline.update", 1) {
			return nil, errors.New("timeline.update 终态结果未回灌")
		}
		if containsName(bound, "timeline.inspect") && !containsName(bound, "timeline.check") {
			stub.inspectAfterFix = true
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "dynamic_preview_fix_inspect",
				Function: schema.FunctionCall{
					Name:      "timeline.inspect",
					Arguments: fmt.Sprintf(`{"timeline_id":%q}`, stub.draftID+":v2"),
				},
			}}), nil
		}
		if !containsName(bound, "timeline.check") {
			return nil, fmt.Errorf("修正后既不能 inspect 也不能 check: %v", bound)
		}
		return dynamicPreviewQACheckCall(stub.draftID), nil
	case 5:
		if stub.inspectAfterFix {
			if !hasSuccessfulDynamicPreviewToolResult(messages, "timeline.inspect", 1) ||
				!containsName(bound, "timeline.check") {
				return nil, fmt.Errorf("修正后观察屏障未回到检查面: %v", bound)
			}
			return dynamicPreviewQACheckCall(stub.draftID), nil
		}
		if !hasSuccessfulDynamicPreviewToolResult(messages, "timeline.check", 1) {
			return nil, errors.New("timeline.check 终态结果未回灌")
		}
		return schema.AssistantMessage("动态 preview QA 已在同一 turn 修正并验证。", nil), nil
	case 6:
		if !stub.inspectAfterFix ||
			!hasSuccessfulDynamicPreviewToolResult(messages, "timeline.check", 1) {
			return nil, errors.New("修正后的 timeline.check 终态结果未回灌")
		}
		return schema.AssistantMessage("动态 preview QA 已在同一 turn 修正并验证。", nil), nil
	default:
		return nil, fmt.Errorf("动态 preview QA 收到额外 provider call: %d", stub.calls)
	}
}

func (stub *dynamicPreviewQAReactModel) Stream(
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

func (stub *dynamicPreviewQAReactModel) snapshot() (
	int, [][]string, []int, bool,
) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	surfaces := make([][]string, len(stub.surfaces))
	for index := range stub.surfaces {
		surfaces[index] = append([]string(nil), stub.surfaces[index]...)
	}
	return stub.calls, surfaces, append([]int(nil), stub.versions...), stub.leaseSeen
}

func dynamicPreviewQACurrentView(messages []*schema.Message) (map[string]any, error) {
	var current *schema.Message
	for _, message := range messages {
		if message == nil {
			continue
		}
		if phase, _ := message.Extra["context_phase"].(string); phase == currentTimelineViewContextPhase {
			if current != nil {
				return nil, errors.New("provider 同时收到多份 CurrentTimelineView")
			}
			current = message
		}
	}
	if current == nil {
		return nil, errors.New("provider 缺少 CurrentTimelineView")
	}
	start := strings.IndexByte(current.Content, '{')
	end := strings.LastIndex(current.Content, "\n这是当前时间线")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("CurrentTimelineView 格式无效: %s", current.Content)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(current.Content[start:end]), &view); err != nil {
		return nil, err
	}
	return view, nil
}

func hasSuccessfulDynamicPreviewToolResult(
	messages []*schema.Message,
	name string,
	want int,
) bool {
	count := 0
	for _, message := range messages {
		if message == nil || message.Role != schema.Tool || message.ToolName != name {
			continue
		}
		var result struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(message.Content), &result) == nil &&
			result.Status == string(rushestools.StatusSucceeded) {
			count++
		}
	}
	return count >= want
}

func dynamicPreviewQAClipStartsAt(view map[string]any, clipID string, sourceStart int) bool {
	clips, _ := view["clips"].([]any)
	for _, raw := range clips {
		clip, _ := raw.(map[string]any)
		start, ok := agentexec.NumericValue(clip["source_start_frame"])
		if clip["timeline_clip_id"] == clipID && ok && int(start) == sourceStart {
			return true
		}
	}
	return false
}

func dynamicPreviewQACheckCall(draftID string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: "dynamic_preview_revalidate",
		Function: schema.FunctionCall{
			Name: "timeline.check", Arguments: fmt.Sprintf(`{"timeline_id":%q}`, draftID+":v2"),
		},
	}})
}

func TestDynamicPreviewQAReActReturnsToMutationAndRefreshesTimeline(t *testing.T) {
	installPreviewQAMediaFixture(t)

	database := agenttest.AgentTestDatabase(t)
	const (
		draftID   = "draft_dynamic_preview_qa"
		assetID   = "asset_dynamic_preview_qa"
		previewID = "preview_dynamic_qa"
		jobID     = "job_dynamic_preview_qa"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	provider := &dynamicPreviewQAReactModel{
		draftID: draftID, timelineID: draftID + ":v1", previewID: previewID,
	}
	service, err := NewService(t.Context(), database, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)

	source := filepath.Join(database.Paths.Temporary, "dynamic-preview-qa-source.mp4")
	if err := os.WriteFile(source, []byte("deterministic dynamic preview fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPreviewQAAsset(t, database, draftID, assetID, source)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: assetID, AssetKind: "video", HasAudio: true,
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(
		service, t.Context(), draftID, document, "dynamic_preview_qa_fixture", nil,
	); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persist timeline=%#v err=%v", persisted, persistErr)
	}
	seedPreviewQAArtifact(t, database, draftID, previewID, source)

	now := time.Now().UTC()
	enqueued, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobEnqueued", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"idempotency_key": "render_preview:" + draftID + ":1:auto",
			"job_payload":     map[string]any{"timeline_version": 1, "orientation": "auto"},
			"next_run_at":     now.Format(time.RFC3339Nano), "max_retries": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorAgent})
	if err != nil || enqueued.Status != reducer.StatusApplied {
		t.Fatalf("enqueue preview status=%s err=%v", enqueued.Status, err)
	}
	completed, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "JobSucceeded", DraftID: draftID,
		Payload: map[string]any{
			"job_id": jobID, "kind": "render_preview", "requested_by_draft_id": draftID,
			"result": map[string]any{
				"artifact_id": previewID, "timeline_version": 1, "orientation": "auto",
			},
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || completed.Status != reducer.StatusApplied {
		t.Fatalf("complete preview status=%s err=%v", completed.Status, err)
	}

	recoveryState := newToolRecoveryState()
	ctx := withToolRecoveryState(t.Context(), recoveryState)
	ctx = withTurnBudgetState(ctx, newTurnBudgetState(maxToolRoundsPerTurn))
	ctx = withTestTurnLeaseSession(t, service, ctx, draftID)
	ctx = withModelToolSurfaceSession(ctx)
	ctx = withTerminalTimelineTruthState(ctx, newTerminalTimelineTruthState())
	ctx = agentexec.WithTurnInteractionState(
		ctx, agentexec.NewTurnInteractionState(service.indexedResources),
	)
	ctx = rushestools.WithReporter(ctx, service.toolReporter(ctx, draftID))
	response, err := service.react.Generate(ctx, []*schema.Message{
		schema.UserMessage("生成预览并检查黑帧；发现问题就处理。"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Content != "动态 preview QA 已在同一 turn 修正并验证。" {
		t.Fatalf("response=%#v", response)
	}

	calls, surfaces, versions, leaseSeen := provider.snapshot()
	if calls < 5 || calls > 6 || len(surfaces) != calls || len(versions) != calls {
		t.Fatalf("calls=%d surfaces=%v versions=%v", calls, surfaces, versions)
	}
	if !containsName(surfaces[0], "preview.generate") ||
		!containsName(surfaces[1], "preview.check") ||
		!containsName(surfaces[2], "timeline.update") ||
		containsName(surfaces[2], "preview.check") || !leaseSeen {
		t.Fatalf("动态阶段披露或 lease 不符合预期: surfaces=%v lease=%v", surfaces, leaseSeen)
	}
	if versions[0] != 1 || versions[1] != 1 || versions[2] != 1 || versions[3] != 2 {
		t.Fatalf("CurrentTimelineView 未在 mutation 后刷新: versions=%v", versions)
	}
	latest, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest=%#v err=%v", latest, err)
	}
	clips := timelineTrackClips(latest, "visual_base")
	if len(clips) != 1 || clips[0].SourceStartFrame != 15 || clips[0].SourceEndFrame != 60 {
		t.Fatalf("QA 修正未精确提交: %#v", clips)
	}
	if recoveryState.unresolved() {
		t.Fatalf("preview QA 后仍有未解决 recovery: %s", recoveryState.summary())
	}
}

// TestPreviewQAWorkflowRunsChecksInParallelThenAppliesOneAtomicFix 固化 #141 的
// preview QA playbook：同一 assistant message 并行取得多个只读证据，模型只根据
// 具体证据选择一个通用原子编辑，检查工具本身不改变任何业务状态。
func TestPreviewQAWorkflowRunsChecksInParallelThenAppliesOneAtomicFix(t *testing.T) {
	installPreviewQAMediaFixture(t)

	database := agenttest.AgentTestDatabase(t)
	const (
		draftID   = "draft_preview_qa_atomic"
		assetID   = "asset_preview_qa_atomic"
		previewID = "preview_qa_atomic"
	)
	agenttest.CreateAgentDraft(t, database, draftID)
	source := filepath.Join(database.Paths.Temporary, "preview-qa-source.mp4")
	if err := os.WriteFile(source, []byte("deterministic preview fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedPreviewQAAsset(t, database, draftID, assetID, source)

	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: assetID, AssetKind: "video", HasAudio: true,
		SourceStartFrame: 0, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(service,
		t.Context(), draftID, document, "preview_qa_fixture", nil); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persist timeline=%#v err=%v", persisted, persistErr)
	}
	seedPreviewQAArtifact(t, database, draftID, previewID, source)

	previewSpec, exists := service.Tools().Spec("preview.check")
	if !exists || previewSpec.Family != rushestools.FamilyCheck ||
		previewSpec.Effect != rushestools.EffectReadOnly {
		t.Fatalf("preview.check spec=%#v exists=%v", previewSpec, exists)
	}
	router, err := newToolRouter(
		t.Context(),
		compose.ToolsNodeConfig{Tools: service.Tools().EinoTools(true, false)},
		service.Tools().Spec,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ctx = withTestTurnLeaseSession(t, service, ctx, draftID)
	ctx = agentexec.WithTurnInteractionState(
		ctx,
		agentexec.NewTurnInteractionState(service.indexedResources),
	)

	beforeChecks := previewQABusinessSnapshot(t, database, draftID)
	checkMessage := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{
				ID: "preview_black",
				Function: schema.FunctionCall{
					Name:      "preview.check",
					Arguments: `{"preview_id":"preview_qa_atomic","check":"black"}`,
				},
			},
			{
				ID: "preview_freeze",
				Function: schema.FunctionCall{
					Name:      "preview.check",
					Arguments: `{"preview_id":"preview_qa_atomic","check":"freeze"}`,
				},
			},
			{
				ID: "preview_silence",
				Function: schema.FunctionCall{
					Name:      "preview.check",
					Arguments: `{"preview_id":"preview_qa_atomic","check":"silence"}`,
				},
			},
		},
	}
	if !router.canRunParallel(checkMessage) {
		t.Fatal("Registry Effect 未让纯 preview.check assistant message 进入并行 ToolsNode")
	}
	checkMessages, err := router.Invoke(ctx, checkMessage)
	if err != nil || len(checkMessages) != 3 {
		t.Fatalf("parallel preview checks=%#v err=%v", checkMessages, err)
	}
	afterChecks := previewQABusinessSnapshot(t, database, draftID)
	if !reflect.DeepEqual(afterChecks, beforeChecks) {
		t.Fatalf(
			"preview.check 自动写入业务状态:\nbefore=%s\nafter=%s",
			beforeChecks.Database, afterChecks.Database,
		)
	}
	if entered := previewQABarrierEntrants(t); entered != 3 {
		t.Fatalf("同一 assistant message 未让 3 个真实 preview.check 同时越过屏障: entrants=%d", entered)
	}

	evidence := make(map[string]rushestools.PreviewInspectionResult, len(checkMessages))
	for _, message := range checkMessages {
		var result rushestools.PreviewInspectionResult
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
			t.Fatalf("decode preview evidence %q: %v", message.Content, err)
		}
		if result.PreviewID != previewID || len(result.Issues) != 1 {
			t.Fatalf("preview evidence=%#v", result)
		}
		evidence[result.Check] = result
	}
	for _, check := range []string{"black", "freeze", "silence"} {
		if _, ok := evidence[check]; !ok {
			t.Fatalf("missing %s evidence: %#v", check, evidence)
		}
	}

	// “模型决定”只消费 black 检查返回的具体帧范围：去掉开头黑帧；freeze 与
	// silence 仍作为证据保留，但不会触发第二个隐藏修复或复合工作流。
	blackFrames := previewQAIssueFrames(t, evidence["black"].Issues[0])
	current, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	primary := timelineTrackClips(current, "visual_base")
	if len(primary) != 1 || blackFrames[0] != 0 || blackFrames[1] <= blackFrames[0] {
		t.Fatalf("black evidence=%v primary=%#v", blackFrames, primary)
	}
	updateArguments, err := json.Marshal(rushestools.TimelineUpdateInput{
		"kind": "trim_clip", "timeline_clip_id": primary[0].TimelineClipID,
		"source_start_frame": blackFrames[1], "source_end_frame": primary[0].SourceEndFrame,
	})
	if err != nil {
		t.Fatal(err)
	}
	updateMessages, err := router.Invoke(ctx, &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "preview_qa_fix",
			Function: schema.FunctionCall{
				Name: "timeline.update", Arguments: string(updateArguments),
			},
		}},
	})
	if err != nil || len(updateMessages) != 1 {
		t.Fatalf("atomic preview fix=%#v err=%v", updateMessages, err)
	}
	var updateResult rushestools.ToolResult
	if err := json.Unmarshal([]byte(updateMessages[0].Content), &updateResult); err != nil {
		t.Fatal(err)
	}
	applied, _ := updateResult.Data["applied_operation"].(map[string]any)
	if updateResult.Status != string(rushestools.StatusSucceeded) ||
		applied["kind"] != "trim_clip" {
		t.Fatalf("atomic preview fix result=%#v", updateResult)
	}

	afterFix, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || afterFix.Version != beforeChecks.TimelineVersion+1 {
		t.Fatalf("after fix version=%d err=%v", afterFix.Version, err)
	}
	fixedPrimary := timelineTrackClips(afterFix, "visual_base")
	if len(fixedPrimary) != 1 ||
		fixedPrimary[0].SourceStartFrame != blackFrames[1] ||
		fixedPrimary[0].SourceEndFrame != primary[0].SourceEndFrame ||
		!timeline.Validate(afterFix).Valid {
		t.Fatalf("fixed timeline=%#v", afterFix)
	}
	var editBatchCount, editedOperationCount int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), COALESCE(SUM(json_array_length(operations_json)), 0)
		FROM timeline_edit_batches WHERE draft_id=?`, draftID,
	).Scan(&editBatchCount, &editedOperationCount); err != nil ||
		editBatchCount != 1 || editedOperationCount != 1 {
		t.Fatalf(
			"preview QA 应只提交一个原子编辑: batches=%d operations=%d err=%v",
			editBatchCount, editedOperationCount, err,
		)
	}

	checkSpec, exists := service.Tools().Spec("timeline.check")
	if !exists || checkSpec.Effect != rushestools.EffectReadOnly {
		t.Fatalf("timeline.check spec=%#v exists=%v", checkSpec, exists)
	}
	beforeValidation := previewQABusinessSnapshot(t, database, draftID)
	validationMessages, err := router.Invoke(ctx, &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "preview_qa_revalidate",
			Function: schema.FunctionCall{
				Name: "timeline.check", Arguments: `{}`,
			},
		}},
	})
	if err != nil || len(validationMessages) != 1 {
		t.Fatalf("timeline revalidation=%#v err=%v", validationMessages, err)
	}
	var validation rushestools.ToolResult
	if err := json.Unmarshal([]byte(validationMessages[0].Content), &validation); err != nil {
		t.Fatal(err)
	}
	afterValidation := previewQABusinessSnapshot(t, database, draftID)
	if validation.Status != string(rushestools.StatusSucceeded) ||
		!reflect.DeepEqual(afterValidation, beforeValidation) {
		t.Fatalf(
			"只读 revalidation 未通过或写入状态: result=%#v before=%s after=%s",
			validation, beforeValidation.Database, afterValidation.Database,
		)
	}
}

type previewQAState struct {
	Database        string
	TimelineVersion int
	EventCount      int
	JobCount        int
}

func previewQABusinessSnapshot(
	t *testing.T,
	database *storage.DB,
	draftID string,
) previewQAState {
	t.Helper()
	state := previewQAState{}
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COALESCE(MAX(version), 0) FROM timeline_versions WHERE draft_id=?`,
		draftID,
	).Scan(&state.TimelineVersion); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM event_log WHERE draft_id=?`, draftID,
	).Scan(&state.EventCount); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM jobs WHERE draft_id=? OR requested_by_draft_id=?`,
		draftID, draftID,
	).Scan(&state.JobCount); err != nil {
		t.Fatal(err)
	}
	state.Database = previewQADatabaseContents(t, database)
	return state
}

func previewQADatabaseContents(t *testing.T, database *storage.DB) string {
	t.Helper()
	tableRows, err := database.Read().QueryContext(t.Context(), `
		SELECT name FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{}
	for tableRows.Next() {
		var table string
		if err := tableRows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := tableRows.Close(); err != nil {
		t.Fatal(err)
	}
	contents := make(map[string][]string, len(tables))
	for _, table := range tables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		rows, queryErr := database.Read().QueryContext(t.Context(), "SELECT * FROM "+quoted)
		if queryErr != nil {
			t.Fatalf("snapshot table %s: %v", table, queryErr)
		}
		columns, columnsErr := rows.Columns()
		if columnsErr != nil {
			_ = rows.Close()
			t.Fatal(columnsErr)
		}
		encodedRows := []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for index := range values {
				destinations[index] = &values[index]
			}
			if scanErr := rows.Scan(destinations...); scanErr != nil {
				_ = rows.Close()
				t.Fatal(scanErr)
			}
			encoded, marshalErr := json.Marshal(values)
			if marshalErr != nil {
				_ = rows.Close()
				t.Fatal(marshalErr)
			}
			encodedRows = append(encodedRows, string(encoded))
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			t.Fatal(rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		sort.Strings(encodedRows)
		contents[table] = encodedRows
	}
	encoded, err := json.Marshal(contents)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func seedPreviewQAAsset(
	t *testing.T,
	database *storage.DB,
	draftID string,
	assetID string,
	source string,
) {
	t.Helper()
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{
		{
			Type: "AssetImported",
			Payload: map[string]any{
				"asset_id": assetID, "job_id": "job_" + assetID,
				"storage_mode": "reference", "reference_path": source,
				"kind": "video", "source": "local_path", "filename": filepath.Base(source),
				"hash": assetID, "size": 29, "ingest_status": "ready", "usable": true,
				"probe": map[string]any{
					"duration_sec": 2.0, "has_audio": true,
				},
			},
		},
		{
			Type: "AssetLinked", DraftID: draftID,
			Payload: map[string]any{"asset_id": assetID},
		},
	}, reducer.Options{Actor: contracts.ActorUser})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed asset status=%s err=%v", result.Status, err)
	}
}

func seedPreviewQAArtifact(
	t *testing.T,
	database *storage.DB,
	draftID string,
	previewID string,
	source string,
) {
	t.Helper()
	object, err := media.NewObjectStore(database.Paths).PutFile(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reducer.Apply(t.Context(), database, []contracts.Event{{
		Type: "PreviewRendered", DraftID: draftID,
		Payload: map[string]any{
			"artifact_id": previewID, "timeline_version": 1,
			"object_hash": object.Hash, "object_size": object.Size,
			"render_width": 320, "render_height": 240,
			"render_fps": 30, "expected_duration_sec": 2,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil || result.Status != reducer.StatusApplied {
		t.Fatalf("seed preview status=%s err=%v", result.Status, err)
	}
}

func installPreviewQAMediaFixture(t *testing.T) {
	t.Helper()
	fakeBin := t.TempDir()
	barrier := t.TempDir()
	ffprobe := fmt.Sprintf(`#!/bin/sh
set -eu
barrier=%q
if [ ! -f "$barrier/released" ]; then
  : > "$barrier/entered.$$"
  while [ "$(ls "$barrier"/entered.* 2>/dev/null | wc -l | tr -d ' ')" -lt 3 ]; do
    sleep 0.01
  done
  : > "$barrier/released"
fi
cat <<'JSON'
{"format":{"duration":"2.0"},"streams":[{"codec_type":"video","duration":"2.0","avg_frame_rate":"30/1","width":320,"height":240},{"codec_type":"audio","duration":"2.0"}]}
JSON
`, barrier)
	ffmpeg := `#!/bin/sh
set -eu
case "$*" in
  *blackdetect*)
    printf 'black_start:0 black_end:0.5 black_duration:0.5\n' >&2
    ;;
  *freezedetect*)
    printf 'freeze_start: 1.0\nfreeze_end: 1.7 | freeze_duration: 0.7\n' >&2
    ;;
  *silencedetect*)
    printf 'silence_start: 0\nsilence_end: 0.6 | silence_duration: 0.6\n' >&2
    ;;
esac
`
	for name, body := range map[string]string{"ffprobe": ffprobe, "ffmpeg": ffmpeg} {
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUSHES_FFMPEG_SANDBOX", "0")
	t.Setenv("RUSHES_PREVIEW_QA_BARRIER", barrier)
}

func previewQABarrierEntrants(t *testing.T) int {
	t.Helper()
	barrier := os.Getenv("RUSHES_PREVIEW_QA_BARRIER")
	entrants, err := filepath.Glob(filepath.Join(barrier, "entered.*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entrants)
}

func previewQAIssueFrames(t *testing.T, issue map[string]interface{}) [2]int {
	t.Helper()
	raw, ok := issue["frames"].([]any)
	if !ok || len(raw) != 2 {
		t.Fatalf("preview issue frames=%#v", issue["frames"])
	}
	start, startOK := raw[0].(float64)
	end, endOK := raw[1].(float64)
	if !startOK || !endOK {
		t.Fatalf("preview issue frame types=%T,%T", raw[0], raw[1])
	}
	return [2]int{int(start), int(end)}
}
