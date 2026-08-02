package agent

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestHarnessBoundaryRecognizesAutomaticTimelineCheckEvidence(t *testing.T) {
	tests := []struct {
		name    string
		message *schema.Message
		want    bool
	}{
		{name: "nil"},
		{name: "not tool", message: schema.UserMessage("done")},
		{name: "not mutation", message: &schema.Message{Role: schema.Tool, ToolName: "shot.search", Content: `{}`}},
		{name: "malformed mutation", message: &schema.Message{Role: schema.Tool, ToolName: "timeline.update", Content: `{`}},
		{name: "missing automatic check", message: &schema.Message{Role: schema.Tool, ToolName: "timeline.update", Content: `{"data":{}}`}},
		{name: "failed automatic check", message: &schema.Message{Role: schema.Tool, ToolName: "timeline.update", Content: `{"data":{"automatic_timeline_check":{"status":"failed","data":{"timeline_id":"draft:v2"}}}}`}},
		{name: "invalid automatic timeline", message: &schema.Message{Role: schema.Tool, ToolName: "timeline.update", Content: `{"data":{"automatic_timeline_check":{"status":"succeeded","data":{"timeline_id":"draft"}}}}`}},
		{name: "succeeded", message: &schema.Message{Role: schema.Tool, ToolName: "timeline.update", Content: `{"data":{"automatic_timeline_check":{"status":"succeeded","data":{"timeline_id":"draft:v2"}}}}`}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := automaticTimelineCheckSucceeded(test.message); got != test.want {
				t.Fatalf("got=%v want=%v message=%#v", got, test.want, test.message)
			}
		})
	}
}

func TestHarnessBoundaryTerminalTruthHelpers(t *testing.T) {
	for _, test := range []struct {
		kind string
		want string
	}{
		{kind: "timeline_check_missing", want: "尚未通过同版本"},
		{kind: "timeline_check_stale", want: "终态检查已过期"},
		{kind: "timeline_mutation_unverified", want: "缺少有效的 timeline_id"},
		{kind: "timeline_check_unverified", want: "缺少有效的 timeline_id"},
		{kind: "timeline_latest_changed", want: "时间线已变化"},
		{kind: "unknown", want: "终态真值门禁未通过"},
	} {
		t.Run("guard error "+test.kind, func(t *testing.T) {
			err := (&terminalReplyGuardError{
				kind: test.kind, mutationTimelineID: "draft:v2",
				checkTimelineID: "draft:v1", latestTimelineID: "draft:v3",
			}).Error()
			if !containsSurfaceKeyword(err, test.want) {
				t.Fatalf("error=%q want substring=%q", err, test.want)
			}
		})
	}

	value := rushestools.ToolResult{
		Status: string(rushestools.StatusSucceeded),
		Data:   map[string]any{"timeline_id": "draft:v2"},
	}
	for _, test := range []struct {
		name  string
		input any
		want  bool
	}{
		{name: "value", input: value, want: true},
		{name: "pointer", input: &value, want: true},
		{name: "nil pointer", input: (*rushestools.ToolResult)(nil)},
		{name: "unsupported", input: map[string]any{"status": "succeeded"}},
	} {
		t.Run("tool result "+test.name, func(t *testing.T) {
			result, ok := terminalTruthToolResult(test.input)
			if ok != test.want || (ok && result.Status != value.Status) {
				t.Fatalf("result=%#v ok=%v want=%v", result, ok, test.want)
			}
		})
	}

	state := newTerminalTimelineTruthState()
	state.recordToolResult("timeline.update", "succeeded", "not a tool result")
	state.recordTimelineCheckResult("draft:v2", "failed")
	state.recordTimelineCheckResult("draft:v2", string(rushestools.StatusSucceeded))
	if snapshot := state.snapshot(); snapshot.checkTimelineID != "draft:v2" ||
		snapshot.checkResult.Status != "" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestHarnessBoundaryTerminalGuardRejectsMissingAndStaleChecks(t *testing.T) {
	assertKind := func(t *testing.T, state *terminalTimelineTruthState, want string) {
		t.Helper()
		ctx := withTerminalTimelineTruthState(context.Background(), state)
		err := (&Service{}).terminalReplyGuard(ctx, "draft")
		guardErr, ok := err.(*terminalReplyGuardError)
		if !ok || guardErr.kind != want {
			t.Fatalf("err=%#v want kind=%q", err, want)
		}
	}

	missing := newTerminalTimelineTruthState()
	missing.recordMutationTimelineID("draft:v1")
	assertKind(t, missing, "timeline_check_missing")

	stale := newTerminalTimelineTruthState()
	stale.recordMutationTimelineID("draft:v1")
	stale.recordTimelineCheckResult("draft:v1", string(rushestools.StatusSucceeded))
	stale.recordMutationTimelineID("draft:v2")
	assertKind(t, stale, "timeline_check_stale")
}

func TestHarnessBoundaryProgressiveSurfaceTransitions(t *testing.T) {
	budgetReminder := schema.SystemMessage("【工具预算提醒】请先用 plan.update 固化后续步骤")
	if !needsPlanUpdateSurface([]*schema.Message{budgetReminder}) {
		t.Fatal("budget reminder must require plan.update")
	}
	planResult := schema.ToolMessage(
		`{"status":"succeeded"}`, "plan-call", schema.WithToolName("plan.update"),
	)
	if needsPlanUpdateSurface([]*schema.Message{
		budgetReminder, schema.UserMessage("继续"), planResult,
	}) {
		t.Fatal("successful plan.update must satisfy the reminder")
	}
	automaticCheck := &schema.Message{
		Role: schema.Tool, ToolName: "timeline.update",
		Content: `{"data":{"automatic_timeline_check":{"status":"succeeded","data":{"timeline_id":"draft:v2"}}}}`,
	}
	if !successfulToolCallSinceLatestUser(
		[]*schema.Message{schema.UserMessage("修剪时间线"), automaticCheck}, "timeline.check",
	) {
		t.Fatal("automatic exact-version check must satisfy timeline.check prerequisite")
	}

	deepSearch := schema.ToolMessage(
		`{"status":"succeeded","candidates":[{}]}`,
		"deep-search", schema.WithToolName("shot.deep_search"),
	)
	for _, test := range []struct {
		name string
		text string
		want rushestools.Surface
	}{
		{name: "talking head", text: "为口播检索镜头", want: rushestools.SurfaceTalkingHead},
		{name: "beat edit", text: "为卡点剪辑检索镜头", want: rushestools.SurfaceBeatEdit},
		{name: "timeline edit", text: "为时间线检索镜头并插入", want: rushestools.SurfaceTimelineEdit},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := []*schema.Message{schema.UserMessage(test.text), deepSearch}
			if got := recentSuccessfulWorkflowSurface(messages, test.text); got != test.want {
				t.Fatalf("surface=%d want=%d", got, test.want)
			}
		})
	}
}

func TestHarnessBoundaryRecognizesSuccessfulWorkflowResults(t *testing.T) {
	tests := []struct {
		name    string
		message *schema.Message
		want    bool
	}{
		{name: "shot malformed", message: &schema.Message{ToolName: "shot.search", Content: `{`}},
		{name: "shot empty", message: &schema.Message{ToolName: "shot.search", Content: `{"shots":[]}`}},
		{name: "shot found", message: &schema.Message{ToolName: "shot.search", Content: `{"shots":[{}]}`}, want: true},
		{name: "deep malformed", message: &schema.Message{ToolName: "shot.deep_search", Content: `{`}},
		{name: "deep failed", message: &schema.Message{ToolName: "shot.deep_search", Content: `{"status":"failed","candidates":[{}]}`}},
		{name: "deep empty", message: &schema.Message{ToolName: "shot.deep_search", Content: `{"status":"succeeded","candidates":[]}`}},
		{name: "deep found", message: &schema.Message{ToolName: "shot.deep_search", Content: `{"status":"succeeded","candidates":[{}]}`}, want: true},
		{name: "generic malformed", message: &schema.Message{ToolName: "plan.update", Content: `{`}},
		{name: "generic failed", message: &schema.Message{ToolName: "plan.update", Content: `{"status":"failed"}`}},
		{name: "generic succeeded", message: &schema.Message{ToolName: "plan.update", Content: `{"status":"succeeded"}`}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowToolCallSucceeded(test.message); got != test.want {
				t.Fatalf("got=%v want=%v message=%#v", got, test.want, test.message)
			}
		})
	}
}

func TestHarnessBoundaryTraceMetadataInputs(t *testing.T) {
	index := 7
	for _, test := range []struct {
		name string
		call schema.ToolCall
		want string
	}{
		{name: "id", call: schema.ToolCall{ID: "call-1", Index: &index}, want: "call-1"},
		{name: "index", call: schema.ToolCall{Index: &index}, want: "idx:7"},
		{name: "name", call: schema.ToolCall{Function: schema.FunctionCall{Name: "timeline.update"}}, want: "name:timeline.update"},
	} {
		t.Run("dedup "+test.name, func(t *testing.T) {
			if got := lateToolCallDedupKey(test.call); got != test.want {
				t.Fatalf("got=%q want=%q", got, test.want)
			}
		})
	}

	value := rushestools.PreviewCheckInput{PreviewID: " preview-1 ", Check: " black "}
	for _, test := range []struct {
		name        string
		tool        string
		input       any
		wantPreview string
		wantCheck   string
	}{
		{name: "wrong tool", tool: "timeline.check", input: value},
		{name: "value", tool: "preview.check", input: value, wantPreview: "preview-1", wantCheck: "black"},
		{name: "pointer", tool: "preview.check", input: &value, wantPreview: "preview-1", wantCheck: "black"},
		{name: "nil pointer", tool: "preview.check", input: (*rushestools.PreviewCheckInput)(nil)},
		{name: "map", tool: "preview.check", input: map[string]any{"preview_id": " preview-2 ", "check": " visual "}, wantPreview: "preview-2", wantCheck: "visual"},
		{name: "unsupported", tool: "preview.check", input: "preview-3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := previewIDFromToolReport(test.tool, test.input); got != test.wantPreview {
				t.Fatalf("preview=%q want=%q", got, test.wantPreview)
			}
			if got := previewCheckFromToolReport(test.tool, test.input); got != test.wantCheck {
				t.Fatalf("check=%q want=%q", got, test.wantCheck)
			}
		})
	}
}
