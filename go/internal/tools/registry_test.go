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
)

type fakeExecutor struct{}

func (fakeExecutor) ExecuteTool(ctx context.Context, name string, _ any) (any, error) {
	draftID, _ := DraftID(ctx)
	switch name {
	case "asset.list_assets":
		return AssetListResult{DraftID: draftID, Assets: []AssetManifest{}, Total: 0}, nil
	case "media.detect_shots":
		return DetectShotsResult{DraftID: draftID, JobID: "job", AssetID: "asset", Status: "queued"}, nil
	case "shot.search":
		return ShotSearchResult{Shots: []ShotCandidate{}, TotalMatches: 0}, nil
	case "shot.deep_search":
		return ShotDeepSearchResult{
			Status: "succeeded", Candidates: []ShotDeepCandidate{},
		}, nil
	case "audio.analyze_beats":
		return AudioBeatAnalysisResult{AssetID: "audio", BPM: 120, BeatFrames: []int{0, 15}}, nil
	case "audio.analyze_speech_pauses":
		return SpeechPauseAnalysisResult{AssetID: "audio", TimelineFPS: 30, Pauses: []SpeechPauseCandidate{}}, nil
	case "speech.search":
		return SpeechSearchResult{
			Status: "succeeded", AssetID: "video", TimelineFPS: 30,
			Utterances: []SpeechUtteranceEvidence{},
		}, nil
	case "speech.transcribe":
		return SpeechTranscribeResult{AssetID: "video", TimelineFPS: 30}, nil
	case "preview.check":
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

type requiredNestedInput struct {
	Value string `json:"value" jsonschema:"required"`
}

type requiredEnvelopeInput struct {
	Item    *requiredNestedInput  `json:"item" jsonschema:"required"`
	Items   []requiredNestedInput `json:"items"`
	Ignored string                `json:"-"`
	private string
}

type failingExecutor struct{}

func (failingExecutor) ExecuteTool(context.Context, string, any) (any, error) {
	return map[string]any{"status": "failed"}, errors.New("executor failed")
}

type untypedErrorExecutor struct{}

func (untypedErrorExecutor) ExecuteTool(context.Context, string, any) (any, error) {
	return map[string]any{"error": "adapter boom"}, nil
}

func TestDetectShotsResultJSONUsesSingleAssetShape(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(DetectShotsResult{
		DraftID: "draft", JobID: "job", AssetID: "asset", Status: "succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "asset_ids") ||
		!strings.Contains(string(encoded), `"asset_id":"asset"`) {
		t.Fatalf("检测结果不是单素材形状: %s", encoded)
	}
	var decoded DetectShotsResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != "succeeded" || decoded.AssetID != "asset" {
		t.Fatalf("单素材 JSON 无法解码: %#v", decoded)
	}
}

func TestValidateRequiredFieldsRejectsMalformedAtomicInputs(t *testing.T) {
	t.Parallel()
	assertError := func(name string, input reflect.Type, value any) {
		t.Helper()
		if err := validateRequiredFields(input, value, "arguments"); err == nil {
			t.Fatalf("%s should fail", name)
		}
	}

	assertError("nil pointer", reflect.TypeFor[*requiredNestedInput](), nil)
	assertError("atomic input is not object", reflect.TypeFor[TimelineInsertInput](), []any{})
	assertError(
		"atomic input missing fields",
		reflect.TypeFor[TimelineInsertInput](),
		map[string]any{},
	)
	assertError(
		"nested required field missing",
		reflect.TypeFor[requiredEnvelopeInput](),
		map[string]any{"item": map[string]any{}},
	)
	assertError(
		"slice contains null",
		reflect.TypeFor[[]requiredNestedInput](),
		[]any{nil},
	)

	if err := validateRequiredFields(
		reflect.TypeFor[requiredEnvelopeInput](),
		"not-an-object",
		"arguments",
	); err != nil {
		t.Fatalf("non-object struct is handled by JSON decoding before required validation: %v", err)
	}
	if err := validateRequiredFields(
		reflect.TypeFor[[]requiredNestedInput](),
		"not-an-array",
		"arguments",
	); err != nil {
		t.Fatalf("non-array slice is handled by JSON decoding before required validation: %v", err)
	}
	if name, atomic := atomicTimelineToolForType(reflect.TypeFor[string]()); atomic || name != "" {
		t.Fatalf("non-atomic type classified as %q", name)
	}
	if value := atomicTimelineInputValue(reflect.TypeFor[string](), map[string]any{}); value != nil {
		t.Fatalf("non-atomic value=%#v", value)
	}
	if err := validateRequiredFields(
		reflect.TypeFor[requiredEnvelopeInput](),
		map[string]any{
			"item":  map[string]any{"value": "ok"},
			"items": []any{},
		},
		"arguments",
	); err != nil {
		t.Fatalf("valid nested input=%v", err)
	}
	_ = requiredEnvelopeInput{private: "not-model-facing"}
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

func TestEveryToolHasValidEffect(t *testing.T) {
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
		if !spec.Effect.Valid() {
			t.Fatalf("工具 %s 缺少合法 Effect 分级: %q", spec.Name, spec.Effect)
		}
		effect, ok := registry.Effect(spec.Name)
		if !ok || effect != spec.Effect {
			t.Fatalf("registry.Effect(%s)=%q,%v 与 spec.Effect=%q 不一致", spec.Name, effect, ok, spec.Effect)
		}
	}
	if _, ok := registry.Effect("does.not.exist"); ok {
		t.Fatal("未注册工具的 Effect 应返回 false")
	}
}

func TestModelReceiptPoliciesAreRegistryOwned(t *testing.T) {
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

	typedAdapters := map[string]bool{
		"shot.search":                false,
		"shot.deep_search":           false,
		"speech.search":              false,
		"interaction.ask_user":       false,
		"decision.answer":            false,
		"plan.update":                false,
		"memory.set":                 false,
		"memory.remove":              false,
		"timeline.insert":            false,
		"timeline.delete":            false,
		"timeline.update":            false,
		"timeline.split":             false,
		"interaction.confirm_action": false,
	}
	waitingUser := map[string]bool{
		"interaction.ask_user":       true,
		"interaction.confirm_action": true,
	}

	for _, spec := range registry.Specs(true) {
		policy, exists := registry.ModelReceiptPolicy(spec.Name)
		if spec.Exposure == ExposureHarness {
			if spec.CompletionSemantics != "" || spec.TypedSuccessAdapter || exists {
				t.Fatalf("harness 工具 %s 泄漏模型回执合同: spec=%#v policy=%#v exists=%v", spec.Name, spec, policy, exists)
			}
			continue
		}
		wantAdapter, classified := typedAdapters[spec.Name]
		if !classified {
			t.Fatalf("模型工具 %s 未进入 typed adapter 分类表", spec.Name)
		}
		wantCompletion := CompletionTerminalOnly
		if waitingUser[spec.Name] {
			wantCompletion = CompletionTerminalOrWaitingUser
		}
		if !exists || policy.Completion != wantCompletion ||
			policy.TypedSuccessAdapter != wantAdapter ||
			spec.CompletionSemantics != wantCompletion || spec.TypedSuccessAdapter != wantAdapter {
			t.Fatalf("模型工具 %s 回执合同错误: spec=%#v policy=%#v exists=%v", spec.Name, spec, policy, exists)
		}
		for _, status := range []ToolStatus{
			StatusSucceeded, StatusFailed, StatusValidationFailed, StatusCancelled, StatusTimeout,
		} {
			if !policy.Allows(status) {
				t.Fatalf("模型工具 %s 不允许标准终态 %q", spec.Name, status)
			}
		}
		for _, status := range []ToolStatus{"queued", "running", "completed", "mystery", ""} {
			if policy.Allows(status) {
				t.Fatalf("模型工具 %s 错误允许非合同状态 %q", spec.Name, status)
			}
		}
		if policy.Allows(StatusWaiting) != waitingUser[spec.Name] {
			t.Fatalf("模型工具 %s waiting_user 语义错误", spec.Name)
		}
	}
	modelToolCount := 0
	for _, spec := range registry.Specs(true) {
		if spec.Exposure == ExposureLLM {
			modelToolCount++
		}
	}
	if len(typedAdapters) != modelToolCount {
		t.Fatalf("typed adapter 分类数=%d，与模型工具数=%d 不一致", len(typedAdapters), modelToolCount)
	}
	if _, exists := registry.ModelReceiptPolicy("asset.import_local_file"); exists {
		t.Fatal("harness 工具不得有模型回执策略")
	}
	for _, name := range []string{
		"asset.list_assets", "media.detect_shots", "timeline.inspect", "timeline.check",
		"preview.generate", "preview.check",
	} {
		spec, exists := registry.Spec(name)
		if !exists || spec.Exposure != ExposureHarness {
			t.Fatalf("%s exposure=%q exists=%v", name, spec.Exposure, exists)
		}
		for _, modelTool := range registry.EinoTools(true, false) {
			info, infoErr := modelTool.Info(t.Context())
			if infoErr != nil {
				t.Fatal(infoErr)
			}
			if info.Name == name {
				t.Fatalf("Harness-only %s leaked into LLM registry surface", name)
			}
		}
	}
	if _, exists := registry.ModelReceiptPolicy("does.not.exist"); exists {
		t.Fatal("未注册工具不得有模型回执策略")
	}
}

func TestTypedToolRejectsUnknownExecutorErrorEnvelope(t *testing.T) {
	t.Parallel()
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	registry, err := NewRegistry(database, untypedErrorExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	tool := registry.specs["asset.list_assets"].Implementation.(einotool.InvokableTool)
	_, err = tool.InvokableRun(WithDraftID(t.Context(), "draft"), `{}`)
	if err == nil || !strings.Contains(err.Error(), "unknown field \"error\"") {
		t.Fatalf("typed adapter 应拒绝未知错误 envelope，err=%v", err)
	}
}

// TestToolEffectClassificationTable 把 #103 G1 的全量分类表锁成可执行断言：任何工具的
// Effect 被误改或新增工具未标注都会在此失败。speech.transcribe 负责持久化索引，
// speech.search 与 timeline.check 必须严格只读；memory.set 可逆，memory.remove 归破坏性。
func TestToolEffectClassificationTable(t *testing.T) {
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
	expected := map[string]Effect{
		"asset.import_local_file":     EffectReversible, // harness-only
		"asset.list_assets":           EffectReadOnly,
		"shot.search":                 EffectReadOnly,
		"shot.deep_search":            EffectReversible,
		"audio.analyze_beats":         EffectReversible,
		"audio.analyze_speech_pauses": EffectReversible,
		"timeline.inspect":            EffectReadOnly,
		"preview.check":               EffectReadOnly,
		"preview.generate":            EffectReversible,
		"media.detect_shots":          EffectReversible,
		"speech.transcribe":           EffectReversible,
		"speech.search":               EffectReadOnly,
		"plan.update":                 EffectReversible,
		"interaction.ask_user":        EffectReversible,
		"decision.answer":             EffectReversible,
		"interaction.confirm_action":  EffectReversible,
		"timeline.insert":             EffectReversible,
		"timeline.delete":             EffectReversible,
		"timeline.update":             EffectReversible,
		"timeline.split":              EffectReversible,
		"timeline.check":              EffectReadOnly,
		"memory.set":                  EffectReversible,
		"memory.remove":               EffectDestructive,
	}
	specs := registry.Specs(true)
	if len(specs) != len(expected) {
		t.Fatalf("注册工具数 %d 与分类表 %d 不一致，新增/删除工具后请同步分类表", len(specs), len(expected))
	}
	for _, spec := range specs {
		want, ok := expected[spec.Name]
		if !ok {
			t.Fatalf("工具 %s 不在 Effect 分类表内", spec.Name)
		}
		if spec.Effect != want {
			t.Fatalf("工具 %s 的 Effect=%q，分类表期望 %q", spec.Name, spec.Effect, want)
		}
	}
}

func TestToolPrimitiveClassificationMatchesEffectAndSurface(t *testing.T) {
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
	expectedFamily := map[string]Family{
		"asset.import_local_file":     FamilyEdit,
		"asset.list_assets":           FamilyRead,
		"media.detect_shots":          FamilyDetect,
		"shot.search":                 FamilyRead,
		"shot.deep_search":            FamilyDetect,
		"audio.analyze_beats":         FamilyDetect,
		"audio.analyze_speech_pauses": FamilyDetect,
		"speech.transcribe":           FamilyDetect,
		"speech.search":               FamilyRead,
		"interaction.ask_user":        FamilyControl,
		"decision.answer":             FamilyControl,
		"plan.update":                 FamilyControl,
		"memory.set":                  FamilyControl,
		"memory.remove":               FamilyControl,
		"timeline.insert":             FamilyEdit,
		"timeline.delete":             FamilyEdit,
		"timeline.update":             FamilyEdit,
		"timeline.split":              FamilyEdit,
		"timeline.check":              FamilyCheck,
		"timeline.inspect":            FamilyRead,
		"preview.generate":            FamilyEdit,
		"preview.check":               FamilyCheck,
		"interaction.confirm_action":  FamilyControl,
	}
	families := map[Family]bool{}
	for _, spec := range registry.Specs(true) {
		if expectedFamily[spec.Name] != spec.Family {
			t.Errorf("%s family=%q want=%q", spec.Name, spec.Family, expectedFamily[spec.Name])
		}
		if !spec.Family.Valid() || !spec.Cost.Valid() {
			t.Errorf("%s classification invalid: family=%q cost=%q", spec.Name, spec.Family, spec.Cost)
		}
		if err := validateFamilyEffect(spec.Name, spec.Family, spec.Effect); err != nil {
			t.Error(err)
		}
		if spec.Parallelizable() != (spec.Effect == EffectReadOnly) {
			t.Errorf("%s parallelizable drifted from Effect", spec.Name)
		}
		if spec.Exposure == ExposureLLM {
			if spec.Surfaces == 0 || !spec.Surfaces.Includes(spec.PrimarySurface) {
				t.Errorf("%s surface metadata invalid: primary=%d surfaces=%d",
					spec.Name, spec.PrimarySurface, spec.Surfaces)
			}
		}
		families[spec.Family] = true
	}
	if len(expectedFamily) != len(registry.Specs(true)) {
		t.Fatalf("family table=%d specs=%d", len(expectedFamily), len(registry.Specs(true)))
	}
	for _, family := range []Family{FamilyDetect, FamilyRead, FamilyEdit, FamilyCheck, FamilyControl} {
		if !families[family] {
			t.Errorf("registry missing family %q", family)
		}
	}
}

func TestInterceptorChainRunsInOrderAndCanReject(t *testing.T) {
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

	var order []string
	registry.Use(func(_ context.Context, spec Spec, _ any) error {
		order = append(order, "a:"+spec.Name)
		return nil
	})
	registry.Use(func(_ context.Context, spec Spec, _ any) error {
		order = append(order, "b:"+spec.Name)
		if spec.Name == "asset.list_assets" {
			return &InterceptorRejection{Observation: "blocked", Data: map[string]any{"error_code": "x"}}
		}
		return nil
	})
	registry.Use(nil) // nil 拦截器被忽略，不改变链

	ctx := WithDraftID(t.Context(), "draft_interceptor")

	// 被否决：第二个拦截器返回 InterceptorRejection，executor 不执行、错误原样上抛。
	rejected := registry.specs["asset.list_assets"].Implementation.(einotool.InvokableTool)
	_, rejectErr := rejected.InvokableRun(ctx, `{}`)
	var rejection *InterceptorRejection
	if !errors.As(rejectErr, &rejection) || rejection.Data["error_code"] != "x" {
		t.Fatalf("拦截器应否决 asset.list_assets: err=%v", rejectErr)
	}
	if len(order) != 2 || order[0] != "a:asset.list_assets" || order[1] != "b:asset.list_assets" {
		t.Fatalf("拦截器未按注册序运行: %v", order)
	}

	// 放行：无前置的 timeline.inspect 两个拦截器都放行，正常进入 executor。
	order = nil
	allowed := registry.specs["timeline.inspect"].Implementation.(einotool.InvokableTool)
	if _, err := allowed.InvokableRun(ctx, `{}`); err != nil {
		t.Fatalf("放行工具不应报错: %v", err)
	}
	if len(order) != 2 || order[0] != "a:timeline.inspect" || order[1] != "b:timeline.inspect" {
		t.Fatalf("放行路径拦截器未按序运行: %v", order)
	}
}

func TestAdmissionInterceptorRunsBeforePreconditionGuard(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	insertToolDraft(t, database, "draft_admission")
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	registry.UseAdmission(func(_ context.Context, spec Spec, _ any) error {
		return &InterceptorRejection{
			Observation: "not admitted",
			Data:        map[string]any{"tool": spec.Name},
		}
	})

	deleteClip := registry.specs["timeline.delete"].Implementation.(einotool.InvokableTool)
	_, err = deleteClip.InvokableRun(
		WithDraftID(t.Context(), "draft_admission"),
		`{"kind":"delete_clip","timeline_clip_id":"missing"}`,
	)
	var rejection *InterceptorRejection
	if !errors.As(err, &rejection) || rejection.Data["tool"] != "timeline.delete" {
		t.Fatalf("准入拦截器应先于 timeline_exists guard 拒绝: %v", err)
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

	speech, err := registry.DecodeInput("speech.search", map[string]any{
		"timeline_clip_id": "clip_v1_001", "query": "口播",
	})
	if err != nil || speech.(SpeechSearchInput).Query != "口播" {
		t.Fatalf("speech input=%#v err=%v", speech, err)
	}
	if _, err := registry.DecodeInput("timeline.inspect", map[string]any{"unknown": true}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err=%v", err)
	}
	if _, err := registry.DecodeInput("missing", map[string]any{}); err == nil {
		t.Fatal("未注册工具必须拒绝解码")
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
	if err := registry.ValidateConfirmation(ctx, "timeline.inspect", map[string]any{}); err == nil {
		t.Fatal("Harness-only timeline.inspect must not be model-confirmable")
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
		{name: "media.detect_shots", args: nil},
		{name: "media.detect_shots", args: map[string]any{}},
	} {
		if err := registry.ValidateConfirmation(ctx, fixture.name, fixture.args); err == nil {
			t.Errorf("ValidateConfirmation(%s) should fail", fixture.name)
		}
	}
	if _, err := registry.DecodeInput("media.detect_shots", nil); err == nil || !strings.Contains(err.Error(), "必须是 JSON 对象") {
		t.Fatalf("DecodeInput nil arguments err=%v", err)
	}
}

func TestShotDeepSearchSchemaOnlyAcceptsExactShotRefs(t *testing.T) {
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
	valid := map[string]any{
		"query": "旋转动作", "index_snapshot_id": "snapshot",
		"candidate_shots": []any{map[string]any{"asset_id": "asset", "shot_id": "shot"}},
	}
	decoded, err := registry.DecodeInput("shot.deep_search", valid)
	if err != nil {
		t.Fatalf("精确 ShotRef 被拒绝: %v", err)
	}
	input := decoded.(ShotDeepSearchInput)
	if len(input.CandidateShots) != 1 || input.CandidateShots[0].ShotID != "shot" {
		t.Fatalf("decoded=%#v", decoded)
	}
	invalid := map[string]any{
		"query": "旋转动作", "index_snapshot_id": "snapshot",
		"candidate_shots": []any{map[string]any{
			"asset_id": "asset", "shot_id": "shot", "source_start_frame": 0, "source_end_frame": 30,
		}},
	}
	if _, err := registry.DecodeInput("shot.deep_search", invalid); err == nil ||
		!strings.Contains(err.Error(), "source_") {
		t.Fatalf("模型不应向 deep_search 传源范围: %v", err)
	}
}

func minimalDecodeArguments(input reflect.Type) map[string]any {
	for input.Kind() == reflect.Pointer {
		input = input.Elem()
	}
	switch input {
	case reflect.TypeFor[TimelineInsertInput]():
		return map[string]any{
			"kind": "insert_clip", "asset_id": "asset_1",
			"source_start_frame": 0, "source_end_frame": 30,
		}
	case reflect.TypeFor[TimelineDeleteInput]():
		return map[string]any{"kind": "delete_clip", "timeline_clip_id": "clip_1"}
	case reflect.TypeFor[TimelineUpdateInput]():
		return map[string]any{
			"kind": "adjust_gain", "timeline_clip_id": "clip_1", "gain_db": -3,
		}
	case reflect.TypeFor[TimelineSplitInput]():
		return map[string]any{
			"kind": "split_clip", "timeline_clip_id": "clip_1", "split_frame": 15,
		}
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
		"shot.search": {
			"冻结目标视频素材", "search_ready", "index_snapshot_id", "无 embedding", "绝不返回部分索引",
		},
		"shot.deep_search": {
			"精确 ShotRef", "冻结快照", "新增有序帧", "通用事实", "requirements", "exclusions", "preferences", "不能传", "源帧范围",
		},
		"plan.update": {
			"RFC 7396", "reset=true", "跨回合",
		},
		"timeline.insert": {
			"一个素材 clip", "空时间线", "原声联动", "服务端派生",
		},
		"timeline.delete": {
			"一个 clip", "一个连续帧范围", "多个目标", "多次调用",
		},
		"timeline.update": {
			"一个 clip", "track", "subtitle", "淡入淡出",
		},
		"timeline.split": {
			"一个 timeline_clip_id", "一个时间线整数帧",
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
	for _, spec := range registry.Specs(true) {
		info, infoErr := spec.Implementation.Info(t.Context())
		if infoErr != nil || info.Name != spec.Name || info.Desc == "" {
			t.Fatalf("spec=%s info=%#v err=%v", spec.Name, info, infoErr)
		}
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

func TestMemorySchemasSeparateSetFromDestructiveRemove(t *testing.T) {
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
	var memorySet, memoryRemove Spec
	for _, spec := range registry.Specs(true) {
		switch spec.Name {
		case "memory.set":
			memorySet = spec
		case "memory.remove":
			memoryRemove = spec
		}
	}
	if memorySet.Implementation == nil || memorySet.Exposure != ExposureLLM ||
		memorySet.Optional || len(memorySet.Requires) != 0 ||
		memorySet.InputType != reflect.TypeFor[MemorySetInput]() ||
		memorySet.Effect != EffectReversible {
		t.Fatalf("memory.set spec=%#v", memorySet)
	}
	if memoryRemove.Implementation == nil || memoryRemove.Exposure != ExposureLLM ||
		memoryRemove.Optional || len(memoryRemove.Requires) != 0 ||
		memoryRemove.InputType != reflect.TypeFor[MemoryRemoveInput]() ||
		memoryRemove.Effect != EffectDestructive {
		t.Fatalf("memory.remove spec=%#v", memoryRemove)
	}
	for _, inputType := range []reflect.Type{
		reflect.TypeFor[MemorySetInput](), reflect.TypeFor[MemoryRemoveInput](),
	} {
		if field := prohibitedField(inputType); field != "" {
			t.Fatalf("%s 触发 PolicyGate: %s", inputType, field)
		}
	}
	info, err := memorySet.Implementation.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := info.ToJSONSchema()
	if err != nil || parameters == nil || parameters.Properties == nil ||
		!containsString(parameters.Required, "entries") {
		t.Fatalf("memory.set parameters=%#v err=%v", parameters, err)
	}
	entries, entriesOK := parameters.Properties.Get("entries")
	if !entriesOK || entries.MinItems == nil || *entries.MinItems != 1 ||
		entries.MaxItems == nil || *entries.MaxItems != 8 || entries.Items == nil {
		t.Fatalf("entries=%#v", entries)
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
	if _, err := registry.DecodeInput("memory.set", map[string]any{
		"entries": []any{map[string]any{
			"key": "pacing", "kind": "preference", "statement": "偏快",
			"evidence_id": "forged",
		}},
	}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("模型伪造 evidence 应被严格解码拒绝: %v", err)
	}
	removeInfo, err := memoryRemove.Implementation.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	removeParameters, err := removeInfo.ToJSONSchema()
	if err != nil || removeParameters == nil || removeParameters.Properties == nil ||
		!containsString(removeParameters.Required, "keys") {
		t.Fatalf("memory.remove parameters=%#v err=%v", removeParameters, err)
	}
	keys, ok := removeParameters.Properties.Get("keys")
	if !ok || keys.MinItems == nil || *keys.MinItems != 1 ||
		keys.MaxItems == nil || *keys.MaxItems != 50 {
		t.Fatalf("memory.remove keys=%#v", keys)
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
	if _, exists := PreconditionRegistry["timeline_absent"]; exists {
		t.Fatal("已删除的 compose_initial 不得留下 timeline_absent 准入分支")
	}
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
	if !containsSpec(allowed, "timeline.insert") {
		t.Fatal("空时间线应始终保留首个原子 insert，具体素材 ID 由执行器校验")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO assets(asset_id,storage_mode,kind,source,filename,hash,size,ingest_status,usable)
		VALUES('asset','reference','video','local_path','a.mp4','hash',1,'ready',1);
		INSERT INTO draft_asset_links(draft_id,asset_id,linked_at,note) VALUES('draft_gate','asset',?,'')`, now); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	if !containsSpec(allowed, "timeline.insert") {
		t.Fatal("空时间线应允许模型用首个原子 insert 建立 v1")
	}
	for _, internal := range []string{
		"audio.analyze_beats", "audio.analyze_speech_pauses", "speech.transcribe",
	} {
		if containsSpec(allowed, internal) {
			t.Fatalf("Harness 内部分析工具不得进入模型允许集: %s", internal)
		}
	}
	if !containsSpec(allowed, "speech.search") {
		t.Fatal("speech.search 应在 Harness 自动确保 transcript 前即可披露")
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO transcripts(
			transcript_id,asset_id,provider_id,raw_preserved,utterances_json,vad_segments_json
		) VALUES('transcript_gate','asset','fixture',0,'[]','[]')`); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	if !containsSpec(allowed, "speech.search") {
		t.Fatal("任一关联可用素材已有 transcript 索引后应披露 speech.search")
	}
	if _, err := database.Write().ExecContext(t.Context(),
		"UPDATE drafts SET timeline_current_version=1, timeline_validated=0 WHERE draft_id='draft_gate'"); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	for _, name := range []string{
		"timeline.insert", "timeline.delete", "timeline.update", "timeline.split",
	} {
		if !containsSpec(allowed, name) {
			t.Fatalf("已有但未标记 validated 的时间线也应放行 %s", name)
		}
	}
	if containsSpec(allowed, "render.start") || containsSpec(allowed, "job.read") {
		t.Fatal("Agent Tool Registry 不得披露 render.start 或 job.read")
	}
	for _, internal := range []string{"asset.list_assets", "preview.generate", "preview.check"} {
		if containsSpec(allowed, internal) {
			t.Fatalf("Harness-only 工具不得进入模型允许集: %s", internal)
		}
	}
	now = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES('hash','hash',1,?);
		INSERT INTO previews(preview_id,draft_id,timeline_version,object_hash,quality_json,created_at)
		VALUES('preview','draft_gate',1,'hash','{}',?)`, now, now); err != nil {
		t.Fatal(err)
	}
	allowed, _ = registry.Allowed(ctx, true)
	for _, internal := range []string{"asset.list_assets", "preview.generate", "preview.check"} {
		if containsSpec(allowed, internal) {
			t.Fatalf("状态变化后 Harness-only 工具仍不得进入模型允许集: %s", internal)
		}
	}
	if passed, err := EvaluatePrecondition(ctx, database, "draft_gate", "unknown"); err == nil || passed {
		t.Fatalf("unknown predicate passed=%v err=%v", passed, err)
	}
}

func TestAllowedPropagatesPreconditionEvaluationErrors(t *testing.T) {
	database, err := storage.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	insertToolDraft(t, database, "draft_allowed_error")
	registry, err := NewRegistry(database, fakeExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	allowed, err := registry.Allowed(WithDraftID(t.Context(), "draft_allowed_error"), true)
	if err == nil {
		t.Fatalf("关闭数据库后 Allowed=%v，预期传播查询错误", allowed)
	}
	if !strings.Contains(err.Error(), "判断工具") || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("Allowed error=%v", err)
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
	readMetadata := terminalMetadata(FamilyRead, CostLow, SurfaceDiscovery)
	if err := addTool[cleanInput, ToolResult](registry, "clean", "clean", nil, ExposureLLM, EffectReadOnly, false, readMetadata); err != nil {
		t.Fatal(err)
	}
	if err := addTool[cleanInput, ToolResult](registry, "clean", "duplicate", nil, ExposureLLM, EffectReadOnly, false, readMetadata); err == nil {
		t.Fatal("duplicate tool should fail")
	}
	if err := addTool[prohibitedPathInput, ToolResult](registry, "bad", "bad", nil, ExposureLLM, EffectReadOnly, false, readMetadata); err == nil {
		t.Fatal("prohibited field should fail")
	}
	if err := addTool[prohibitedNestedInput, ToolResult](registry, "nested_bad", "bad", nil, ExposureLLM, EffectReadOnly, false, readMetadata); err == nil {
		t.Fatal("nested prohibited field should fail")
	}
	// Effect 与 PolicyGate 同为注册期强约束：缺省或非法枚举必须在注册期报错。
	if err := addTool[cleanInput, ToolResult](registry, "no_effect", "missing effect", nil, ExposureLLM, "", false, readMetadata); err == nil {
		t.Fatal("missing Effect should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "bogus_effect", "bad effect", nil, ExposureLLM, Effect("weird"), false, readMetadata); err == nil {
		t.Fatal("invalid Effect should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "no_family", "missing family", nil, ExposureLLM, EffectReadOnly, false,
		terminalMetadata("", CostLow, SurfaceDiscovery)); err == nil {
		t.Fatal("missing Family should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "no_cost", "missing cost", nil, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, "", SurfaceDiscovery)); err == nil {
		t.Fatal("missing Cost should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "no_surface", "missing surface", nil, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostLow)); err == nil {
		t.Fatal("missing LLM Surface should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "unknown_surface", "unknown surface", nil, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostLow, Surface(1<<20))); err == nil {
		t.Fatal("unknown LLM Surface should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "compound_primary", "compound primary", nil, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostLow, Surfaces(SurfaceDiscovery, SurfaceTalkingHead))); err == nil {
		t.Fatal("compound PrimarySurface should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "bad_family_effect", "bad classification", nil, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyRead, CostLow, SurfaceDiscovery)); err == nil {
		t.Fatal("inconsistent Family and Effect should fail registration")
	}
	if err := addTool[cleanInput, ToolResult](registry, "no_completion", "missing completion", nil, ExposureLLM, EffectReadOnly, false,
		modelMetadata(FamilyRead, CostLow, "", SurfaceDiscovery)); err == nil {
		t.Fatal("LLM 工具缺少 CompletionSemantics 应注册失败")
	}
	if err := addTool[cleanInput, ToolResult](registry, "bad_completion", "bad completion", nil, ExposureLLM, EffectReadOnly, false,
		modelMetadata(FamilyRead, CostLow, CompletionSemantics("queued_allowed"), SurfaceDiscovery)); err == nil {
		t.Fatal("LLM 工具声明非法 CompletionSemantics 应注册失败")
	}
	if err := addTool[cleanInput, ToolResult](registry, "read.waiting", "unexpected waiting", nil, ExposureLLM, EffectReadOnly, false,
		waitingUserMetadata(FamilyRead, CostLow, SurfaceDiscovery)); err == nil {
		t.Fatal("非交互模型工具不得允许 waiting_user")
	}
	if err := addTool[cleanInput, ToolResult](registry, "harness_with_completion", "unexpected model policy", nil, ExposureHarness, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostLow)); err == nil {
		t.Fatal("harness 工具不得声明 CompletionSemantics")
	}
	if _, exists := registry.specs["no_effect"]; exists {
		t.Fatal("未标注 Effect 的工具不得进入注册表")
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
	if _, err := convertResult[AssetListResult](map[string]any{"error": "adapter boom"}); err == nil {
		t.Fatal("typed result 不得忽略未知 error 字段并解成零值成功")
	}
	if _, err := convertResult[ToolResult](map[string]any{"status": "succeeded", "unexpected": true}); err == nil {
		t.Fatal("ToolResult 不得忽略未知字段")
	}
	if _, err := convertResult[AssetListResult](nil); err == nil {
		t.Fatal("工具结果不得把 null 解成零值成功")
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
