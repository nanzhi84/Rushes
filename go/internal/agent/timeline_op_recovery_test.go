package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func TestApplyPatchSemanticFailuresReturnCurrentTimelineFacts(t *testing.T) {
	t.Parallel()

	t.Run("clip_not_found", func(t *testing.T) {
		service, database, ctx := timelineOpRecoveryFixture(t, "draft_semantic_missing")
		before, _ := timeline.Latest(t.Context(), database, "draft_semantic_missing")
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "delete_clip", "timeline_clip_id": "clip_missing",
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		available, _ := result.Data["available_timeline_clip_ids"].(map[string][]string)
		if result.Status != "failed" || result.Data["failed_op"] == nil ||
			result.Data["expected_schema"] == nil || len(available["visual_base"]) != 1 ||
			available["visual_base"][0] != "clip_v1_001" {
			t.Fatalf("result=%#v", result)
		}
		after, _ := timeline.Latest(t.Context(), database, "draft_semantic_missing")
		if after.Version != before.Version {
			t.Fatalf("semantic failure changed timeline: %d -> %d", before.Version, after.Version)
		}
	})

	t.Run("frame_range", func(t *testing.T) {
		service, _, ctx := timelineOpRecoveryFixture(t, "draft_semantic_range")
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
			"timeline_frame": 60, "edge": "end",
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		actual, _ := result.Data["actual_clip_range"].(map[string]any)
		if result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
			actual["timeline_start_frame"] != 0 || actual["timeline_end_frame"] != 60 ||
			actual["provided_frame"] != 60 {
			t.Fatalf("result=%#v", result)
		}
	})

	t.Run("reorder_clip_not_found", func(t *testing.T) {
		service, database, ctx := timelineOpRecoveryFixture(t, "draft_reorder_missing")
		before, _ := timeline.Latest(t.Context(), database, "draft_reorder_missing")
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "reorder_clip", "timeline_clip_id": "missing", "target_frame": 0,
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		available, _ := result.Data["available_timeline_clip_ids"].(map[string][]string)
		if result.Data["semantic_error_kind"] != timeline.SemanticClipNotFound ||
			len(available["visual_base"]) != 1 || available["visual_base"][0] != "clip_v1_001" {
			t.Fatalf("result=%#v", result)
		}
		after, _ := timeline.Latest(t.Context(), database, "draft_reorder_missing")
		if after.Version != before.Version {
			t.Fatalf("reorder failure persisted: %d -> %d", before.Version, after.Version)
		}
	})

	t.Run("reorder_frame_range", func(t *testing.T) {
		service, _, ctx := timelineOpRecoveryFixture(t, "draft_reorder_range")
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "reorder_clip", "timeline_clip_id": "clip_v1_001", "target_frame": 61,
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		actual, _ := result.Data["actual_clip_range"].(map[string]any)
		if result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
			actual["timeline_start_frame"] != 0 || actual["timeline_end_frame"] != 60 ||
			actual["provided_frame"] != 61 {
			t.Fatalf("result=%#v", result)
		}
	})

	t.Run("split_frame_range", func(t *testing.T) {
		service, database, ctx := timelineOpRecoveryFixture(t, "draft_split_range")
		before, _ := timeline.Latest(t.Context(), database, "draft_split_range")
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 60,
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		actual, _ := result.Data["actual_clip_range"].(map[string]any)
		if result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
			actual["timeline_start_frame"] != 0 || actual["timeline_end_frame"] != 60 ||
			actual["provided_frame"] != 60 {
			t.Fatalf("result=%#v", result)
		}
		after, _ := timeline.Latest(t.Context(), database, "draft_split_range")
		if after.Version != before.Version {
			t.Fatalf("split failure persisted: %d -> %d", before.Version, after.Version)
		}
	})

	t.Run("locked_track", func(t *testing.T) {
		service, _, ctx := timelineOpRecoveryFixture(t, "draft_semantic_locked")
		if raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "set_track_state", "track_id": "visual_base", "locked": true,
		}}); err != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
			t.Fatalf("lock result=%#v err=%v", raw, err)
		}
		raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
			"kind": "set_playback_rate", "timeline_clip_id": "clip_v1_001", "playback_rate": 1.25,
		}})
		if err != nil {
			t.Fatal(err)
		}
		result := raw.(rushestools.ToolResult)
		if result.Data["semantic_error_kind"] != timeline.SemanticTrackLocked ||
			result.Data["locked_track_id"] != "visual_base" {
			t.Fatalf("result=%#v", result)
		}
	})
}

func TestLinkedSplitRangeFailureReturnsFailedMemberFacts(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_linked_split_range")
	document, err := timeline.ComposeInitial("draft_linked_split_range", 2, []timeline.Selection{{
		AssetID: "talk", AssetKind: "video", HasAudio: true, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for trackIndex := range document.Tracks {
		if document.Tracks[trackIndex].TrackID == "original_audio" {
			document.Tracks[trackIndex].Clips[0].TimelineEndFrame = 20
			document.Tracks[trackIndex].Clips[0].SourceEndFrame = 20
		}
	}
	if persisted, persistErr := service.persistTimeline(t.Context(), "draft_linked_split_range", document, "linked_split_fixture"); persistErr != nil || persisted.Status != "validation_failed" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	before, _ := timeline.Latest(t.Context(), database, "draft_linked_split_range")
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
		"kind": "split_clip", "timeline_clip_id": "clip_v2_001", "split_frame": 30,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	actual, _ := result.Data["actual_clip_range"].(map[string]any)
	if result.Status != "failed" || result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
		actual["timeline_end_frame"] != 20 || actual["provided_frame"] != 30 {
		t.Fatalf("result=%#v", result)
	}
	after, _ := timeline.Latest(t.Context(), database, "draft_linked_split_range")
	if after.Version != before.Version {
		t.Fatalf("linked split failure persisted: %d -> %d", before.Version, after.Version)
	}
}

func TestLinkedLockedTrackFailureReturnsSemanticJITAndPreservesTimeline(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_semantic_linked_lock")
	document, err := timeline.ComposeInitial("draft_semantic_linked_lock", 2, []timeline.Selection{{
		AssetID: "talk", AssetKind: "video", HasAudio: true, SourceEndFrame: 60, Role: "a_roll",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result, persistErr := service.persistTimeline(t.Context(), "draft_semantic_linked_lock", document, "linked_fixture"); persistErr != nil || result.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", result, persistErr)
	}
	if raw, lockErr := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
		"kind": "set_track_state", "track_id": "original_audio", "locked": true,
	}}); lockErr != nil || raw.(rushestools.ToolResult).Status != "succeeded" {
		t.Fatalf("lock=%#v err=%v", raw, lockErr)
	}
	before, _ := timeline.Latest(t.Context(), database, "draft_semantic_linked_lock")
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{Op: rushestools.TimelineOp{
		"kind": "trim_clip_edge", "timeline_clip_id": "clip_v2_001", "timeline_frame": 30, "edge": "end",
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != "failed" || result.Data["semantic_error_kind"] != timeline.SemanticTrackLocked ||
		result.Data["locked_track_id"] != "original_audio" || result.Data["current_timeline_unchanged"] != true {
		t.Fatalf("result=%#v", result)
	}
	after, _ := timeline.Latest(t.Context(), database, "draft_semantic_linked_lock")
	if after.Version != before.Version {
		t.Fatalf("locked failure persisted: %d -> %d", before.Version, after.Version)
	}
}

func TestComposeInitialFailuresIncludeAssetFacts(t *testing.T) {
	t.Parallel()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, "draft_compose_facts")
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(
			asset_id,storage_mode,reference_path,kind,source,filename,hash,size,
			probe_json,ingest_status,understanding_status,usable
		) VALUES
			('compose_audio','reference','/tmp/audio.wav','audio','local_path','audio.wav','compose_audio',1,'{"duration_sec":10}','ready','none',1),
			('compose_video','reference','/tmp/video.mp4','video','local_path','video.mp4','compose_video',1,'{"duration_sec":10}','ready','none',1)`); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(t.Context(), database, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx := rushestools.WithDraftID(t.Context(), "draft_compose_facts")

	for _, test := range []struct {
		name string
		clip rushestools.ComposeClip
		kind string
	}{
		{name: "wrong_kind", clip: rushestools.ComposeClip{AssetID: "compose_audio", SourceEndFrame: 30, Role: "a_roll"}, kind: "audio"},
		{name: "range", clip: rushestools.ComposeClip{AssetID: "compose_video", SourceEndFrame: 301, Role: "a_roll"}, kind: "video"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := service.ExecuteTool(ctx, "timeline.compose_initial", rushestools.ComposeInitialInput{Clips: []rushestools.ComposeClip{test.clip}})
			if err != nil {
				t.Fatal(err)
			}
			result := raw.(rushestools.ToolResult)
			facts, _ := result.Data["asset_facts"].(map[string]any)
			if result.Status != "failed" || result.Data["failed_clip"] == nil ||
				facts["kind"] != test.kind || facts["duration_frames"] != 300 {
				t.Fatalf("result=%#v", result)
			}
		})
	}
}

func TestFailureDecorationIncludesRemainingToolRoundsOnlyOnFailure(t *testing.T) {
	t.Parallel()
	budget := newTurnBudgetState(5)
	ctx := withTurnBudgetState(t.Context(), budget)
	budget.beginModelCall()
	budget.beginModelCall()
	raw := decorateToolFailure(ctx, &compose.ToolInput{Name: "timeline.apply_patch", Arguments: `{}`},
		`{"status":"failed","observation":"bad"}`, 1)
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

func TestApplyPatchFieldFailureReturnsExactJITSchemaAndExample(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_op_jit_single")
	before, err := timeline.Latest(t.Context(), database, "draft_op_jit_single")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{
		Op: map[string]any{
			"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
			"target_frame": float64(10), "edge": "end",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != "failed" || result.Data["op_kind"] != "trim_clip_edge" ||
		result.Data["invalid_field"] != "timeline_frame" {
		t.Fatalf("result=%#v", result)
	}
	schema, ok := result.Data["expected_schema"].(map[string]any)
	if !ok {
		t.Fatalf("expected_schema=%#v", result.Data["expected_schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["timeline_frame"] == nil || properties["target_frame"] != nil {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	example, ok := result.Data["correct_example"].(map[string]any)
	if !ok || example["kind"] != "trim_clip_edge" {
		t.Fatalf("correct_example=%#v", result.Data["correct_example"])
	}
	if err := timeline.ValidateOpFields(example); err != nil {
		t.Fatalf("correct_example must pass field validation: %#v err=%v", example, err)
	}
	if _, exists := result.Data["op_catalog"]; exists {
		t.Fatalf("known kind must not resend full catalog: %#v", result.Data)
	}
	after, err := timeline.Latest(t.Context(), database, "draft_op_jit_single")
	if err != nil || after.Version != before.Version {
		t.Fatalf("failed patch changed timeline: before=%d after=%d err=%v", before.Version, after.Version, err)
	}
}

func TestApplyPatchUnknownKindReturnsOnlyNineteenEntryCatalog(t *testing.T) {
	t.Parallel()
	service, _, ctx := timelineOpRecoveryFixture(t, "draft_op_jit_unknown")
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patch", rushestools.TimelinePatchInput{
		Op: map[string]any{"kind": "remove_clip", "timeline_clip_id": "clip_v1_001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	catalog, ok := result.Data["op_catalog"].([]map[string]string)
	if result.Status != "failed" || result.Data["op_kind"] != "remove_clip" ||
		!ok || len(catalog) != len(timeline.Catalog) || len(catalog) != 19 {
		t.Fatalf("result=%#v", result)
	}
	seen := map[string]struct{}{}
	for _, entry := range catalog {
		if entry["kind"] == "" || entry["summary"] == "" {
			t.Fatalf("catalog entry=%#v", entry)
		}
		seen[entry["kind"]] = struct{}{}
	}
	if len(seen) != 19 {
		t.Fatalf("catalog kinds=%#v", seen)
	}
	if _, exists := result.Data["expected_schema"]; exists {
		t.Fatalf("unknown kind must use catalog rather than a made-up schema: %#v", result.Data)
	}
	if _, exists := result.Data["correct_example"]; exists {
		t.Fatalf("unknown kind must not include an invented example: %#v", result.Data)
	}
}

func TestApplyPatchesFieldFailurePreservesAtomicityAndJITMetadata(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_op_jit_batch")
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{
		Ops: []rushestools.TimelineOp{
			{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -6.0},
			{
				"kind": "trim_clip_edge", "timeline_clip_id": "clip_v1_001",
				"target_frame": 10, "edge": "end",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	if result.Status != "failed" || result.Data["failed_op_index"] != 2 ||
		result.Data["op_kind"] != "trim_clip_edge" || result.Data["expected_schema"] == nil ||
		result.Data["correct_example"] == nil {
		t.Fatalf("result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_op_jit_batch")
	if err != nil || latest.Version != 1 || latest.Tracks[0].Clips[0].GainDB != 0 {
		t.Fatalf("batch must remain atomic: latest=%#v err=%v", latest, err)
	}
}

func TestApplyPatchesSemanticFailureUsesDocumentBeforeFailedOperation(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_op_jit_semantic_batch")
	document, err := timeline.ComposeInitial("draft_op_jit_semantic_batch", 2, []timeline.Selection{
		{AssetID: "talk-a", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll"},
		{AssetID: "talk-b", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.persistTimeline(t.Context(), "draft_op_jit_semantic_batch", document, "jit_semantic_fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{
		Ops: []rushestools.TimelineOp{
			{"kind": "delete_clip", "timeline_clip_id": "clip_v2_001"},
			{"kind": "delete_clip", "timeline_clip_id": "missing"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	available, ok := result.Data["available_timeline_clip_ids"].(map[string][]string)
	if !ok {
		t.Fatalf("result missing available timeline facts: %#v", result)
	}
	if result.Status != "failed" || result.Data["failed_op_index"] != 2 ||
		len(available["visual_base"]) != 1 || available["visual_base"][0] != "clip_v2_002" {
		t.Fatalf("result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_op_jit_semantic_batch")
	if err != nil || latest.Version != 2 || len(latest.Tracks[0].Clips) != 2 {
		t.Fatalf("batch must remain atomic: latest=%#v err=%v", latest, err)
	}
}

func TestApplyPatchesReorderFailureUsesFailedPointFactsAndStaysAtomic(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_reorder_semantic_batch")
	document, err := timeline.ComposeInitial("draft_reorder_semantic_batch", 2, []timeline.Selection{
		{AssetID: "talk-a", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll"},
		{AssetID: "talk-b", AssetKind: "video", SourceEndFrame: 60, Role: "a_roll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, persistErr := service.persistTimeline(t.Context(), "draft_reorder_semantic_batch", document, "reorder_semantic_fixture"); persistErr != nil || persisted.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", persisted, persistErr)
	}
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{
		Ops: []rushestools.TimelineOp{
			{"kind": "delete_clip", "timeline_clip_id": "clip_v2_001"},
			{"kind": "reorder_clip", "timeline_clip_id": "missing", "target_frame": 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	available, _ := result.Data["available_timeline_clip_ids"].(map[string][]string)
	if result.Status != "failed" || result.Data["failed_op_index"] != 2 ||
		len(available["visual_base"]) != 1 || available["visual_base"][0] != "clip_v2_002" {
		t.Fatalf("result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_reorder_semantic_batch")
	if err != nil || latest.Version != 2 || len(latest.Tracks[0].Clips) != 2 {
		t.Fatalf("batch must remain atomic: latest=%#v err=%v", latest, err)
	}
}

func TestApplyPatchesSplitFailureUsesFailedPointFactsAndStaysAtomic(t *testing.T) {
	t.Parallel()
	service, database, ctx := timelineOpRecoveryFixture(t, "draft_split_semantic_batch")
	raw, err := service.ExecuteTool(ctx, "timeline.apply_patches", rushestools.TimelinePatchBatchInput{
		Ops: []rushestools.TimelineOp{
			{"kind": "adjust_gain", "timeline_clip_id": "clip_v1_001", "gain_db": -6.0},
			{"kind": "split_clip", "timeline_clip_id": "clip_v1_001", "split_frame": 60},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := raw.(rushestools.ToolResult)
	actual, _ := result.Data["actual_clip_range"].(map[string]any)
	if result.Status != "failed" || result.Data["failed_op_index"] != 2 ||
		result.Data["semantic_error_kind"] != timeline.SemanticFrameRange ||
		actual["provided_frame"] != 60 {
		t.Fatalf("result=%#v", result)
	}
	latest, err := timeline.Latest(t.Context(), database, "draft_split_semantic_batch")
	if err != nil || latest.Version != 1 || latest.Tracks[0].Clips[0].GainDB != 0 {
		t.Fatalf("batch must remain atomic: latest=%#v err=%v", latest, err)
	}
}

func TestTimelineOpExpectedSchemasFollowCatalogAndHideInjectedFields(t *testing.T) {
	t.Parallel()
	for _, spec := range timeline.Catalog {
		schema := timelineOpExpectedSchema(spec)
		properties := schema["properties"].(map[string]any)
		kind := properties["kind"].(map[string]any)
		if kind["const"] != spec.Kind || schema["additionalProperties"] != false {
			t.Fatalf("kind=%s schema=%#v", spec.Kind, schema)
		}
		for _, field := range spec.Fields {
			_, visible := properties[field.Name]
			if field.Injected == visible {
				t.Fatalf("kind=%s field=%s injected=%v visible=%v", spec.Kind, field.Name, field.Injected, visible)
			}
			for _, alias := range field.Aliases {
				if properties[alias] == nil {
					t.Fatalf("kind=%s missing alias=%s", spec.Kind, alias)
				}
			}
		}
		if len(spec.RequireAny) > 0 && schema["allOf"] == nil {
			t.Fatalf("kind=%s missing RequireAny schema", spec.Kind)
		}
	}

	insertSpec, ok := timeline.LookupOpSpec("insert_clip")
	if !ok {
		t.Fatal("insert_clip missing")
	}
	first := timelineOpExpectedSchema(*insertSpec)
	metadata := first["properties"].(map[string]any)["metadata"].(map[string]any)
	metadata["examples"].([]any)[0].(map[string]any)["source"] = "mutated"
	second := timelineOpExpectedSchema(*insertSpec)
	secondMetadata := second["properties"].(map[string]any)["metadata"].(map[string]any)
	if secondMetadata["examples"].([]any)[0].(map[string]any)["source"] != "catalog_example" {
		t.Fatal("expected_schema 暴露了 Catalog 的可变示例")
	}

	wrapped := fmt.Errorf("wrapped: %w", &timeline.OpFieldError{Kind: "delete_clip", Field: "timeline_clip_id"})
	if fieldErr, ok := timelineOpFieldError(wrapped); !ok || fieldErr.Kind != "delete_clip" {
		t.Fatalf("wrapped field error not preserved: %#v ok=%v", fieldErr, ok)
	}
	if _, ok := timelineOpFailureAt(errors.New("semantic failure"), nil, 0, timeline.Document{}); ok {
		t.Fatal("非字段错误不应被改写成 JIT field failure")
	}
}

func timelineOpRecoveryFixture(
	t *testing.T,
	draftID string,
) (*Service, *storage.DB, context.Context) {
	t.Helper()
	database := agentTestDatabase(t)
	createAgentDraft(t, database, draftID)
	service, err := NewService(t.Context(), database, nil)
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
	result, err := service.persistTimeline(t.Context(), draftID, document, "op_jit_fixture")
	if err != nil || result.Status != "succeeded" {
		t.Fatalf("persist=%#v err=%v", result, err)
	}
	ctx := rushestools.WithDraftID(t.Context(), draftID)
	return service, database, ctx
}
