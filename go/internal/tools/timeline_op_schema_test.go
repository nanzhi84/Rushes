package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

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
		rootKind, exists := fixture.schema.Properties.Get("kind")
		if !exists || strings.TrimSpace(rootKind.Description) == "" {
			t.Fatalf("%s root kind 缺少模型可见语义", fixture.name)
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
			spec, _ := timeline.LookupOpSpec(kind)
			if kindSchema.Description != spec.Summary {
				t.Errorf(
					"%s.%s summary=%q want=%q",
					fixture.name, kind, kindSchema.Description, spec.Summary,
				)
			}
			allowedNames := modelFacingTimelineOpFieldNames(*spec)
			for _, field := range spec.Fields {
				if field.Injected || field.Generated {
					continue
				}
				property, exposed := fixture.schema.Properties.Get(field.Name)
				if !exposed || !strings.Contains(property.Description, field.Desc) {
					t.Errorf(
						"%s.%s field %s description=%q missing=%q",
						fixture.name, kind, field.Name, property.Description, field.Desc,
					)
				}
			}
			if branch.MaxProperties == nil ||
				*branch.MaxProperties != uint64(len(allowedNames)) {
				t.Errorf(
					"%s.%s maxProperties=%v want=%d",
					fixture.name, kind, branch.MaxProperties, len(allowedNames),
				)
			}
			for _, hidden := range []string{
				"ops", "asset_kind", "include_original_audio", "audio_asset_ids",
			} {
				if _, exposed := fixture.schema.Properties.Get(hidden); exposed {
					t.Errorf("%s.%s 暴露字段 %s", fixture.name, kind, hidden)
				}
			}
			if len(branch.Required) < len(allowedNames) {
				assertPropertyNameWhitelist(t, fixture.name, kind, branch, allowedNames)
			}
		}
	}
	updateSchema := (TimelineUpdateInput{}).JSONSchema()
	if updateSchema.Not == nil ||
		!containsString(updateSchema.Not.Required, "timeline_clip_id") ||
		!containsString(updateSchema.Not.Required, "track_id") {
		t.Fatalf("timeline.update 未在 schema 层拒绝双 target: %#v", updateSchema.Not)
	}
	if len(seen) != len(timeline.Catalog) {
		t.Fatalf("原子 schema 覆盖 %d 个 op，期望完整覆盖 %d", len(seen), len(timeline.Catalog))
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
	adjustGain := timelineOpBranchByKind(t, updateSchema, "adjust_gain")
	if adjustGain.MaxProperties == nil || *adjustGain.MaxProperties != 3 ||
		len(adjustGain.Required) != 3 {
		t.Fatalf("adjust_gain 未用 required+maxProperties 拒绝跨 kind 字段: %#v", adjustGain)
	}
	setFades := timelineOpBranchByKind(t, updateSchema, "set_clip_fades")
	if setFades.MaxProperties == nil || *setFades.MaxProperties != 4 ||
		len(setFades.Required) != 4 {
		t.Fatalf("set_clip_fades 未用 required+maxProperties 拒绝跨 kind 字段: %#v", setFades)
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

func TestTimelineOpRecoverySchemaMatchesAtomicToolSurface(t *testing.T) {
	t.Parallel()

	deleteSpec, exists := timeline.LookupOpSpec("delete_clip")
	if !exists {
		t.Fatal("delete_clip spec missing")
	}
	deleteSchema := TimelineOpExpectedSchema("timeline.delete", *deleteSpec)
	deleteProperties, ok := deleteSchema["properties"].(map[string]any)
	if !ok || deleteProperties["timeline_clip_id"] == nil {
		t.Fatalf("delete recovery properties=%#v", deleteSchema["properties"])
	}
	if deleteProperties["clip_id"] != nil {
		t.Fatalf("delete recovery 暴露兼容别名 clip_id: %#v", deleteProperties)
	}
	if required, ok := deleteSchema["required"].([]string); !ok ||
		!containsString(required, "timeline_clip_id") {
		t.Fatalf("delete recovery required=%#v", deleteSchema["required"])
	}
	if TimelineOpExpectedSchema("timeline.update", *deleteSpec) != nil {
		t.Fatal("跨工具家族不得生成 recovery schema")
	}

	insertSpec, exists := timeline.LookupOpSpec("insert_clip")
	if !exists {
		t.Fatal("insert_clip spec missing")
	}
	insertSchema := TimelineOpExpectedSchema("timeline.insert", *insertSpec)
	insertProperties, ok := insertSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("insert recovery properties=%#v", insertSchema["properties"])
	}
	for _, hidden := range []string{
		"asset_kind", "include_original_audio", "timeline_clip_id", "parent_block_id", "clip_id",
	} {
		if insertProperties[hidden] != nil {
			t.Errorf("insert recovery 暴露服务端或兼容字段 %s", hidden)
		}
	}

	catalog := TimelineAtomicCatalog("timeline.delete")
	if len(catalog) != len(timelineAtomicKinds["timeline.delete"]) {
		t.Fatalf("delete catalog=%v", catalog)
	}
	for _, spec := range catalog {
		owner, exposed := TimelineAtomicToolForKind(spec.Kind)
		if !exposed || owner != "timeline.delete" {
			t.Errorf("delete catalog 暴露跨家族 kind=%s owner=%s", spec.Kind, owner)
		}
	}
	if catalog := TimelineAtomicCatalog("timeline.unknown"); len(catalog) != 0 {
		t.Fatalf("unknown tool catalog=%v", catalog)
	}
}

func TestAtomicForwardAndRecoverySchemasStayInCatalogParity(t *testing.T) {
	t.Parallel()
	forwardSchemas := map[string]*jsonschema.Schema{
		"timeline.insert": (TimelineInsertInput{}).JSONSchema(),
		"timeline.delete": (TimelineDeleteInput{}).JSONSchema(),
		"timeline.update": (TimelineUpdateInput{}).JSONSchema(),
		"timeline.split":  (TimelineSplitInput{}).JSONSchema(),
	}
	for toolName, kinds := range timelineAtomicKinds {
		forwardSchema := forwardSchemas[toolName]
		forwardMap := marshalSchemaMap(t, forwardSchema)
		forwardProperties, _ := forwardMap["properties"].(map[string]any)
		expectedRootProperties := map[string]struct{}{"kind": {}}
		for _, kind := range kinds {
			spec, exists := timeline.LookupOpSpec(kind)
			if !exists {
				t.Fatalf("%s Catalog spec missing", kind)
			}
			expectedProperties := map[string]struct{}{"kind": {}}
			for _, field := range spec.Fields {
				if field.Injected || field.Generated {
					continue
				}
				expectedProperties[field.Name] = struct{}{}
				expectedRootProperties[field.Name] = struct{}{}
			}

			recovery := marshalSchemaMap(t, TimelineOpExpectedSchema(toolName, *spec))
			recoveryProperties, _ := recovery["properties"].(map[string]any)
			if got, want := mapKeys(recoveryProperties), setKeys(expectedProperties); !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s recovery properties=%v want=%v", toolName, kind, got, want)
			}
			branch := marshalSchemaMap(t, timelineOpBranchByKind(t, forwardSchema, kind))
			if branch["maxProperties"] != float64(len(expectedProperties)) {
				t.Errorf(
					"%s.%s maxProperties=%v want=%d",
					toolName, kind, branch["maxProperties"], len(expectedProperties),
				)
			}
			for fieldName := range expectedProperties {
				forwardField, forwardExists := forwardProperties[fieldName].(map[string]any)
				recoveryField, recoveryExists := recoveryProperties[fieldName].(map[string]any)
				if !forwardExists || !recoveryExists {
					t.Errorf(
						"%s.%s field=%s forward=%v recovery=%v",
						toolName, kind, fieldName, forwardExists, recoveryExists,
					)
					continue
				}
				if forwardField["type"] != recoveryField["type"] {
					t.Errorf(
						"%s.%s field=%s type forward=%v recovery=%v",
						toolName, kind, fieldName, forwardField["type"], recoveryField["type"],
					)
				}
			}

			if got, want := schemaRequiredNames(branch), sortedStrings(recovery["required"]); !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s required forward=%v recovery=%v", toolName, kind, got, want)
			}
			if got, want := schemaAnyOfRequiredGroups(branch), schemaAnyOfRequiredGroups(recovery); !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s require-any forward=%v recovery=%v", toolName, kind, got, want)
			}
		}
		if got, want := mapKeys(forwardProperties), setKeys(expectedRootProperties); !reflect.DeepEqual(got, want) {
			t.Errorf("%s root properties=%v want Catalog union=%v", toolName, got, want)
		}
	}
}

func TestTimelineAtomicFieldDescriptionGroupsKindsWithoutLosingOwnership(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name   string
		inputs []timelineAtomicFieldSemantic
		want   string
	}{
		{
			name: "相同语义保持紧凑",
			inputs: []timelineAtomicFieldSemantic{
				{kinds: []string{"kind_a"}, description: "同一语义"},
				{kinds: []string{"kind_b"}, description: "同一语义"},
			},
			want: "同一语义",
		},
		{
			name: "不同语义标注 kind",
			inputs: []timelineAtomicFieldSemantic{
				{kinds: []string{"kind_a"}, description: "语义甲"},
				{kinds: []string{"kind_b"}, description: "语义乙"},
			},
			want: "按 kind 解释：kind_a：语义甲；kind_b：语义乙",
		},
		{
			name: "重复语义后出现分歧仍保留全部 kind",
			inputs: []timelineAtomicFieldSemantic{
				{kinds: []string{"kind_a"}, description: "语义甲"},
				{kinds: []string{"kind_b"}, description: "语义甲"},
				{kinds: []string{"kind_c"}, description: "语义乙"},
			},
			want: "按 kind 解释：kind_a/kind_b：语义甲；kind_c：语义乙",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var semantics []timelineAtomicFieldSemantic
			var got string
			for _, input := range testCase.inputs {
				semantics, got = mergeTimelineAtomicFieldSemantic(
					semantics, input.kinds[0], input.description,
				)
			}
			if got != testCase.want {
				t.Fatalf("description=%q want=%q", got, testCase.want)
			}
		})
	}
}

type countingAtomicExecutor struct {
	calls int
}

func (executor *countingAtomicExecutor) ExecuteTool(
	context.Context,
	string,
	any,
) (any, error) {
	executor.calls++
	return ToolResult{Status: string(StatusSucceeded)}, nil
}

func TestAtomicRegistryPreflightRejectsCrossKindFieldsBeforeExecutor(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor := &countingAtomicExecutor{}
	registry, err := NewRegistry(database, executor)
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{
		"kind": "adjust_gain", "timeline_clip_id": "clip_1",
		"gain_db": -3, "fade_in_frames": 7,
	}
	if _, err := registry.DecodeInput("timeline.update", arguments); err == nil ||
		!strings.Contains(err.Error(), "fade_in_frames 不是该操作支持的字段") {
		t.Fatalf("DecodeInput cross-kind field err=%v", err)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	update := registry.specs["timeline.update"].Implementation.(einotool.InvokableTool)
	output, err := update.InvokableRun(
		WithDraftID(t.Context(), "draft_atomic_preflight"),
		string(encoded),
	)
	if err != nil {
		t.Fatalf("structured preflight failed as transport error: %v", err)
	}
	var failure ToolResult
	if err := json.Unmarshal([]byte(output), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Status != string(StatusFailed) ||
		failure.Data["invalid_field"] != "fade_in_frames" ||
		failure.Data["expected_schema"] == nil ||
		failure.Data["current_timeline_unchanged"] != true {
		t.Fatalf("preflight failure=%#v", failure)
	}
	if executor.calls != 0 {
		t.Fatalf("invalid cross-kind input reached executor %d times", executor.calls)
	}
	if _, err := strictUnmarshalToolArguments[TimelineUpdateInput](
		t.Context(),
		"timeline.update",
		`{"kind":"adjust_gain","timeline_clip_id":"clip_1","gain_db":-3}`,
	); err != nil {
		t.Fatalf("valid adjust_gain rejected: %v", err)
	}
}

func TestTimelineAtomicToolForKindCoversCatalogPartition(t *testing.T) {
	for toolName, kinds := range timelineAtomicKinds {
		for _, kind := range kinds {
			got, ok := TimelineAtomicToolForKind(kind)
			if !ok || got != toolName {
				t.Fatalf("kind=%s tool=%s ok=%v want=%s", kind, got, ok, toolName)
			}
		}
	}
	if toolName, ok := TimelineAtomicToolForKind("batch"); ok || toolName != "" {
		t.Fatalf("batch 不得映射到原子工具: tool=%q ok=%v", toolName, ok)
	}
}

func TestTimelineAtomicOperationRejectsWrongFamilyAndInjectedFields(t *testing.T) {
	t.Parallel()
	if _, err := TimelineAtomicOperation("timeline.delete", TimelineDeleteInput{
		"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
	}); err == nil || !strings.Contains(err.Error(), "属于 timeline.insert") {
		t.Fatalf("wrong family err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.insert", TimelineInsertInput{
		"kind": "insert_clip", "asset_id": "asset", "source_start_frame": 0, "source_end_frame": 30,
		"asset_kind": "video",
	}); err == nil || !strings.Contains(err.Error(), "不接受字段 asset_kind") {
		t.Fatalf("injected field err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.delete", TimelineDeleteInput{
		"kind": "delete_clip", "timeline_clip_id": "clip_1", "undeclared": true,
	}); err == nil || !strings.Contains(err.Error(), "undeclared 不是该操作支持的字段") {
		t.Fatalf("undeclared field err=%v", err)
	}
	if _, err := TimelineAtomicOperation("timeline.delete", TimelineDeleteInput{
		"kind": "delete_clip", "clip_id": "legacy_clip_id",
	}); err == nil || !strings.Contains(err.Error(), "timeline_clip_id 缺少必填字段") {
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
	if _, err := TimelineAtomicOperation("timeline.insert", TimelineUpdateInput{
		"kind": "adjust_gain", "timeline_clip_id": "clip_1", "gain_db": -3,
	}); err == nil || !strings.Contains(err.Error(), "输入类型不匹配") {
		t.Fatalf("mismatched input type err=%v", err)
	}
}

func TestTimelineOpSchemaExamplesAreDeepCloned(t *testing.T) {
	t.Parallel()
	originalStrings := []string{"voiceover", "original_audio"}
	original := map[string]any{
		"nested":  map[string]any{"value": "kept"},
		"choices": []any{map[string]any{"name": "first"}},
		"tracks":  originalStrings,
	}
	cloned := cloneTimelineOpSchemaExample(original).(map[string]any)
	cloned["nested"].(map[string]any)["value"] = "changed"
	cloned["choices"].([]any)[0].(map[string]any)["name"] = "changed"
	cloned["tracks"].([]string)[0] = "changed"
	if original["nested"].(map[string]any)["value"] != "kept" ||
		original["choices"].([]any)[0].(map[string]any)["name"] != "first" ||
		originalStrings[0] != "voiceover" {
		t.Fatalf("schema example clone mutated source: original=%#v cloned=%#v", original, cloned)
	}
	if got := timelineOpJSONType(timeline.OpFieldType("future_type")); got != "string" {
		t.Fatalf("unknown Catalog field type fallback=%q", got)
	}
}

func TestAtomicTimelineToolRejectsInvalidCatalogCombinationBeforeExecutor(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	executor := &countingAtomicExecutor{}
	registry, err := NewRegistry(database, executor)
	if err != nil {
		t.Fatal(err)
	}
	insert := registry.specs["timeline.insert"].Implementation.(einotool.InvokableTool)
	ctx := WithDraftID(t.Context(), "draft_atomic_decode")
	for _, arguments := range []string{
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"asset_kind":"video"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"timeline_clip_id":"model_chosen"}`,
		`{"kind":"insert_clip","asset_id":"asset","source_start_frame":0,"source_end_frame":30,"track_id":"original_audio"}`,
	} {
		output, runErr := insert.InvokableRun(ctx, arguments)
		if runErr != nil {
			t.Errorf("invalid atomic input should return structured preflight failure: %v", runErr)
			continue
		}
		var failure ToolResult
		if err := json.Unmarshal([]byte(output), &failure); err != nil {
			t.Fatal(err)
		}
		if failure.Status != string(StatusFailed) ||
			failure.Data["current_timeline_unchanged"] != true {
			t.Errorf("invalid atomic input failure=%#v arguments=%s", failure, arguments)
		}
	}
	if executor.calls != 0 {
		t.Fatalf("invalid atomic inputs reached executor %d times", executor.calls)
	}
}

func TestStrictUnmarshalNormalizesOnlyCanonicalAtomicFrameStrings(t *testing.T) {
	t.Parallel()

	decoded, err := strictUnmarshalToolArguments[TimelineInsertInput](
		t.Context(),
		"timeline.insert",
		`{"kind":"insert_clip","asset_id":"asset_bgm","track_id":"bgm","source_start_frame":"0","source_end_frame":"1440","timeline_start_frame":"0"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	input := decoded.(TimelineInsertInput)
	if input["source_start_frame"] != int64(0) || input["source_end_frame"] != int64(1440) ||
		input["timeline_start_frame"] != int64(0) {
		t.Fatalf("canonical frame strings not normalized: %#v", input)
	}
	if _, err := TimelineAtomicOperation("timeline.insert", input); err != nil {
		t.Fatalf("normalized provider arguments should pass atomic preflight: %v", err)
	}

	for _, invalid := range []string{"01", " 1", "1.5", "1e3", "9223372036854775808"} {
		raw := fmt.Sprintf(
			`{"kind":"insert_clip","asset_id":"asset_bgm","track_id":"bgm","source_start_frame":%q,"source_end_frame":%q}`,
			invalid, "1440",
		)
		decoded, decodeErr := strictUnmarshalToolArguments[TimelineInsertInput](
			t.Context(), "timeline.insert", raw,
		)
		if decodeErr != nil {
			t.Fatalf("map transport should remain decodable for %q: %v", invalid, decodeErr)
		}
		if _, preflightErr := TimelineAtomicOperation(
			"timeline.insert", decoded.(TimelineInsertInput),
		); preflightErr == nil || !strings.Contains(preflightErr.Error(), "类型必须是整数帧") {
			t.Errorf("non-canonical %q must remain rejected: %v", invalid, preflightErr)
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

func modelFacingTimelineOpFieldNames(spec timeline.OpSpec) []string {
	names := []string{"kind"}
	for _, field := range spec.Fields {
		if !field.Injected && !field.Generated {
			names = append(names, field.Name)
		}
	}
	return names
}

func assertPropertyNameWhitelist(
	t *testing.T,
	toolName, kind string,
	branch *jsonschema.Schema,
	allowedNames []string,
) {
	t.Helper()
	if branch.PropertyNames == nil || branch.PropertyNames.Pattern == "" {
		t.Fatalf("%s.%s 缺少可选字段白名单", toolName, kind)
	}
	compiled, err := regexp.Compile(branch.PropertyNames.Pattern)
	if err != nil {
		t.Fatalf("%s.%s propertyNames pattern=%q: %v", toolName, kind, branch.PropertyNames.Pattern, err)
	}
	for _, name := range allowedNames {
		if !compiled.MatchString(name) {
			t.Errorf("%s.%s 白名单漏掉 %s", toolName, kind, name)
		}
	}
	if compiled.MatchString("cross_kind_field") {
		t.Errorf("%s.%s 白名单错误接受跨 kind 字段", toolName, kind)
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

func marshalSchemaMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return result
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func setKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(value any) []string {
	values := make([]string, 0)
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				values = append(values, text)
			}
		}
	}
	sort.Strings(values)
	return values
}

func schemaRequiredNames(schema map[string]any) []string {
	return sortedStrings(schema["required"])
}

func schemaAnyOfRequiredGroups(schema map[string]any) [][]string {
	rawAllOf, _ := schema["allOf"].([]any)
	groups := make([][]string, 0, len(rawAllOf))
	for _, rawConstraint := range rawAllOf {
		constraint, _ := rawConstraint.(map[string]any)
		rawAnyOf, _ := constraint["anyOf"].([]any)
		group := make([]string, 0, len(rawAnyOf))
		for _, rawChoice := range rawAnyOf {
			choice, _ := rawChoice.(map[string]any)
			group = append(group, sortedStrings(choice["required"])...)
		}
		sort.Strings(group)
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		left, _ := json.Marshal(groups[i])
		right, _ := json.Marshal(groups[j])
		return string(left) < string(right)
	})
	return groups
}
