package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

type Exposure string

const (
	ExposureLLM     Exposure = "llm"
	ExposureHarness Exposure = "harness_only"
)

// Effect 是工具副作用风险的显式分级，注册期必填（缺省与 PolicyGate 同为注册期
// 强约束）。它是「只读并发调度 / 破坏性强制确认 / 瞬时失败可重试」等治理策略的
// 单一事实源（#103 G1），替代此前散落在硬编码白名单与工具描述里的隐式副本。
// Effect 只用于 harness 治理，绝不进入模型可见的工具 schema。
type Effect string

const (
	// EffectReadOnly 纯读：不写任何持久状态，可安全重试、可并发调度、无需确认。
	EffectReadOnly Effect = "read_only"
	// EffectReversible 有写入，但可经 Rewind 或经稳定键的幂等重放恢复。
	EffectReversible Effect = "reversible"
	// EffectDestructive 不可逆，或影响 agent 之外的持久状态，须先经确认。
	EffectDestructive Effect = "destructive"
)

// Valid 报告 Effect 是否为三个合法枚举之一；空值一律视为未标注。
func (effect Effect) Valid() bool {
	switch effect {
	case EffectReadOnly, EffectReversible, EffectDestructive:
		return true
	default:
		return false
	}
}

type Spec struct {
	Name           string
	Description    string
	Requires       []string
	Exposure       Exposure
	Effect         Effect
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
		registerDecisionAnswer, registerPlanUpdate, registerMemoryUpdate,
		registerComposeInitial, registerApplyPatchBatch,
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

func (registry *Registry) DecodeInput(name string, arguments map[string]any) (any, error) {
	spec, exists := registry.specs[name]
	if !exists {
		return nil, fmt.Errorf("工具未注册: %s", name)
	}
	if spec.InputType == nil {
		return nil, fmt.Errorf("工具 %s 缺少输入类型", name)
	}
	if arguments == nil {
		return nil, fmt.Errorf("解码工具 %s 参数: arguments 必须是 JSON 对象", name)
	}
	data, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("编码工具 %s 参数: %w", name, err)
	}
	target := reflect.New(spec.InputType)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target.Interface()); err != nil {
		return nil, fmt.Errorf("解码工具 %s 参数: %w", name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return nil, fmt.Errorf("解码工具 %s 参数: %w", name, err)
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解码工具 %s 参数: %w", name, err)
	}
	if err := validateRequiredFields(spec.InputType, raw, "arguments"); err != nil {
		return nil, fmt.Errorf("解码工具 %s 参数: %w", name, err)
	}
	return target.Elem().Interface(), nil
}

func validateRequiredFields(input reflect.Type, value any, path string) error {
	for input.Kind() == reflect.Pointer {
		input = input.Elem()
	}
	if value == nil {
		return fmt.Errorf("%s 不允许为 null", path)
	}
	if input == reflect.TypeFor[TimelineOp]() {
		operation, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s 必须是 JSON 对象", path)
		}
		if err := validateTimelineOp(operation); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
	switch input.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for index := range input.NumField() {
			field := input.Field(index)
			if field.PkgPath != "" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			fieldValue, exists := object[name]
			if schemaTagContains(field.Tag.Get("jsonschema"), "required") && (!exists || fieldValue == nil) {
				return fmt.Errorf("缺少必填字段 %s.%s", path, name)
			}
			if exists {
				if err := validateRequiredFields(field.Type, fieldValue, path+"."+name); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for index, item := range items {
			if err := validateRequiredFields(input.Elem(), item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTimelineOp(operation map[string]any) error {
	if err := timeline.ValidateOpFields(operation); err != nil {
		return err
	}
	kind := operation["kind"].(string)
	spec, _ := timeline.LookupOpSpec(kind)
	allowed := map[string]bool{"kind": true}
	for _, field := range spec.Fields {
		if field.Injected {
			continue
		}
		allowed[field.Name] = true
		for _, alias := range field.Aliases {
			allowed[alias] = true
		}
	}
	for name := range operation {
		if !allowed[name] {
			return fmt.Errorf("时间线补丁 %s 包含未声明字段 %s", kind, name)
		}
	}
	return nil
}

func schemaTagContains(tag, option string) bool {
	for part := range strings.SplitSeq(tag, ",") {
		if strings.TrimSpace(part) == option {
			return true
		}
	}
	return false
}

func (registry *Registry) ValidateConfirmation(ctx context.Context, name string, arguments map[string]any) error {
	spec, exists := registry.specs[name]
	if !exists {
		return fmt.Errorf("目标工具未注册: %s", name)
	}
	if spec.Exposure != ExposureLLM {
		return fmt.Errorf("目标工具不可由模型确认后执行: %s", name)
	}
	if strings.HasPrefix(name, "interaction.") || name == "decision.answer" {
		return fmt.Errorf("交互类工具不能嵌套确认: %s", name)
	}
	if _, err := registry.DecodeInput(name, arguments); err != nil {
		return fmt.Errorf("目标工具参数无效: %w", err)
	}
	if err := registry.guard(ctx, spec); err != nil {
		return fmt.Errorf("目标工具前置条件不满足: %w", err)
	}
	return nil
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
	effect Effect,
	optional bool,
) error {
	if _, exists := registry.specs[name]; exists {
		return fmt.Errorf("工具重复注册: %s", name)
	}
	if !effect.Valid() {
		return fmt.Errorf("工具 %s 缺少合法 Effect 风险分级: %q", name, effect)
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
			reporter(ctx, name, "started", input, nil, nil)
		}
		raw, executeErr := registry.executor.ExecuteTool(ctx, name, input)
		output, convertErr := convertResult[O](raw)
		if executeErr == nil {
			executeErr = convertErr
		}
		if reporter, ok := ctx.Value(reporterKey).(Reporter); ok && reporter != nil {
			reporter(ctx, name, "finished", input, output, executeErr)
		}
		return output, executeErr
	}, utils.WithUnmarshalArguments(strictUnmarshalToolArguments[I]))
	if err != nil {
		return err
	}
	registry.specs[name] = Spec{
		Name: name, Description: description, Requires: append([]string(nil), requires...),
		Exposure: exposure, Effect: effect, Optional: optional,
		InputType: inputType, Implementation: implementation,
	}
	return nil
}

// Effect 返回指定工具的副作用风险分级；未注册工具返回 ("", false)。消费方
// （瞬时失败重试、G2 破坏性确认、G3 只读并发分组）都从这里派生，不再各自维护镜像。
func (registry *Registry) Effect(name string) (Effect, bool) {
	spec, exists := registry.specs[name]
	if !exists {
		return "", false
	}
	return spec.Effect, true
}

func strictUnmarshalToolArguments[I any](_ context.Context, arguments string) (any, error) {
	var input I
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return nil, err
	}
	return input, nil
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

const prohibitedFieldMaxDepth = 4

func prohibitedField(input reflect.Type) string {
	return prohibitedFieldAtDepth(input, 0, map[reflect.Type]struct{}{})
}

func prohibitedFieldAtDepth(input reflect.Type, depth int, active map[reflect.Type]struct{}) string {
	switch input.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		if _, recursive := active[input]; recursive {
			return ""
		}
		active[input] = struct{}{}
		result := prohibitedFieldAtDepth(input.Elem(), depth, active)
		delete(active, input)
		return result
	}
	if input.Kind() != reflect.Struct {
		return ""
	}
	if _, recursive := active[input]; recursive {
		return ""
	}
	active[input] = struct{}{}
	defer delete(active, input)
	for index := range input.NumField() {
		field := input.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
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
		if depth < prohibitedFieldMaxDepth {
			if nested := prohibitedFieldAtDepth(field.Type, depth+1, active); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func registerAssetImport(registry *Registry) error {
	// 仅 harness 调用；写入素材行并触发导入，可通过移除素材回滚，故归可逆。
	return addTool[AssetImportInput, ToolResult](registry, "asset.import_local_file", "导入用户已确认的本地素材", nil, ExposureHarness, EffectReversible, false)
}

func registerAssetList(registry *Registry) error {
	return addTool[AssetListInput, AssetListResult](registry, "asset.list_assets", "列出当前草稿可用素材", nil, ExposureLLM, EffectReadOnly, false)
}

func registerUnderstand(registry *Registry) error {
	return addTool[UnderstandInput, UnderstandResult](registry, "understand.materials", "幂等理解所选素材并生成可检索的逐镜头时间证据；相同素材和参数默认直接复用持久化结果，只有用户明确要求重新分析时才设置 force_refresh=true；旧强制任务终态后再次重跑完全相同分析才更换 refresh_nonce；多素材、deep 或 force_refresh 可能返回 queued，后台完成后会自动续跑当前任务，不要轮询", nil, ExposureLLM, EffectReversible, false)
}

func registerShotSearch(registry *Registry) error {
	return addTool[ShotSearchInput, ShotSearchResult](
		registry,
		"media.search_shots",
		"像检索代码一样按创作意图搜索已理解视频中的镜头级源区间；返回稳定 shot_id、精确源帧、语义、匹配证据和剪辑提示。若更匹配的文件尚未理解，understanding_candidates 会返回文件名与 asset_id；先对候选调用 understand.materials，再用同一意图重搜，禁止把候选文件臆造为 shot_id",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
	)
}

func registerAudioBeatAnalysis(registry *Registry) error {
	return addTool[AudioBeatAnalysisInput, AudioBeatAnalysisResult](
		registry,
		"audio.analyze_beats",
		"读取音频的 BPM、普通拍点、强瞬态、推断小节第一拍和按时间顺序压缩的 RMS 波形。拍点坐标使用整数帧；波形使用固定 0-100 编码并返回采样间隔，不标注高潮、低潮或剪辑好坏",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
	)
}

func registerSpeechPauseAnalysis(registry *Registry) error {
	return addTool[SpeechPauseAnalysisInput, SpeechPauseAnalysisResult](
		registry,
		"audio.analyze_speech_pauses",
		"分析音频或视频内音轨的停顿/气口，返回源素材整数帧；传 timeline_clip_id 时同时映射为当前时间线帧，可用于剪口播。结果是 RMS 静音候选，不会把语义停顿或口头禅误报成已确认删除项",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
	)
}

func registerSpeechInspect(registry *Registry) error {
	// 偏离 issue 的 ReadOnly：首次调用经 reducer 落盘 transcript（speech_inspect.go 的
	// loadOrBuildSpeechTranscript），命中缓存才是纯读。稳定指纹 ID 让顺序重放幂等（故仍属
	// 重试安全，见 agent 侧 retrySafeFromEffect），但两个并发首调会重复 ASR 且 providerID
	// 可能分叉出不同 transcript 行——对 G3 只读并发不安全，故按 G3a spike 归为副作用。
	return addTool[SpeechInspectInput, SpeechInspectResult](
		registry,
		"speech.inspect",
		"建立或复用带整数帧坐标的口播索引，并像 grep 一样按台词语义或源帧范围读取逐句 ASR、气口和相似台词证据。要检查句内卡壳、重复词或半句重说时设置 include_words=true，取得稳定 word_id 与词级帧。工具只提供可核验信息，不决定哪些内容应删除；完整转写持久化在本地，后续调用默认命中缓存",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
	)
}

func registerAskUser(registry *Registry) error {
	return addTool[AskUserInput, ToolResult](registry, "interaction.ask_user", "仅在缺少会实质改变成片目标、且无法从素材或上下文安全推断的关键决策时，通过简短结构化决策卡向用户提问；已有可用素材时，成片类型、时长、风格和节奏等可逆首剪细节必须结合 user_memory 与安全默认值自主决定，不得用此工具追问", nil, ExposureLLM, EffectReversible, false)
}

func registerDecisionAnswer(registry *Registry) error {
	return addTool[DecisionAnswerInput, ToolResult](registry, "decision.answer", "提交结构化决策答案", nil, ExposureLLM, EffectReversible, false)
}

func registerPlanUpdate(registry *Registry) error {
	return addTool[PlanUpdateInput, ToolResult](
		registry,
		"plan.update",
		"以 RFC 7396 语义增量合并 plan；reset=true 时先清空旧计划再应用该对象，用于在跨回合继续工作前保存已确定的计划结构；素材可用但请求宽泛时，用此工具记录基于长期画像作出的首剪默认决定并继续执行，不要转去追问可回滚细节",
		nil, ExposureLLM, EffectReversible, false,
	)
}

func registerMemoryUpdate(registry *Registry) error {
	// remove_keys 删除用户长期记忆是不可逆、且影响 agent 之外的持久画像，故归破坏性。
	// 注意：Effect 是工具级信号，对 memory.update 必要不充分——纯新增/更新路径可逆，
	// G2 验收明确其不受强制确认影响。因此 G2 拦截器不能只按 spec.Effect 拦截，必须再检查
	// 本次 input 是否携带 remove_keys，据此豁免纯新增/更新路径、只拦真正的删除。
	return addTool[MemoryUpdateInput, ToolResult](
		registry,
		"memory.update",
		"仅当当前用户明确表达跨项目稳定的偏好、习惯、纠正，或明确要求忘记已有长期记忆时更新用户画像；一次性草稿要求和模型自己的创作判断不得写入",
		nil, ExposureLLM, EffectDestructive, false,
	)
}

func registerComposeInitial(registry *Registry) error {
	return addTool[ComposeInitialInput, ToolResult](registry, "timeline.compose_initial", "按整数帧源区间组装时间线；只传入 video/image 主视觉素材，不能传 audio/font；先从 asset.list_assets 读取 kind、duration_frames 与 timeline_fps", []string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false)
}

func registerApplyPatchBatch(registry *Registry) error {
	return addTool[TimelinePatchBatchInput, ToolResult](
		registry,
		"timeline.apply_patches",
		"原子应用多个时间线语义补丁，整批只写入一次当前时间线；整批替换主视频时把新 insert_clip 和旧 delete_clip 放在同一次调用，工具会规划安全执行顺序，并默认保护未被本批直接编辑的 BGM/SFX；卡点剪辑必须改用 timeline.recut_to_beats",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
	)
}

func registerBeatRecut(registry *Registry) error {
	return addTool[TimelineBeatRecutInput, ToolResult](
		registry,
		"timeline.recut_to_beats",
		"从空时间线或已有时间线原子完成卡点混剪：传 bgm_asset_id 后按真实拍点重建主视频；优先传 media.search_shots 返回的有序 shot_ids，工具会解析精确源帧。cut_frames 可多于视频素材数，同一素材会使用不同且不重叠的源区间；use_all_video_assets=true 表示每个素材至少一次；cover_entire_bgm=true 覆盖整首音乐；SFX 始终独立分轨。禁止用 compose_initial 加几十个低层补丁替代",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
	)
}

func registerTalkingHeadEdit(registry *Registry) error {
	return addTool[TalkingHeadEditInput, ToolResult](
		registry,
		"timeline.edit_talking_head",
		"按模型已经选定的 utterance_id、pause/repetition/fragment 决定、连续 word_id 范围和 b_roll shot_id 原子剪辑口播。模型结合两侧原文自主选择 remove/preserve；工具只校验稳定 ID 与合法范围、波纹删除整句/句内卡壳/气口，并把 B-roll 放到独立叠加轨。B-roll 的 utterance、anchor_text 或 word_id 必须以本次全部删除决定展开后的保留台词为准：anchor_text 要从 speech.inspect 原文逐字复制，不能包含同次将删除的卡壳、重复词或短片段。短镜头可用保留 utterance 内的唯一连续 anchor_text，或直接使用保留的 start/end_word_id。若删气口会把保留台词夹成不足 2 秒的孤立碎片，工具会保守撤回最短的相邻气口删除并在 auto_preserved_pause_ids 中报告；语义删除造成的孤片或落在口误证据上的保留岛会失败，并在 island_counter_proposals 里给出可直接采纳的合并删除区间。未处理的内容候选作为非阻塞证据随成功结果返回，工具不替模型判断内容好坏",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
	)
}

func registerTimelineValidate(registry *Registry) error {
	// 归可逆而非只读：写 TimelineValidated/TimelineValidationFailed 校验事件（reducer.Apply）。
	// 校验事件按 merge key 幂等、顺序重放安全，故仍属重试安全，由 retrySafeFromEffect 单独放行。
	return addTool[TimelineValidateInput, ToolResult](registry, "timeline.validate", "验证当前时间线不变量", []string{"timeline_exists"}, ExposureLLM, EffectReversible, false)
}

func registerTimelineInspect(registry *Registry) error {
	return addTool[TimelineInspectInput, ToolResult](registry, "timeline.inspect", "读取可编辑的时间线摘要与完整 track/clip ID、素材、角色和帧范围；尚无时间线时返回 timeline_exists=false，而不是失败", nil, ExposureLLM, EffectReadOnly, false)
}

func registerRenderPreview(registry *Registry) error {
	return addTool[RenderPreviewInput, ToolResult](registry, "render.preview", "排队渲染当前已验证时间线预览", []string{"timeline_validated"}, ExposureLLM, EffectReversible, false)
}

func registerRenderFinal(registry *Registry) error {
	return addTool[RenderFinalInput, ToolResult](registry, "render.final_mp4", "排队导出最终 MP4", []string{"timeline_validated"}, ExposureLLM, EffectReversible, false)
}

func registerRenderStatus(registry *Registry) error {
	return addTool[RenderStatusInput, ToolResult](registry, "render.status", "读取渲染任务与产物状态", []string{"timeline_exists"}, ExposureLLM, EffectReadOnly, false)
}

func registerInspectPreview(registry *Registry) error {
	return addTool[RenderInspectInput, PreviewInspectionResult](registry, "render.inspect_preview", "检查预览的流、解码、黑帧、静帧、静音和响度；传 visual 可追加切点、B-roll 与字幕 contact sheet 视觉检查", []string{"any_preview_exists"}, ExposureLLM, EffectReadOnly, true)
}

func registerConfirmAction(registry *Registry) error {
	// 创建确认决策是一次可逆写入（决策行）；G2 的强制确认拦截器读 EffectDestructive，
	// confirm_action 本身不是被拦截对象，故按其写行为归可逆。
	return addTool[ConfirmActionInput, ToolResult](registry, "interaction.confirm_action", "为破坏性动作创建确认决策", nil, ExposureLLM, EffectReversible, true)
}
