package agentexec

import (
	"errors"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/agenttest"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestTimelineOpFailureReturnsAtomicRepairFacts(t *testing.T) {
	t.Parallel()
	document := timeline.Empty("draft_recovery", 1)
	document.Tracks[0].Clips = []timeline.Clip{{
		TimelineClipID: "clip_1",
		TrackID:        document.Tracks[0].TrackID,
	}}

	if result, ok := TimelineOpFailure(
		"timeline.delete",
		errors.New("unclassified"),
		map[string]any{"kind": "delete_clip"},
		document,
	); ok || result.Status != "" {
		t.Fatalf("普通错误不应被时间线恢复器接管: result=%#v ok=%v", result, ok)
	}

	spec, exists := timeline.LookupOpSpec("trim_clip_edge")
	if !exists {
		t.Fatal("trim_clip_edge spec missing")
	}
	fieldResult, ok := TimelineOpFailure(
		"timeline.update",
		&timeline.OpFieldError{
			Kind: "trim_clip_edge", Field: "timeline_frame",
			Reason: "缺少必填字段", Spec: spec,
		},
		map[string]any{"kind": "trim_clip_edge"},
		document,
	)
	if !ok ||
		fieldResult.Data["error_code"] != string(rushestools.ErrCodeTimelineOpFieldError) ||
		fieldResult.Data["failed_op"] == nil ||
		fieldResult.Data["expected_schema"] == nil ||
		fieldResult.Data["correct_example"] == nil {
		t.Fatalf("known field failure=%#v ok=%v", fieldResult, ok)
	}

	unknownResult, ok := TimelineOpFailure(
		"timeline.update",
		&timeline.OpFieldError{Kind: "unknown", Field: "kind", Reason: "不受支持"},
		map[string]any{"kind": "unknown"},
		document,
	)
	if !ok || unknownResult.Data["op_catalog"] == nil ||
		unknownResult.Data["failed_op"] == nil {
		t.Fatalf("unknown field failure=%#v ok=%v", unknownResult, ok)
	}
	catalog, ok := unknownResult.Data["op_catalog"].([]map[string]string)
	if !ok || len(catalog) == 0 {
		t.Fatalf("unknown field catalog=%#v", unknownResult.Data["op_catalog"])
	}
	for _, entry := range catalog {
		owner, exposed := rushestools.TimelineAtomicToolForKind(entry["kind"])
		if !exposed || owner != "timeline.update" {
			t.Errorf("unknown field 暴露跨家族 kind=%s owner=%s", entry["kind"], owner)
		}
	}

	for _, test := range []struct {
		name  string
		err   *timeline.SemanticError
		op    map[string]any
		check func(t *testing.T, data map[string]any)
	}{
		{
			name: "missing clip",
			err:  &timeline.SemanticError{Kind: timeline.SemanticClipNotFound, ClipID: "missing"},
			op:   map[string]any{"kind": "delete_clip", "timeline_clip_id": "missing"},
			check: func(t *testing.T, data map[string]any) {
				available := data["available_timeline_clip_ids"].(map[string][]string)
				if len(available[document.Tracks[0].TrackID]) != 1 ||
					available[document.Tracks[0].TrackID][0] != "clip_1" ||
					data["expected_schema"] == nil || data["correct_example"] == nil {
					t.Fatalf("clip facts=%#v", data)
				}
			},
		},
		{
			name: "frame range",
			err: &timeline.SemanticError{
				Kind: timeline.SemanticFrameRange, ClipID: "clip_1",
				ProvidedFrame: 30, TimelineStartFrame: 0, TimelineEndFrame: 30,
				SourceStartFrame: 10, SourceEndFrame: 40,
			},
			op: map[string]any{
				"kind": "split_clip", "timeline_clip_id": "clip_1", "split_frame": 30,
			},
			check: func(t *testing.T, data map[string]any) {
				actual := data["actual_clip_range"].(map[string]any)
				if actual["provided_frame"] != 30 || actual["source_start_frame"] != 10 {
					t.Fatalf("range facts=%#v", data)
				}
			},
		},
		{
			name: "locked track",
			err:  &timeline.SemanticError{Kind: timeline.SemanticTrackLocked, TrackID: "visual_base"},
			op:   map[string]any{"kind": "set_track_state", "track_id": "visual_base"},
			check: func(t *testing.T, data map[string]any) {
				if data["locked_track_id"] != "visual_base" {
					t.Fatalf("locked facts=%#v", data)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			toolName, exists := rushestools.TimelineAtomicToolForKind(test.op["kind"].(string))
			if !exists {
				t.Fatalf("op 未归属原子工具: %#v", test.op)
			}
			result, ok := TimelineOpFailure(toolName, test.err, test.op, document)
			if !ok || result.Status != string(rushestools.StatusFailed) ||
				result.Data["error_code"] != string(rushestools.ErrCodeTimelineOpSemanticError) ||
				result.Data["current_timeline_unchanged"] != true {
				t.Fatalf("result=%#v ok=%v", result, ok)
			}
			test.check(t, result.Data)
		})
	}
}

func TestAtomicLinkedSplitRangeFailureReturnsMemberFactsWithoutVersion(t *testing.T) {
	t.Parallel()
	const draftID = "draft_atomic_linked_split_range"
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "talk", AssetKind: "video", HasAudio: true,
		SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document.Tracks[2].Clips[0].TimelineEndFrame = 20
	document.Tracks[2].Clips[0].SourceEndFrame = 20
	if persisted, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, document, "linked_split_fixture", nil); persistErr != nil || persisted.Status != "validation_failed" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	before, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.ExecuteTool(
		rushestools.WithDraftID(t.Context(), draftID),
		"timeline.split",
		rushestools.TimelineSplitInput{
			"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	actual, _ := result.Data["actual_clip_range"].(map[string]any)
	if result.Status != string(rushestools.StatusFailed) ||
		result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
		actual["timeline_end_frame"] != 20 || actual["provided_frame"] != 30 ||
		result.Data["current_timeline_unchanged"] != true {
		t.Fatalf("result=%#v", result)
	}
	after, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || after.Version != before.Version {
		t.Fatalf("linked split failure persisted: before=%d after=%d err=%v", before.Version, after.Version, err)
	}
}

func TestAtomicLinkedLockedTrackFailurePreservesTimeline(t *testing.T) {
	t.Parallel()
	const draftID = "draft_atomic_linked_lock"
	database := agenttest.AgentTestDatabase(t)
	agenttest.CreateAgentDraft(t, database, draftID)
	exec, err := newTestExecutor(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := agenttest.ComposeTimeline(draftID, 1, []agenttest.TimelineSelection{{
		AssetID: "talk", AssetKind: "video", HasAudio: true,
		SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := seedTimelineVersion(exec,
		t.Context(), draftID, document, "linked_lock_fixture", nil); persistErr != nil || persisted.Status != string(rushestools.StatusSucceeded) {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	lock := executeAtomicTimelineTool(t, exec, ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "set_track_state", "track_id": "original_audio", "locked": true,
	})
	assertAtomicTimelineResult(t, lock, "set_track_state")
	before, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.ExecuteTool(ctx, "timeline.update", rushestools.TimelineUpdateInput{
		"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
		"timeline_frame": 30, "edge": "end",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != string(rushestools.StatusFailed) ||
		result.Data["semantic_error_kind"] != timeline.SemanticTrackLocked ||
		result.Data["locked_track_id"] != "original_audio" ||
		result.Data["current_timeline_unchanged"] != true {
		t.Fatalf("result=%#v", result)
	}
	after, err := timeline.Latest(t.Context(), database, draftID)
	if err != nil || after.Version != before.Version {
		t.Fatalf("locked failure persisted: before=%d after=%d err=%v", before.Version, after.Version, err)
	}
}
