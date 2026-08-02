package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/nanzhi84/Rushes/go/internal/agentexec"
	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// isStructuredFailureStatus 判定一个工具结果状态是否属于「结构化失败」域（应当携带 recovery）。
func isStructuredFailureStatus(status string) bool {
	return status == string(rushestools.StatusFailed) || status == string(rushestools.StatusValidationFailed)
}

func assertFailureDataHasRecovery(t *testing.T, label string, data map[string]any) {
	t.Helper()
	if recovery, _ := data["recovery"].(string); strings.TrimSpace(recovery) == "" {
		t.Errorf("%s 结构化失败缺少非空 recovery: %#v", label, data)
	}
}

func assertFailureJSONHasRecovery(t *testing.T, label, raw string) {
	t.Helper()
	var payload struct {
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("%s 无法解析工具失败 JSON: %v", label, err)
	}
	if !isStructuredFailureStatus(payload.Status) {
		t.Fatalf("%s 期望结构化失败状态，实际 status=%q", label, payload.Status)
	}
	assertFailureDataHasRecovery(t, label, payload.Data)
}

// TestSharedToolFailureAlwaysCarriesRecovery 锁定共享构造器不变量（#95 T5）：即便传入空
// recovery，也回退到非空兜底，从而让「结构化失败必带非空 recovery」成为构造期保证。
func TestSharedToolFailureAlwaysCarriesRecovery(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"", "具体恢复指引"} {
		failure := rushestools.ToolFailure(
			rushestools.StatusValidationFailed, "obs", rushestools.ErrCodeUnknownTool, recovery, nil,
		)
		if !isStructuredFailureStatus(failure.Status) {
			t.Fatalf("ToolFailure status=%q 非结构化失败", failure.Status)
		}
		assertFailureDataHasRecovery(t, "ToolFailure(recovery="+recovery+")", failure.Data)
	}
}

// TestAgentRecoveryMiddlewareFailuresCarryRecovery 覆盖 agent 中间件产出的结构化失败，
// 断言独立失败都带非空 recovery。
func TestAgentRecoveryMiddlewareFailuresCarryRecovery(t *testing.T) {
	t.Parallel()
	unknown, err := unknownToolRecoveryHandler(t.Context(), "fake.tool", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	assertFailureJSONHasRecovery(t, "unknown_tool", unknown)
	assertFailureJSONHasRecovery(t, "execution_error",
		executionErrorOutput("timeline.check", errors.New("boom"), 1, false))
}

// TestExecutorStructuredFailuresCarryRecovery 通过 Service.ExecuteTool 触发领域层代表性结构化
// 失败（plan.update 参数缺失、原子更新字段错误与未知 kind），断言 Data.recovery 非空。
// 覆盖 planUpdateFailure 兜底与原子 timeline op 恢复两条主线。
func TestExecutorStructuredFailuresCarryRecovery(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_recovery_coverage"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "fixture", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service,
		t.Context(), draftID, document, "recovery_coverage_fixture", nil); err != nil {
		t.Fatal(err)
	}
	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)

	cases := []struct {
		name  string
		tool  string
		input any
	}{
		{"plan_required", "plan.update", rushestools.PlanUpdateInput{}},
		{"update_field_error", "timeline.update", rushestools.TimelineUpdateInput{
			"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
			"target_frame": 10, "edge": "end",
		}},
		{"update_unknown_kind", "timeline.update", rushestools.TimelineUpdateInput{
			"kind": "remove_clip", "timeline_clip_id": "clip_v1_001",
		}},
	}
	for _, test := range cases {
		raw, err := service.ExecuteTool(ctx, test.tool, test.input)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		result := raw.(rushestools.ToolResult)
		if !isStructuredFailureStatus(result.Status) {
			t.Fatalf("%s 期望结构化失败，实际 status=%q data=%#v", test.name, result.Status, result.Data)
		}
		assertFailureDataHasRecovery(t, test.name, result.Data)
	}
}

func TestRegistryRouterTimelineFieldFailuresPreserveFailedOperation(t *testing.T) {
	t.Parallel()
	database := agenttest.AgentTestDatabase(t)
	const draftID = "draft_registry_failure_recovery"
	agenttest.CreateAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "asset_recovery", AssetKind: "video",
		SourceStartFrame: 0, SourceEndFrame: 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedTimelineVersion(service,
		t.Context(), draftID, document, "registry_recovery_fixture", nil,
	); err != nil {
		t.Fatal(err)
	}
	router, err := newToolRouter(
		t.Context(),
		compose.ToolsNodeConfig{Tools: service.Tools().EinoTools(true, false)},
		service.Tools().Spec,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withTestTurnLeaseSession(t, service, t.Context(), draftID)
	ctx = agentexec.WithTurnInteractionState(
		ctx, agentexec.NewTurnInteractionState(service.indexedResources),
	)
	results, err := router.Invoke(ctx, &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{
			{
				ID: "missing_field",
				Function: schema.FunctionCall{
					Name: "timeline.update",
					Arguments: `{"kind":"trim_clip_edge","timeline_clip_id":"clip_v1_001",` +
						`"target_frame":10,"edge":"end"}`,
				},
			},
			{
				ID: "unknown_field",
				Function: schema.FunctionCall{
					Name:      "timeline.delete",
					Arguments: `{"kind":"delete_clip","timeline_clip_id":"clip_v1_001","future":true}`,
				},
			},
			{
				ID: "cross_family",
				Function: schema.FunctionCall{
					Name:      "timeline.update",
					Arguments: `{"kind":"delete_clip","timeline_clip_id":"clip_v1_001"}`,
				},
			},
		},
	})
	if err != nil || len(results) != 3 {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	for index, resultMessage := range results {
		var result rushestools.ToolResult
		if err := json.Unmarshal([]byte(resultMessage.Content), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != string(rushestools.StatusFailed) {
			t.Fatalf("result[%d]=%#v", index, result)
		}
		failedOp, ok := result.Data["failed_op"].(map[string]any)
		if !ok || failedOp["kind"] == nil {
			t.Fatalf("result[%d] 丢失 failed_op: %#v", index, result)
		}
		if index != 2 {
			if result.Data["expected_schema"] == nil || result.Data["correct_example"] == nil {
				t.Fatalf("result[%d] 缺少同家族恢复 schema: %#v", index, result)
			}
			continue
		}
		if result.Data["required_tool"] != "timeline.delete" ||
			result.Data["op_catalog"] == nil ||
			result.Data["expected_schema"] != nil ||
			result.Data["correct_example"] != nil {
			t.Fatalf("跨家族恢复指向错误: %#v", result)
		}
	}
}

func TestFailureDecorationIncludesRemainingToolRoundsOnlyOnFailure(t *testing.T) {
	t.Parallel()
	budget := newTurnBudgetState(5)
	ctx := withTurnBudgetState(t.Context(), budget)
	budget.beginModelCall()
	budget.beginModelCall()
	raw := decorateToolFailure(
		ctx,
		&compose.ToolInput{Name: "timeline.update", Arguments: `{}`},
		`{"status":"failed","observation":"bad"}`,
		1,
	)
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	data := payload["data"].(map[string]any)
	if data["remaining_tool_rounds"] != float64(4) {
		t.Fatalf("payload=%#v", payload)
	}
	success := rushestools.ToolResult{Status: "succeeded", Observation: "ok", Data: map[string]any{}}
	if _, exists := success.Data["remaining_tool_rounds"]; exists {
		t.Fatal("remaining_tool_rounds leaked into success payload")
	}
}
