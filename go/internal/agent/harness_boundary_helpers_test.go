package agent

import (
	"context"
	"testing"

	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

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
			if !containsBoundaryKeyword(err, test.want) {
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

func TestHarnessBoundaryTraceMetadataInputs(t *testing.T) {
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
