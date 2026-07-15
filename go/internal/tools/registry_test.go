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
		"timeline.apply_patch": {
			"move_clip/reorder_clip", "target_frame", "timeline.inspect", "整数帧",
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
	if len(core) != 19 {
		t.Fatalf("core tools=%d", len(core))
	}
	if len(registry.Specs(true)) != 21 {
		t.Fatalf("all tools=%d", len(registry.Specs(true)))
	}
	for _, spec := range registry.Specs(true) {
		info, infoErr := spec.Implementation.Info(t.Context())
		if infoErr != nil || info.Name != spec.Name || info.Desc == "" {
			t.Fatalf("spec=%s info=%#v err=%v", spec.Name, info, infoErr)
		}
	}
	if got := len(registry.EinoTools(false, false)); got != 18 {
		t.Fatalf("LLM core tools=%d", got)
	}
	if got := len(registry.EinoTools(false, true)); got != 19 {
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
	for _, name := range []string{"timeline.apply_patch", "timeline.apply_patches", "timeline.recut_to_beats", "timeline.validate", "timeline.inspect", "render.preview", "render.final_mp4", "render.status"} {
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
		prohibitedField(reflect.TypeFor[*prohibitedFrameInput]()) != "" ||
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

	tool := registry.specs["clean"].Implementation.(einotool.InvokableTool)
	if _, err := tool.InvokableRun(t.Context(), `{}`); err == nil {
		t.Fatal("missing draft context should fail")
	}
	reports := []string{}
	ctx := WithReporter(WithDraftID(t.Context(), "draft"), func(name, phase string, _, _ any, err error) {
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
			timeline_validated,scratch_memory_json,created_at,updated_at
		) VALUES(?,?,0,'active','{}','[]','{"goal":""}',0,'{}',?,?)`, draftID, draftID, now, now)
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
