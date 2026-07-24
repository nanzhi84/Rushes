package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

func TestTimelineApplyPatchesInfoCompilesCatalogToOneOf(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}

	var applyPatches Spec
	for _, spec := range registry.Specs(true) {
		if spec.Name == "timeline.apply_patches" {
			applyPatches = spec
			break
		}
	}
	if applyPatches.Implementation == nil {
		t.Fatal("timeline.apply_patches 未注册")
	}
	info, err := applyPatches.Implementation.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := info.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	opsSchema, exists := parameters.Properties.Get("ops")
	if !exists || opsSchema.Type != "array" || opsSchema.Items == nil {
		t.Fatalf("timeline.apply_patches ops schema=%#v", opsSchema)
	}
	assertTimelineOpCatalogSchema(t, opsSchema.Items)
}

// 这个快照测试有意锁定 Eino v0.9.12 与 jsonschema v1.0.3 的自定义类型反射链。
// 升级任一依赖时，必须先确认 TimelineOp{} 仍被反射为主动 oneOf，而不是普通 map。
func TestTimelineOpReflectionSnapshotPinsOneOf(t *testing.T) {
	t.Parallel()
	reflector := jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	reflected := reflector.Reflect(TimelineOp{})
	assertTimelineOpCatalogSchema(t, reflected)
	assertGoModDependencyVersion(t, "github.com/cloudwego/eino", "v0.9.12")
	assertGoModDependencyVersion(t, "github.com/eino-contrib/jsonschema", "v1.0.3")
}

func TestAtomicTimelineSchemasPartitionCatalogWithoutBatchOrInjectedFields(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name   string
		schema *jsonschema.Schema
		kinds  []string
	}{
		{"timeline.insert", (TimelineInsertInput{}).JSONSchema(), timelineAtomicKinds["timeline.insert"]},
		{"timeline.delete", (TimelineDeleteInput{}).JSONSchema(), timelineAtomicKinds["timeline.delete"]},
		{"timeline.update", (TimelineUpdateInput{}).JSONSchema(), timelineAtomicKinds["timeline.update"]},
		{"timeline.split", (TimelineSplitInput{}).JSONSchema(), timelineAtomicKinds["timeline.split"]},
	}
	seen := map[string]string{}
	for _, fixture := range fixtures {
		if fixture.schema.Type != "object" || len(fixture.schema.OneOf) != len(fixture.kinds) {
			t.Fatalf("%s schema=%#v", fixture.name, fixture.schema)
		}
		for index, kind := range fixture.kinds {
			branch := fixture.schema.OneOf[index]
			kindSchema, exists := branch.Properties.Get("kind")
			if !exists || kindSchema.Const != kind {
				t.Fatalf("%s branch[%d] kind=%#v want=%s", fixture.name, index, kindSchema.Const, kind)
			}
			if owner := seen[kind]; owner != "" {
				t.Fatalf("Catalog op %s 同时属于 %s 与 %s", kind, owner, fixture.name)
			}
			seen[kind] = fixture.name
			for _, hidden := range []string{
				"ops", "asset_kind", "include_original_audio", "audio_asset_ids",
			} {
				if _, exposed := branch.Properties.Get(hidden); exposed {
					t.Errorf("%s.%s 暴露字段 %s", fixture.name, kind, hidden)
				}
			}
		}
	}
	updateSchema := (TimelineUpdateInput{}).JSONSchema()
	if updateSchema.Not == nil ||
		!containsString(updateSchema.Not.Required, "timeline_clip_id") ||
		!containsString(updateSchema.Not.Required, "track_id") {
		t.Fatalf("timeline.update 未在 schema 层拒绝双 target: %#v", updateSchema.Not)
	}
	if _, exposed := seen["sync_original_audio"]; exposed {
		t.Fatal("sync_original_audio 不得进入模型可见原子 schema")
	}
	if len(seen) != len(timeline.Catalog)-1 {
		t.Fatalf("原子 schema 覆盖 %d 个 op，期望排除 sync_original_audio 后为 %d", len(seen), len(timeline.Catalog)-1)
	}
	insertSchema := (TimelineInsertInput{}).JSONSchema()
	for _, generated := range []string{"timeline_clip_id", "parent_block_id"} {
		if _, exposed := insertSchema.Properties.Get(generated); exposed {
			t.Errorf("timeline.insert 暴露服务端生成字段 %s", generated)
		}
	}
	splitSchema := (TimelineSplitInput{}).JSONSchema()
	if _, exposed := splitSchema.Properties.Get("new_timeline_clip_id"); exposed {
		t.Error("timeline.split 暴露服务端生成字段 new_timeline_clip_id")
	}
	insertClip := timelineOpBranchByKind(t, insertSchema, "insert_clip")
	trackID, exists := insertClip.Properties.Get("track_id")
	if !exists || !reflect.DeepEqual(
		trackID.Enum,
		[]any{"visual_base", "visual_overlay", "voiceover", "bgm", "sfx"},
	) {
		t.Fatalf("timeline.insert track_id enum=%#v", trackID)
	}
}

func TestTimelineAtomicOperationRejectsWrongFamilyAndInjectedFields(t *testing.T) {
	t.Parallel()
	if _, err := TimelineAtomicOperation("timeline.delete", TimelineDeleteInput{
		"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
	}); err == nil || !strings.Contains(err.Error(), "不接受") {
		t.Fatalf("wrong family err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.insert", TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
		"asset_kind": "video",
	}); err == nil || !strings.Contains(err.Error(), "未声明字段 asset_kind") {
		t.Fatalf("injected field err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.delete", TimelineDeleteInput{
		"kind": "delete_clip", "clip_id": "legacy_clip_id",
	}); err == nil || !strings.Contains(err.Error(), "不接受字段 clip_id") {
		t.Fatalf("legacy alias err=%v", err)
	}
	for field, input := range map[string]TimelineInsertInput{
		"timeline_clip_id": {
			"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
			"timeline_clip_id": "model_chosen",
		},
		"parent_block_id": {
			"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
			"parent_block_id": "model_chosen",
		},
	} {
		if _, err := TimelineAtomicOperation("timeline.insert", input); err == nil ||
			!strings.Contains(err.Error(), "不接受字段 "+field) {
			t.Errorf("generated field %s err=%v", field, err)
		}
	}
	if _, err := TimelineAtomicOperation("timeline.split", TimelineSplitInput{
		"kind": "split_clip", "timeline_clip_id": "clip_1", "split_frame": 15,
		"new_timeline_clip_id": "model_chosen",
	}); err == nil || !strings.Contains(err.Error(), "不接受字段 new_timeline_clip_id") {
		t.Errorf("split generated ID err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.insert", TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
		"track_id": "original_audio",
	}); err == nil || !strings.Contains(err.Error(), "不允许写入轨道 original_audio") {
		t.Errorf("derived track err=%v", err)
	}
	operation, err := TimelineAtomicOperation("timeline.split", TimelineSplitInput{
		"kind": "split_clip", "timeline_clip_id": "clip_1", "split_frame": 15,
	})
	if err != nil || operation["kind"] != "split_clip" || len(operation) != 3 {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
}

func TestAtomicTimelineToolRejectsInvalidCatalogCombinationBeforeExecutor(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	insert := registry.specs["timeline.insert"].Implementation.(einotool.InvokableTool)
	ctx := WithDraftID(t.Context(), "draft_atomic_decode")
	for _, arguments := range []string{
		`{"kind":"delete_clip","timeline_clip_id":"clip_1"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"asset_kind":"video"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"timeline_clip_id":"a","clip_id":"b"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"timeline_clip_id":"model_chosen"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"track_id":"original_audio"}`,
	} {
		if _, err := insert.InvokableRun(ctx, arguments); err == nil {
			t.Errorf("非法原子参数进入 executor 并返回成功: %s", arguments)
		}
	}
}

func TestTimelineOpSchemaCallsDoNotShareMutableExamples(t *testing.T) {
	t.Parallel()
	first := (TimelineOp{}).JSONSchema()
	insert := timelineOpBranchByKind(t, first, "insert_clip")
	metadata, exists := insert.Properties.Get("metadata")
	if !exists || len(metadata.Examples) != 1 {
		t.Fatalf("insert_clip.metadata examples=%#v", metadata)
	}
	firstExample, ok := metadata.Examples[0].(map[string]any)
	if !ok {
		t.Fatalf("metadata example type=%T", metadata.Examples[0])
	}
	firstExample["source"] = "mutated"

	second := (TimelineOp{}).JSONSchema()
	metadata, exists = timelineOpBranchByKind(t, second, "insert_clip").Properties.Get("metadata")
	if !exists || metadata.Examples[0].(map[string]any)["source"] != "catalog_example" {
		t.Fatalf("不同 schema 调用共享了可变示例: %#v", metadata)
	}
}

func TestTimelinePatchBatchInputJSONKeepsNamedMapRuntimeSemantics(t *testing.T) {
	t.Parallel()
	var decoded TimelinePatchBatchInput
	if err := json.Unmarshal([]byte(`{"ops":[{"kind":"delete_clip","clip_id":"clip_1"}]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Ops) != 1 || decoded.Ops[0]["kind"] != "delete_clip" || decoded.Ops[0]["clip_id"] != "clip_1" {
		t.Fatalf("decoded ops=%#v", decoded.Ops)
	}
	if err := timeline.ValidateOpFields(map[string]any(decoded.Ops[0])); err != nil {
		t.Fatalf("命名 map 未保留运行时语义: %v", err)
	}
}

func assertTimelineOpCatalogSchema(t *testing.T, schema *jsonschema.Schema) {
	t.Helper()
	if len(timeline.Catalog) != 19 {
		t.Fatalf("timeline.Catalog kinds=%d want=19", len(timeline.Catalog))
	}
	if schema == nil || schema.Type != "object" {
		t.Fatalf("TimelineOp root=%#v want object", schema)
	}
	if len(schema.OneOf) != len(timeline.Catalog) {
		t.Fatalf("op.oneOf branches=%d want=%d", len(schema.OneOf), len(timeline.Catalog))
	}

	seenKinds := make(map[string]bool, len(timeline.Catalog))
	for index, spec := range timeline.Catalog {
		branch := schema.OneOf[index]
		if branch == nil || branch.Type != "object" || branch.Properties == nil {
			t.Fatalf("branch[%d]=%#v want object properties", index, branch)
		}
		kindSchema, exists := branch.Properties.Get("kind")
		if !exists {
			t.Fatalf("branch[%d] 缺少 kind", index)
		}
		kind, ok := kindSchema.Const.(string)
		if !ok || kind == "" {
			t.Fatalf("branch[%d] kind.const=%#v", index, kindSchema.Const)
		}
		if kind != spec.Kind {
			t.Fatalf("branch[%d] kind=%q want=%q", index, kind, spec.Kind)
		}
		if seenKinds[kind] {
			t.Fatalf("kind.const 重复: %s", kind)
		}
		seenKinds[kind] = true
		if !containsString(branch.Required, "kind") {
			t.Errorf("%s 未要求 kind", kind)
		}
		assertFalseSchema(t, kind, branch.AdditionalProperties)

		expectedPropertyCount := 1
		for _, field := range spec.Fields {
			if field.Injected {
				if _, exposed := branch.Properties.Get(field.Name); exposed {
					t.Errorf("%s 暴露服务端注入字段 %s", kind, field.Name)
				}
				continue
			}
			expectedPropertyCount += 1 + len(field.Aliases)
			property, exists := branch.Properties.Get(field.Name)
			if !exists {
				t.Errorf("%s 缺少字段 %s", kind, field.Name)
				continue
			}
			if property.Type != expectedTimelineOpJSONType(field.Type) {
				t.Errorf("%s.%s type=%q want=%q", kind, field.Name, property.Type, expectedTimelineOpJSONType(field.Type))
			}
			for _, alias := range field.Aliases {
				if _, exists := branch.Properties.Get(alias); !exists {
					t.Errorf("%s.%s 缺少兼容别名 %s", kind, field.Name, alias)
				}
			}
			if !field.Required {
				continue
			}
			if len(field.Aliases) == 0 {
				if !containsString(branch.Required, field.Name) {
					t.Errorf("%s 未要求字段 %s", kind, field.Name)
				}
				continue
			}
			alternatives := append([]string{field.Name}, field.Aliases...)
			if !hasRequiredAlternatives(branch.AllOf, alternatives) {
				t.Errorf("%s 缺少 %v 的 required anyOf", kind, alternatives)
			}
		}
		if branch.Properties.Len() != expectedPropertyCount {
			t.Errorf("%s properties=%d want=%d", kind, branch.Properties.Len(), expectedPropertyCount)
		}
		if len(spec.RequireAny) > 0 && !hasRequiredAlternatives(branch.AllOf, spec.RequireAny) {
			t.Errorf("%s 缺少 RequireAny %v", kind, spec.RequireAny)
		}
	}

	trimEdge := timelineOpBranchByKind(t, schema, "trim_clip_edge")
	if _, exists := trimEdge.Properties.Get("timeline_frame"); !exists {
		t.Error("trim_clip_edge 缺少 timeline_frame")
	}
	if _, exists := trimEdge.Properties.Get("target_frame"); exists {
		t.Error("trim_clip_edge 错误暴露 target_frame")
	}
	for _, injected := range []string{"asset_kind", "include_original_audio", "audio_asset_ids"} {
		for _, branch := range schema.OneOf {
			if _, exists := branch.Properties.Get(injected); exists {
				t.Errorf("op.oneOf 暴露服务端注入字段 %s", injected)
			}
		}
	}
}

func timelineOpBranchByKind(t *testing.T, schema *jsonschema.Schema, kind string) *jsonschema.Schema {
	t.Helper()
	for _, branch := range schema.OneOf {
		if branch == nil || branch.Properties == nil {
			continue
		}
		kindSchema, exists := branch.Properties.Get("kind")
		if exists && kindSchema.Const == kind {
			return branch
		}
	}
	t.Fatalf("oneOf 缺少 kind=%s", kind)
	return nil
}

func hasRequiredAlternatives(constraints []*jsonschema.Schema, names []string) bool {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = true
	}
	for _, constraint := range constraints {
		if constraint == nil || len(constraint.AnyOf) != len(want) {
			continue
		}
		seen := make(map[string]bool, len(want))
		valid := true
		for _, alternative := range constraint.AnyOf {
			if alternative == nil || len(alternative.Required) != 1 || !want[alternative.Required[0]] {
				valid = false
				break
			}
			seen[alternative.Required[0]] = true
		}
		if valid && len(seen) == len(want) {
			return true
		}
	}
	return false
}

func assertFalseSchema(t *testing.T, kind string, schema *jsonschema.Schema) {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "false" {
		t.Errorf("%s additionalProperties=%s want=false", kind, encoded)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func expectedTimelineOpJSONType(fieldType timeline.OpFieldType) string {
	switch fieldType {
	case timeline.OpFieldString:
		return "string"
	case timeline.OpFieldInteger:
		return "integer"
	case timeline.OpFieldNumber:
		return "number"
	case timeline.OpFieldBoolean:
		return "boolean"
	case timeline.OpFieldObject:
		return "object"
	case timeline.OpFieldStringArray:
		return "array"
	default:
		return ""
	}
}

func assertGoModDependencyVersion(t *testing.T, path, want string) {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 timeline_op_schema_test.go")
	}
	contents, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	needle := "\t" + path + " " + want + "\n"
	if !strings.Contains(string(contents), needle) {
		t.Fatalf("go.mod 未直接锁定 %s %s", path, want)
	}
}
