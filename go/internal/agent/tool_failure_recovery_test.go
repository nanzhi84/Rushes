package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/compose"
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

// TestAgentRecoveryMiddlewareFailuresCarryRecovery 覆盖 agent 侧恢复中间件产出的结构化失败
// （未注册工具、执行错误、重复/耗尽拦截），断言都带非空 recovery。
func TestAgentRecoveryMiddlewareFailuresCarryRecovery(t *testing.T) {
	t.Parallel()
	unknown, err := unknownToolRecoveryHandler(t.Context(), "fake.tool", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	assertFailureJSONHasRecovery(t, "unknown_tool", unknown)
	assertFailureJSONHasRecovery(t, "execution_error",
		executionErrorOutput("timeline.validate", errors.New("boom"), 1, false))
	assertFailureJSONHasRecovery(t, "blocked_duplicate",
		blockedToolCallOutput(&compose.ToolInput{Name: "timeline.validate"}, recoveryDecision{duplicate: true}))
	assertFailureJSONHasRecovery(t, "blocked_exhausted",
		blockedToolCallOutput(&compose.ToolInput{Name: "timeline.validate"}, recoveryDecision{exhausted: true}))
}

// TestExecutorStructuredFailuresCarryRecovery 通过 Service.ExecuteTool 触发领域层代表性结构化
// 失败（plan.update 参数缺失、apply_patches 字段错误与未知 kind），断言 Data.recovery 非空。
// 覆盖 planUpdateFailure 兜底与 timeline op 恢复两条主线。
func TestExecutorStructuredFailuresCarryRecovery(t *testing.T) {
	t.Parallel()
	service, _, ctx := timelineOpRecoveryFixture(t, "draft_recovery_coverage")

	cases := []struct {
		name  string
		tool  string
		input any
	}{
		{"plan_required", "plan.update", rushestools.PlanUpdateInput{}},
		{"apply_patches_field_error", "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{
			"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001", "target_frame": 10, "edge": "end",
		}}}},
		{"apply_patches_unknown_kind", "timeline.apply_patches", rushestools.TimelinePatchBatchInput{Ops: []rushestools.TimelineOp{{
			"kind": "remove_clip", "timeline_clip_id": "clip_v1_001",
		}}}},
		// 经 edit_talking_head / recut_to_beats 的 failed 闭包路径，验证闭包 recovery 兜底
		// 已让这两条主线的结构化失败机械带 recovery（覆盖 talking_head.go:1752 同类闭包）。
		{"talking_head_no_decisions", "timeline.edit_talking_head", rushestools.TalkingHeadEditInput{ARollTimelineClipID: "clip_v1_001"}},
		{"recut_missing_inputs", "timeline.recut_to_beats", rushestools.TimelineBeatRecutInput{}},
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
