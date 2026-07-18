package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type fakeExecutor struct{}

func (fakeExecutor) ExecuteTool(ctx context.Context, name string, _ any) (any, error) {
	draftID, _ := DraftID(ctx)
	switch name {
	case "asset.list_assets":
		return AssetListResult{DraftID: draftID, Assets: []AssetManifest{}, Total: 0}, nil
	case "understand.materials":
		return UnderstandResult{DraftID: draftID, JobID: "job", Status: "queued"}, nil
	case "media.search_shots":
		return ShotSearchResult{Shots: []ShotCandidate{}, TotalMatches: 0}, nil
	case "audio.analyze_beats":
		return AudioBeatAnalysisResult{AssetID: "audio", BPM: 120, BeatFrames: []int{0, 15}}, nil
	case "audio.analyze_speech_pauses":
		return SpeechPauseAnalysisResult{AssetID: "audio", TimelineFPS: 30, Pauses: []SpeechPauseCandidate{}}, nil
	case "speech.inspect":
		return SpeechInspectResult{AssetID: "video", TimelineFPS: 30, Utterances: []SpeechUtteranceEvidence{}}, nil
	case "render.inspect_preview":
		return PreviewInspectionResult{Summary: "ok", Issues: []map[string]interface{}{}}, nil
	default:
		return ToolResult{Status: "succeeded", Observation: name}, nil
	}
}

type prohibitedPathInput struct {
	Path string `json:"path"`
}

type prohibitedFrameInput struct {
	FrameCount int `json:"frame_count"`
}

type prohibitedRevisionInput struct {
	TimelineRevision int `json:"timeline_revision"`
}

type prohibitedNestedPath struct {
	Path string `json:"path"`
}

type prohibitedNestedInput struct {
	Items []prohibitedNestedPath `json:"items"`
}

type prohibitedNestedPointerInput struct {
	Item *prohibitedNestedPath `json:"item"`
}

type prohibitedNestedArrayInput struct {
	Items [1]prohibitedNestedPath `json:"items"`
}

type ignoredProhibitedNestedInput struct {
	Ignored prohibitedNestedPath `json:"-"`
}

type recursiveCleanInput struct {
	Next  *recursiveCleanInput `json:"next,omitempty"`
	Value string               `json:"value"`
}

type recursiveCleanSlice []recursiveCleanSlice

type prohibitedDepth4Input struct {
	Nested prohibitedDepth4Level1 `json:"nested"`
}

type prohibitedDepth4Level1 struct {
	Nested prohibitedDepth4Level2 `json:"nested"`
}

type prohibitedDepth4Level2 struct {
	Nested prohibitedDepth4Level3 `json:"nested"`
}

type prohibitedDepth4Level3 struct {
	Nested prohibitedNestedPath `json:"nested"`
}

type allowedDepth5Input struct {
	Nested allowedDepth5Level1 `json:"nested"`
}

type allowedDepth5Level1 struct {
	Nested allowedDepth5Level2 `json:"nested"`
}

type allowedDepth5Level2 struct {
	Nested allowedDepth5Level3 `json:"nested"`
}

type allowedDepth5Level3 struct {
	Nested allowedDepth5Level4 `json:"nested"`
}

type allowedDepth5Level4 struct {
	Nested prohibitedNestedPath `json:"nested"`
}

type unexportedProhibitedInput struct {
	path string
}

type cleanInput struct {
	Value string `json:"value"`
}

type failingExecutor struct{}

func (failingExecutor) ExecuteTool(context.Context, string, any) (any, error) {
	return map[string]any{"status": "failed"}, errors.New("executor failed")
}

func TestUnderstandResultJSONRemainsBackwardCompatible(t *testing.T) {
	t.Parallel()
	legacy, err := json.Marshal(UnderstandResult{
		DraftID: "draft", JobID: "job", AssetIDs: []string{"asset"}, Status: "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(legacy) != `{"draft_id":"draft","job_id":"job","asset_ids":["asset"],"status":"completed"}` {
		t.Fatalf("旧结果 JSON 形状被破坏: %s", legacy)
	}
	var decoded UnderstandResult
	if err := json.Unmarshal([]byte(`{"draft_id":"draft","job_id":"job","asset_ids":["asset"],"status":"completed"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != "completed" || decoded.Summaries != nil {
		t.Fatalf("旧 JSON 无法兼容解码: %#v", decoded)
	}
}

func TestAssetManifestModelFacingFieldsHaveDescriptions(t *testing.T) {
	t.Parallel()
	typeValue := reflect.TypeFor[AssetManifest]()
	modelFacingFields := 0
	for index := 0; index < typeValue.NumField(); index++ {
		field := typeValue.Field(index)
		if field.PkgPath != "" {
			continue
		}
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "-" {
			continue
		}
		modelFacingFields++
		if jsonName == "" {
			t.Errorf("AssetManifest.%s 缺少 json 字段名", field.Name)
		}
		if description := strings.TrimSpace(field.Tag.Get("jsonschema_description")); description == "" {
			t.Errorf("AssetManifest.%s(%s) 缺少 jsonschema_description", field.Name, jsonName)
		}
	}
	if modelFacingFields != 11 {
		t.Fatalf("AssetManifest 面向模型的 JSON 字段数=%d want=11", modelFacingFields)
	}
}

func TestLLMToolInputFieldsHaveDescriptions(t *testing.T) {
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

	for _, spec := range registry.Specs(true) {
		if spec.Exposure != ExposureLLM {
			continue
		}
		assertInputFieldDescriptions(t, spec.Name, spec.InputType, map[reflect.Type]bool{})
	}
}

func TestRegistryDecodeInputCoversEveryLLMTool(t *testing.T) {
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

	decodedCount := 0
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != ExposureLLM {
			continue
		}
		decoded, decodeErr := registry.DecodeInput(spec.Name, minimalDecodeArguments(spec.InputType))
		if decodeErr != nil {
			t.Errorf("DecodeInput(%s): %v", spec.Name, decodeErr)
			continue
		}
		if got := reflect.TypeOf(decoded); got != spec.InputType {
			t.Errorf("DecodeInput(%s) type=%v want=%v", spec.Name, got, spec.InputType)
		}
		decodedCount++
	}
	if decodedCount == 0 {
		t.Fatal("没有覆盖任何 LLM 工具")
	}

	talkingHead, err := registry.DecodeInput("timeline.edit_talking_head", map[string]any{
		"a_roll_timeline_clip_id": "clip_v1_001",
		"remove_utterance_ids":    []any{"utt_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	talkingHeadInput := talkingHead.(TalkingHeadEditInput)
	if talkingHeadInput.ARollTimelineClipID != "clip_v1_001" || len(talkingHeadInput.RemoveUtteranceIDs) != 1 {
		t.Fatalf("talking head input=%#v", talkingHeadInput)
	}
	if _, err := registry.DecodeInput("timeline.edit_talking_head", map[string]any{
		"a_roll_timeline_clip_id":      "clip_v1_001",
		"preserve_speech_fragment_ids": []any{"legacy_fragment"},
	}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("legacy fragment preservation input must be rejected: %v", err)
	}
	talkingHeadTool := registry.specs["timeline.edit_talking_head"].Implementation.(einotool.InvokableTool)
	if _, err := talkingHeadTool.InvokableRun(
		WithDraftID(t.Context(), "draft"),
		`{"a_roll_timeline_clip_id":"clip_v1_001","remove_utterance_ids":["utt_1"],"preserve_speech_fragment_ids":["legacy_fragment"]}`,
	); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("production tool path must reject legacy fragment preservation input: %v", err)
	}
	speech, err := registry.DecodeInput("speech.inspect", map[string]any{
		"timeline_clip_id": "clip_v1_001", "query": "口播",
	})
	if err != nil || speech.(SpeechInspectInput).Query != "口播" {
		t.Fatalf("speech input=%#v err=%v", speech, err)
	}
	if _, err := registry.DecodeInput("timeline.inspect", map[string]any{"unknown": true}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err=%v", err)
	}
	if _, err := registry.DecodeInput("missing", map[string]any{}); err == nil {
		t.Fatal("未注册工具必须拒绝解码")
	}
	for name, arguments := range map[string]map[string]any{
		"render.final_mp4":       {"orientation": nil},
		"timeline.apply_patches": {"ops": []any{nil}},
	} {
		if _, err := registry.DecodeInput(name, arguments); err == nil || !strings.Contains(err.Error(), "不允许为 null") {
			t.Errorf("DecodeInput(%s) explicit null err=%v", name, err)
		}
	}
}

func TestRegistryConfirmationValidationRejectsUnsafeTargets(t *testing.T) {
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

	ctx := WithDraftID(t.Context(), "draft_confirmation_validation")
	if err := registry.ValidateConfirmation(ctx, "timeline.inspect", map[string]any{}); err != nil {
		t.Fatalf("timeline.inspect should be confirmable: %v", err)
	}
	for _, fixture := range []struct {
		name string
		args map[string]any
	}{
		{name: "missing", args: map[string]any{}},
		{name: "asset.import_local_file", args: map[string]any{}},
		{name: "interaction.ask_user", args: map[string]any{}},
		{name: "interaction.confirm_action", args: map[string]any{}},
		{name: "decision.answer", args: map[string]any{}},
		{name: "timeline.inspect", args: map[string]any{"unknown": true}},
		{name: "understand.materials", args: nil},
		{name: "understand.materials", args: map[string]any{}},
		{name: "render.final_mp4", args: map[string]any{"orientation": nil}},
		{name: "timeline.compose_initial", args: map[string]any{"clips": []any{map[string]any{}}}},
		{name: "timeline.apply_patches", args: map[string]any{"ops": []any{nil}}},
		{name: "timeline.apply_patches", args: map[string]any{"ops": []any{map[string]any{}}}},
		{name: "timeline.apply_patches", args: map[string]any{"ops": []any{map[string]any{"kind": "delete_clip"}}}},
		{name: "timeline.apply_patches", args: map[string]any{"ops": []any{map[string]any{"kind": "unknown"}}}},
		{name: "timeline.apply_patches", args: map[string]any{"ops": []any{map[string]any{"kind": "delete_clip", "clip_id": "clip_1", "extra": true}}}},
	} {
		if err := registry.ValidateConfirmation(ctx, fixture.name, fixture.args); err == nil {
			t.Errorf("ValidateConfirmation(%s) should fail", fixture.name)
		}
	}
	if _, err := registry.DecodeInput("understand.materials", nil); err == nil || !strings.Contains(err.Error(), "必须是 JSON 对象") {
		t.Fatalf("DecodeInput nil arguments err=%v", err)
	}
}

func minimalDecodeArguments(input reflect.Type) map[string]any {
	for input.Kind() == reflect.Pointer {
		input = input.Elem()
	}
	arguments := map[string]any{}
	if input.Kind() != reflect.Struct {
		return arguments
	}
	for index := range input.NumField() {
		field := input.Field(index)
		if field.PkgPath != "" || !strings.Contains(field.Tag.Get("jsonschema"), "required") {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		arguments[name] = minimalDecodeValue(field.Type)
	}
	return arguments
}

func minimalDecodeValue(value reflect.Type) any {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value == reflect.TypeFor[TimelineOp]() {
		return timeline.CorrectOpExample(timeline.Catalog[0])
	}
	switch value.Kind() {
	case reflect.String:
		return "fixture"
	case reflect.Bool:
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return 1
	case reflect.Slice, reflect.Array:
		return []any{minimalDecodeValue(value.Elem())}
	case reflect.Map, reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		return minimalDecodeArguments(value)
	default:
		return nil
	}
}

func assertInputFieldDescriptions(t *testing.T, path string, input reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	for input.Kind() == reflect.Pointer || input.Kind() == reflect.Slice || input.Kind() == reflect.Array {
		input = input.Elem()
	}
	if input.Kind() != reflect.Struct || seen[input] {
		return
	}
	seen[input] = true
	for index := range input.NumField() {
		field := input.Field(index)
		if field.PkgPath != "" {
			continue
		}
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "-" {
			continue
		}
		if jsonName == "" {
			jsonName = field.Name
		}
		fieldPath := path + "." + jsonName
		if strings.TrimSpace(field.Tag.Get("jsonschema_description")) == "" {
			t.Errorf("%s 缺少 jsonschema_description", fieldPath)
		}
		assertInputFieldDescriptions(t, fieldPath, field.Type, seen)
	}
}

func TestAudioWaveformSampleFramesDescriptionRetainsContextSemantics(t *testing.T) {
	t.Parallel()
	field, exists := reflect.TypeFor[AudioWaveformEnvelope]().FieldByName("SampleFrames")
	if !exists {
		t.Fatal("AudioWaveformEnvelope.SampleFrames missing")
	}
	description := field.Tag.Get("jsonschema_description")
	for _, fragment := range []string{"timeline_fps", "一一对应", "完整压缩波形", "WorldState", "24 点摘要"} {
		if !strings.Contains(description, fragment) {
			t.Errorf("SampleFrames description 丢失 %q: %q", fragment, description)
		}
	}
}

func TestLLMToolDescriptionsRetainOwnedContracts(t *testing.T) {
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
	descriptions := make(map[string]string)
	for _, spec := range registry.Specs(true) {
		if spec.Exposure == ExposureLLM {
			descriptions[spec.Name] = spec.Description
		}
	}
	want := map[string][]string{
		"asset.list_assets": {
			"当前草稿", "可用素材",
		},
		"understand.materials": {
			"默认直接复用持久化结果", "force_refresh=true",
		},
		"media.search_shots": {
			"understanding_candidates", "understand.materials", "禁止把候选文件臆造为 shot_id",
		},
		"timeline.compose_initial": {
			"video/image", "不能传 audio/font", "asset.list_assets", "duration_frames", "timeline_fps",
		},
		"plan.update": {
			"RFC 7396", "reset=true", "跨回合",
		},
		"timeline.apply_patches": {
			"insert_clip", "delete_clip", "同一次调用", "BGM/SFX", "timeline.recut_to_beats",
		},
		"timeline.recut_to_beats": {
			"shot_ids", "cut_frames 可多于视频素材数", "use_all_video_assets=true",
			"cover_entire_bgm=true", "SFX 始终独立分轨", "禁止用 compose_initial",
		},
		"timeline.inspect": {
			"完整 track/clip ID", "timeline_exists=false",
		},
	}
	for toolName, fragments := range want {
		description, exists := descriptions[toolName]
		if !exists {
			t.Errorf("LLM 工具未注册: %s", toolName)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s Description 丢失其应承载的契约 %q: %q", toolName, fragment, description)
			}
		}
	}
}

func TestCoreInferToolRegistry(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	insertToolDraft(t, database, "draft_tools")
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	core := registry.Specs(false)
	if len(core) != 20 {
		t.Fatalf("core tools=%d", len(core))
	}
	if len(registry.Specs(true)) != 22 {
		t.Fatalf("all tools=%d", len(registry.Specs(true)))
	}
	for _, spec := range registry.Specs(true) {
		info, infoErr := spec.Implementation.Info(t.Context())
		if infoErr != nil || info.Name != spec.Name || info.Desc == "" {
			t.Fatalf("spec=%s info=%#v err=%v", spec.Name, info, infoErr)
		}
	}
	if got := len(registry.EinoTools(false, false)); got != 19 {
		t.Fatalf("LLM core tools=%d", got)
	}
	if got := len(registry.EinoTools(false, true)); got != 20 {
		t.Fatalf("含 harness core tools=%d", got)
	}

	ctx := WithDraftID(t.Context(), "draft_tools")
	var listTool einotool.InvokableTool
	for _, spec := range core {
		if spec.Name == "asset.list_assets" {
			listTool = spec.Implementation.(einotool.InvokableTool)
		}
	}
	if listTool == nil {
		t.Fatal("asset.list_assets 未构造为 InvokableTool")
	}
	raw, err := listTool.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var result AssetListResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.DraftID != "draft_tools" {
		t.Fatalf("result=%s err=%v", raw, err)
	}
}

func TestPlanUpdateIsAlwaysAvailableWithTypedSchema(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	insertToolDraft(t, database, "draft_plan_schema")
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	var planUpdate Spec
	for _, spec := range registry.Specs(true) {
		if spec.Name == "plan.update" {
			planUpdate = spec
			break
		}
	}
	if planUpdate.Implementation == nil || planUpdate.Exposure != ExposureLLM ||
		planUpdate.Optional || len(planUpdate.Requires) != 0 ||
		planUpdate.InputType != reflect.TypeFor[PlanUpdateInput]() {
		t.Fatalf("plan.update spec=%#v", planUpdate)
	}
	if prohibitedField(reflect.TypeFor[PlanUpdateInput]()) != "" {
		t.Fatal("PlanUpdateInput 顶层字段不应触发 PolicyGate")
	}
	info, err := planUpdate.Implementation.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := info.ToJSONSchema()
	if err != nil || parameters == nil || parameters.Properties == nil {
		t.Fatalf("parameters=%#v err=%v", parameters, err)
	}
	planSchema, planExists := parameters.Properties.Get("plan")
	resetSchema, resetExists := parameters.Properties.Get("reset")
	if !planExists || planSchema.Type != "object" || !containsString(parameters.Required, "plan") {
		t.Fatalf("plan schema=%#v required=%v", planSchema, parameters.Required)
	}
	if !resetExists || resetSchema.Type != "boolean" || containsString(parameters.Required, "reset") {
		t.Fatalf("reset schema=%#v required=%v", resetSchema, parameters.Required)
	}
	allowed, err := registry.Allowed(WithDraftID(t.Context(), "draft_plan_schema"), false)
	if err != nil || !containsSpec(allowed, "plan.update") {
		t.Fatalf("allowed=%#v err=%v", allowed, err)
	}
}

func TestMemoryUpdateSchemaCannotAcceptModelSuppliedEvidence(t *testing.T) {
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
	var memoryUpdate Spec
	for _, spec := range registry.Specs(true) {
		if spec.Name == "memory.update" {
			memoryUpdate = spec
			break
		}
	}
	if memoryUpdate.Implementation == nil || memoryUpdate.Exposure != ExposureLLM ||
		memoryUpdate.Optional || len(memoryUpdate.Requires) != 0 ||
		memoryUpdate.InputType != reflect.TypeFor[MemoryUpdateInput]() {
		t.Fatalf("memory.update spec=%#v", memoryUpdate)
	}
	if field := prohibitedField(reflect.TypeFor[MemoryUpdateInput]()); field != "" {
		t.Fatalf("MemoryUpdateInput 触发 PolicyGate: %s", field)
	}
	info, err := memoryUpdate.Implementation.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := info.ToJSONSchema()
	if err != nil || parameters == nil || parameters.Properties == nil || len(parameters.AnyOf) != 2 {
		t.Fatalf("memory.update parameters=%#v err=%v", parameters, err)
	}
	entries, entriesOK := parameters.Properties.Get("entries")
	removals, removalsOK := parameters.Properties.Get("remove_keys")
	if !entriesOK || entries.MinItems == nil || *entries.MinItems != 1 ||
		entries.MaxItems == nil || *entries.MaxItems != 8 || entries.Items == nil ||
		!removalsOK || removals.MinItems == nil || *removals.MinItems != 1 ||
		removals.MaxItems == nil || *removals.MaxItems != 50 ||
		!containsString(parameters.AnyOf[0].Required, "entries") ||
		!containsString(parameters.AnyOf[1].Required, "remove_keys") {
		t.Fatalf("entries=%#v removals=%#v anyOf=%#v", entries, removals, parameters.AnyOf)
	}
	kind, kindOK := entries.Items.Properties.Get("kind")
	key, keyOK := entries.Items.Properties.Get("key")
	statement, statementOK := entries.Items.Properties.Get("statement")
	quote, quoteOK := entries.Items.Properties.Get("evidence_quote")
	if !kindOK || len(kind.Enum) != 3 || !keyOK || key.Pattern != "^[a-z0-9_]{2,40}$" ||
		!statementOK || statement.MaxLength == nil || *statement.MaxLength != 200 ||
		!quoteOK || quote.MinLength == nil || *quote.MinLength != 2 ||
		!containsString(entries.Items.Required, "evidence_quote") {
		t.Fatalf("entry schema=%#v", entries.Items)
	}
	if _, err := registry.DecodeInput("memory.update", map[string]any{
		"entries": []any{map[string]any{
			"key": "pacing", "kind": "preference", "statement": "偏快",
			"evidence_id": "forged",
		}},
	}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("模型伪造 evidence 应被严格解码拒绝: %v", err)
	}
}

func TestDecisionAnswerSchemaRequiresAnAnswerForm(t *testing.T) {
	t.Parallel()
	schema := (DecisionAnswerInput{}).JSONSchema()
	if !containsString(schema.Required, "decision_id") || len(schema.AnyOf) != 2 ||
		!containsString(schema.AnyOf[0].Required, "option_id") ||
		!containsString(schema.AnyOf[1].Required, "free_text") {
		t.Fatalf("decision answer schema=%#v", schema)
	}
}

func TestPreconditionRegistryPrunesAndUnlocksTools(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	insertToolDraft(t, database, "draft_gate")
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithDraftID(t.Context(), "draft_gate")
	allowed, err := registry.Allowed(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if containsSpec(allowed, "timeline.compose_initial") || containsSpec(allowed, "render.preview") {
		t.Fatalf("空草稿错误放行: %#v", allowed)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('asset','reference','video','local_path','a.mp4','hash',1,'ready',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at,note) VALUES('draft_gate','asset',?,'')`, now); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	if !containsSpec(allowed, "timeline.compose_initial") {
		t.Fatal("可用素材存在后 compose 未放行")
	}
	if !containsSpec(allowed, "audio.analyze_beats") {
		t.Fatal("可用素材存在后节拍分析未放行")
	}
	if !containsSpec(allowed, "audio.analyze_speech_pauses") {
		t.Fatal("可用素材存在后气口分析未放行")
	}
	if !containsSpec(allowed, "timeline.recut_to_beats") {
		t.Fatal("可用素材存在后，空时间线应直接放行卡点重剪")
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE drafts SET timeline_current_version=1, timeline_validated=1 WHERE draft_id='draft_gate'"); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	for _, name := range []string{"timeline.apply_patches", "timeline.recut_to_beats", "timeline.validate", "timeline.inspect", "render.preview", "render.final_mp4", "render.status"} {
		if !containsSpec(allowed, name) {
			t.Fatalf("%s 未放行", name)
		}
	}
	if containsSpec(allowed, "render.inspect_preview") {
		t.Fatal("没有 preview 时不应放行 inspect_preview")
	}
	now = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES('hash','hash',1,?);
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview','draft_gate',1,'hash','{}',?)`, now, now); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	if !containsSpec(allowed, "render.inspect_preview") {
		t.Fatal("preview 存在后 inspect_preview 未放行")
	}
	if passed, err := EvaluatePrecondition(ctx, database, "draft_gate", "unknown"); err == nil || passed {
		t.Fatalf("unknown predicate passed=%v err=%v", passed, err)
	}
}

func TestRegistryValidationConversionReporterAndMissingContext(t *testing.T) {
	t.Parallel()
	_ = unexportedProhibitedInput{}.path
	if _, err := NewRegistry(nil, fakeExecutor{}); err == nil {
		t.Fatal("nil database should fail")
	}
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := NewRegistry(database, nil); err == nil {
		t.Fatal("nil executor should fail")
	}
	if _, err := DraftID(t.Context()); err == nil {
		t.Fatal("missing draft should fail")
	}
	if prohibitedField(reflect.TypeFor[prohibitedPathInput]()) != "path" ||
		prohibitedField(reflect.TypeFor[prohibitedRevisionInput]()) != "timeline_revision" ||
		prohibitedField(reflect.TypeFor[prohibitedNestedInput]()) != "path" ||
		prohibitedField(reflect.TypeFor[prohibitedNestedPointerInput]()) != "path" ||
		prohibitedField(reflect.TypeFor[prohibitedNestedArrayInput]()) != "path" ||
		prohibitedField(reflect.TypeFor[*prohibitedFrameInput]()) != "" ||
		prohibitedField(reflect.TypeFor[ignoredProhibitedNestedInput]()) != "" ||
		prohibitedField(reflect.TypeFor[recursiveCleanInput]()) != "" ||
		prohibitedField(reflect.TypeFor[recursiveCleanSlice]()) != "" ||
		prohibitedField(reflect.TypeFor[prohibitedDepth4Input]()) != "path" ||
		prohibitedField(reflect.TypeFor[allowedDepth5Input]()) != "" ||
		prohibitedField(reflect.TypeFor[unexportedProhibitedInput]()) != "" ||
		prohibitedField(reflect.TypeFor[string]()) != "" ||
		prohibitedField(reflect.TypeFor[cleanInput]()) != "" {
		t.Fatal("PolicyGate field detection mismatch")
	}

	registry := &Registry{database: database, executor: failingExecutor{}, specs: map[string]Spec{}}
	if err := addTool[cleanInput, ToolResult](registry, "clean", "clean", nil, ExposureLLM, false); err != nil {
		t.Fatal(err)
	}
	if err := addTool[cleanInput, ToolResult](registry, "clean", "duplicate", nil, ExposureLLM, false); err == nil {
		t.Fatal("duplicate tool should fail")
	}
	if err := addTool[prohibitedPathInput, ToolResult](registry, "bad", "bad", nil, ExposureLLM, false); err == nil {
		t.Fatal("prohibited field should fail")
	}
	if err := addTool[prohibitedNestedInput, ToolResult](registry, "nested_bad", "bad", nil, ExposureLLM, false); err == nil {
		t.Fatal("nested prohibited field should fail")
	}

	tool := registry.specs["clean"].Implementation.(einotool.InvokableTool)
	if _, err := tool.InvokableRun(t.Context(), `{}`); err == nil {
		t.Fatal("missing draft context should fail")
	}
	reports := []string{}
	ctx := WithReporter(WithDraftID(t.Context(), "draft"), func(_ context.Context, name, phase string, _, _ any, err error) {
		reports = append(reports, name+":"+phase)
		if phase == "finished" && err == nil {
			t.Fatal("executor error missing from reporter")
		}
	})
	if _, err := tool.InvokableRun(ctx, `{"value":"x"}`); err == nil {
		t.Fatal("executor failure should propagate")
	}
	if len(reports) != 2 {
		t.Fatalf("reports=%v", reports)
	}

	converted, err := convertResult[ToolResult](map[string]any{"status": "ok"})
	if err != nil || converted.Status != "ok" {
		t.Fatalf("converted=%#v err=%v", converted, err)
	}
	if _, err := convertResult[ToolResult](make(chan int)); err == nil {
		t.Fatal("unmarshalable result should fail")
	}
	if _, err := convertResult[ToolResult]("wrong-shape"); err == nil {
		t.Fatal("wrong result shape should fail")
	}
	if passed, err := EvaluatePrecondition(t.Context(), database, "missing", "timeline_exists"); err != nil || passed {
		t.Fatalf("missing draft passed=%v err=%v", passed, err)
	}
}

func insertToolDraft(t *testing.T, database *storage.DB, draftID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(
			draft_id,name,state_version,status,defaults_json,running_jobs_json,brief_json,
			timeline_validated,created_at,updated_at
		) VALUES(?,?,0,'active','{}','[]','{"goal":""}',0,?,?)`, draftID, draftID, now, now)
	if err != nil {
		t.Fatal(err)
	}
}

func containsSpec(specs []Spec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}
