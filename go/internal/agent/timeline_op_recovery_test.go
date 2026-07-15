package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

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

func TestApplyPatchUnknownKindReturnsOnlyEighteenEntryCatalog(t *testing.T) {
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
		!ok || len(catalog) != len(timeline.Catalog) || len(catalog) != 18 {
		t.Fatalf("result=%#v", result)
	}
	seen := map[string]struct{}{}
	for _, entry := range catalog {
		if entry["kind"] == "" || entry["summary"] == "" {
			t.Fatalf("catalog entry=%#v", entry)
		}
		seen[entry["kind"]] = struct{}{}
	}
	if len(seen) != 18 {
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
		Ops: []map[string]any{
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
	if _, ok := timelineOpFailureAt(errors.New("semantic failure"), nil, 0); ok {
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
