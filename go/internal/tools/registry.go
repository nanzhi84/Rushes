package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type Exposure string

const (
	ExposureLLM     Exposure = "llm"
	ExposureHarness Exposure = "harness_only"
)

type Spec struct {
	Name           string
	Description    string
	Requires       []string
	Exposure       Exposure
	Optional       bool
	InputType      reflect.Type
	Implementation tool.BaseTool
}

type Registry struct {
	database *storage.DB
	executor Executor
	specs    map[string]Spec
}

func NewRegistry(database *storage.DB, executor Executor) (*Registry, error) {
	if database == nil || executor == nil {
		return nil, errors.New("tool registry 缺少 database 或 executor")
	}
	registry := &Registry{database: database, executor: executor, specs: map[string]Spec{}}
	builders := []func(*Registry) error{
		registerAssetImport, registerAssetList, registerUnderstand, registerShotSearch, registerAudioBeatAnalysis,
		registerSpeechPauseAnalysis, registerSpeechInspect, registerAskUser,
		registerDecisionAnswer, registerComposeInitial, registerApplyPatch, registerApplyPatchBatch,
		registerBeatRecut, registerTalkingHeadEdit,
		registerTimelineValidate, registerTimelineInspect, registerRenderPreview,
		registerRenderFinal, registerRenderStatus, registerInspectPreview,
		registerConfirmAction,
	}
	for _, builder := range builders {
		if err := builder(registry); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (registry *Registry) Specs(includeOptional bool) []Spec {
	result := make([]Spec, 0, len(registry.specs))
	for _, spec := range registry.specs {
		if spec.Optional && !includeOptional {
			continue
		}
		result = append(result, spec)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (registry *Registry) EinoTools(includeOptional, includeHarness bool) []tool.BaseTool {
	result := []tool.BaseTool{}
	for _, spec := range registry.Specs(includeOptional) {
		if spec.Exposure == ExposureHarness && !includeHarness {
			continue
		}
		result = append(result, spec.Implementation)
	}
	return result
}

func (registry *Registry) Allowed(ctx context.Context, includeOptional bool) ([]Spec, error) {
	result := []Spec{}
	for _, spec := range registry.Specs(includeOptional) {
		if spec.Exposure != ExposureLLM {
			continue
		}
		if err := registry.guard(ctx, spec); err == nil {
			result = append(result, spec)
		}
	}
	return result, nil
}

func addTool[I, O any](
	registry *Registry,
	name, description string,
	requires []string,
	exposure Exposure,
	optional bool,
) error {
	if _, exists := registry.specs[name]; exists {
		return fmt.Errorf("工具重复注册: %s", name)
	}
	inputType := reflect.TypeFor[I]()
	if exposure == ExposureLLM {
		if key := prohibitedField(inputType); key != "" {
			return fmt.Errorf("工具 %s 的字段被 PolicyGate 禁止: %s", name, key)
		}
	}
	implementation, err := utils.InferTool(name, description, func(ctx context.Context, input I) (O, error) {
		spec := registry.specs[name]
		if err := registry.guard(ctx, spec); err != nil {
			var zero O
			return zero, err
		}
		if reporter, ok := ctx.Value(reporterKey).(Reporter); ok && reporter != nil {
			reporter(name, "started", input, nil, nil)
		}
		raw, executeErr := registry.executor.ExecuteTool(ctx, name, input)
		output, convertErr := convertResult[O](raw)
		if executeErr == nil {
			executeErr = convertErr
		}
		if reporter, ok := ctx.Value(reporterKey).(Reporter); ok && reporter != nil {
			reporter(name, "finished", input, output, executeErr)
		}
		return output, executeErr
	})
	if err != nil {
		return err
	}
	registry.specs[name] = Spec{
		Name: name, Description: description, Requires: append([]string(nil), requires...),
		Exposure: exposure, Optional: optional,
		InputType: inputType, Implementation: implementation,
	}
	return nil
}

func (registry *Registry) guard(ctx context.Context, spec Spec) error {
	draftID, err := DraftID(ctx)
	if err != nil {
		return err
	}
	for _, predicate := range spec.Requires {
		passed, evaluateErr := EvaluatePrecondition(ctx, registry.database, draftID, predicate)
		if evaluateErr != nil {
			return evaluateErr
		}
		if !passed {
			return fmt.Errorf("工具 %s 未满足前置条件 %s", spec.Name, predicate)
		}
	}
	return nil
}

func convertResult[O any](raw any) (O, error) {
	if typed, ok := raw.(O); ok {
		return typed, nil
	}
	var result O
	data, err := json.Marshal(raw)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	return result, nil
}

var prohibitedParts = []string{"timecode", "ffmpeg", "filter_complex", "codec", "bitrate", "crf", "preset", "pix_fmt"}
var prohibitedNames = map[string]struct{}{
	"path": {}, "file": {}, "file_path": {}, "source_path": {}, "reference_path": {},
	"workspace_object_uri": {}, "local_path": {}, "argv": {}, "vf": {}, "af": {},
	"timeline_version": {}, "timeline_revision": {},
}

func prohibitedField(input reflect.Type) string {
	if input.Kind() == reflect.Pointer {
		input = input.Elem()
	}
	if input.Kind() != reflect.Struct {
		return ""
	}
	for index := range input.NumField() {
		field := input.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = field.Name
		}
		if _, prohibited := prohibitedNames[name]; prohibited {
			return name
		}
		for _, part := range prohibitedParts {
			if strings.Contains(name, part) {
				return name
			}
		}
	}
	return ""
}

func registerAssetImport(registry *Registry) error {
	return addTool[AssetImportInput, ToolResult](registry, "asset.import_local_file", "导入用户已确认的本地素材", nil, ExposureHarness, false)
}

func registerAssetList(registry *Registry) error {
	return addTool[AssetListInput, AssetListResult](registry, "asset.list_assets", "列出当前草稿可用素材", nil, ExposureLLM, false)
}

func registerUnderstand(registry *Registry) error {
	return addTool[UnderstandInput, UnderstandResult](registry, "understand.materials", "幂等理解所选素材并生成可检索的逐镜头时间证据；相同素材和参数默认直接复用持久化结果，只有用户明确要求重新分析时才设置 force_refresh=true", nil, ExposureLLM, false)
}

func registerShotSearch(registry *Registry) error {
	return addTool[ShotSearchInput, ShotSearchResult](
		registry,
		"media.search_shots",
		"像检索代码一样按创作意图搜索已理解视频中的镜头级源区间；返回稳定 shot_id、精确源帧、语义、匹配证据和剪辑提示。若更匹配的文件尚未理解，understanding_candidates 会返回文件名与 asset_id；先对候选调用 understand.materials，再用同一意图重搜，禁止把候选文件臆造为 shot_id",
		[]string{"usable_asset_exists"}, ExposureLLM, false,
	)
}

func registerAudioBeatAnalysis(registry *Registry) error {
	return addTool[AudioBeatAnalysisInput, AudioBeatAnalysisResult](
		registry,
		"audio.analyze_beats",
		"读取音频的 BPM、普通拍点、强瞬态、推断小节第一拍和按时间顺序压缩的 RMS 波形。拍点坐标使用整数帧；波形使用固定 0-100 编码并返回采样间隔，不标注高潮、低潮或剪辑好坏",
		[]string{"usable_asset_exists"}, ExposureLLM, false,
	)
}

func registerSpeechPauseAnalysis(registry *Registry) error {
	return addTool[SpeechPauseAnalysisInput, SpeechPauseAnalysisResult](
		registry,
		"audio.analyze_speech_pauses",
		"分析音频或视频内音轨的停顿/气口，返回源素材整数帧；传 timeline_clip_id 时同时映射为当前时间线帧，可用于剪口播。结果是 RMS 静音候选，不会把语义停顿或口头禅误报成已确认删除项",
		[]string{"usable_asset_exists"}, ExposureLLM, false,
	)
}

func registerSpeechInspect(registry *Registry) error {
	return addTool[SpeechInspectInput, SpeechInspectResult](
		registry,
		"speech.inspect",
		"建立或复用带整数帧坐标的口播索引，并像 grep 一样按台词语义或源帧范围读取逐句 ASR、气口和相似台词证据。要检查句内卡壳、重复词或半句重说时设置 include_words=true，取得稳定 word_id 与词级帧。工具只提供可核验信息，不决定哪些内容应删除；完整转写持久化在本地，后续调用默认命中缓存",
		[]string{"usable_asset_exists"}, ExposureLLM, false,
	)
}

func registerAskUser(registry *Registry) error {
	return addTool[AskUserInput, ToolResult](registry, "interaction.ask_user", "通过结构化决策卡向用户提问", nil, ExposureLLM, false)
}

func registerDecisionAnswer(registry *Registry) error {
	return addTool[DecisionAnswerInput, ToolResult](registry, "decision.answer", "提交结构化决策答案", nil, ExposureLLM, false)
}

func registerComposeInitial(registry *Registry) error {
	return addTool[ComposeInitialInput, ToolResult](registry, "timeline.compose_initial", "按整数帧源区间组装时间线；只传入 video/image 主视觉素材，不能传 audio/font；先从 asset.list_assets 读取 kind、duration_frames 与 timeline_fps", []string{"usable_asset_exists"}, ExposureLLM, false)
}

func registerApplyPatch(registry *Registry) error {
	return addTool[TimelinePatchInput, ToolResult](registry, "timeline.apply_patch", "对当前时间线应用一个语义补丁；支持 trim_clip、split_clip、reorder_clip、move_clip、trim_clip_edge、delete_clip、set_track_state、set_clip_linked、insert_subtitle、delete_range、insert_clip、replace_clip、set_playback_rate、adjust_gain、set_clip_fades、edit_subtitle_text、remove_track_clips、sync_original_audio；带原声视频插入主视觉时会自动建立音画联动；原声缺失或错位时用 sync_original_audio 从最新主视觉原子重建；set_clip_fades 使用 fade_in_frames/fade_out_frames；move_clip/reorder_clip 的位置字段必须是 target_frame；trim_clip 主视觉会自动 ripple 后续片段，不要再逐段 move；先用 timeline.inspect 获取真实 ID；所有位置只接受整数帧", []string{"timeline_exists"}, ExposureLLM, false)
}

func registerApplyPatchBatch(registry *Registry) error {
	return addTool[TimelinePatchBatchInput, ToolResult](
		registry,
		"timeline.apply_patches",
		"原子应用多个时间线语义补丁，整批只写入一次当前时间线；整批替换主视频时把新 insert_clip 和旧 delete_clip 放在同一次调用，工具会规划安全执行顺序，并默认保护未被本批直接编辑的 BGM/SFX；卡点剪辑必须改用 timeline.recut_to_beats",
		[]string{"timeline_exists"}, ExposureLLM, false,
	)
}

func registerBeatRecut(registry *Registry) error {
	return addTool[TimelineBeatRecutInput, ToolResult](
		registry,
		"timeline.recut_to_beats",
		"从空时间线或已有时间线原子完成卡点混剪：传 bgm_asset_id 后按真实拍点重建主视频；优先传 media.search_shots 返回的有序 shot_ids，工具会解析精确源帧。cut_frames 可多于视频素材数，同一素材会使用不同且不重叠的源区间；use_all_video_assets=true 表示每个素材至少一次；cover_entire_bgm=true 覆盖整首音乐；SFX 始终独立分轨。禁止用 compose_initial 加几十个低层补丁替代",
		[]string{"usable_asset_exists"}, ExposureLLM, false,
	)
}

func registerTalkingHeadEdit(registry *Registry) error {
	return addTool[TalkingHeadEditInput, ToolResult](
		registry,
		"timeline.edit_talking_head",
		"按模型已经选定的 utterance_id、pause/repetition/fragment 决定、连续 word_id 范围和 b_roll shot_id 原子剪辑口播：模型结合两侧原文自主选择 remove/preserve；工具只校验稳定 ID 与合法范围、波纹删除整句/句内卡壳/气口，并把 B-roll 放到独立叠加轨覆盖对应语义。未处理的内容候选作为非阻塞证据随成功结果返回，局部修正无需为目标外候选补齐决定。B-roll 可用 utterance 范围；短镜头可在该范围附带唯一连续原文 anchor_text，由工具确定性解析词级帧，或直接使用 start/end_word_id。工具会合并删除项间不含保留台词的极短空白，避免生成数帧孤片，不替模型判断内容好坏",
		[]string{"timeline_exists"}, ExposureLLM, false,
	)
}

func registerTimelineValidate(registry *Registry) error {
	return addTool[TimelineValidateInput, ToolResult](registry, "timeline.validate", "验证当前时间线不变量", []string{"timeline_exists"}, ExposureLLM, false)
}

func registerTimelineInspect(registry *Registry) error {
	return addTool[TimelineInspectInput, ToolResult](registry, "timeline.inspect", "读取可编辑的时间线摘要与完整 track/clip ID、素材、角色和帧范围；尚无时间线时返回 timeline_exists=false，而不是失败", nil, ExposureLLM, false)
}

func registerRenderPreview(registry *Registry) error {
	return addTool[RenderPreviewInput, ToolResult](registry, "render.preview", "排队渲染当前已验证时间线预览", []string{"timeline_validated"}, ExposureLLM, false)
}

func registerRenderFinal(registry *Registry) error {
	return addTool[RenderFinalInput, ToolResult](registry, "render.final_mp4", "排队导出最终 MP4", []string{"timeline_validated"}, ExposureLLM, false)
}

func registerRenderStatus(registry *Registry) error {
	return addTool[RenderStatusInput, ToolResult](registry, "render.status", "读取渲染任务与产物状态", []string{"timeline_exists"}, ExposureLLM, false)
}

func registerInspectPreview(registry *Registry) error {
	return addTool[RenderInspectInput, PreviewInspectionResult](registry, "render.inspect_preview", "检查预览的流、解码、黑帧、静帧、静音和响度", []string{"any_preview_exists"}, ExposureLLM, true)
}

func registerConfirmAction(registry *Registry) error {
	return addTool[ConfirmActionInput, ToolResult](registry, "interaction.confirm_action", "为破坏性动作创建确认决策", nil, ExposureLLM, true)
}
