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

// Family 描述模型看到的能力原语：检测生成一种证据，读取/检索不写状态，编辑提交
// 一个可回滚写入，检查只返回报告，控制面维护 Harness 状态。它与 Effect 正交：
// detect 允许持久化证据，而 read 必须严格只读。
type Family string

const (
	FamilyDetect  Family = "detect"
	FamilyRead    Family = "read"
	FamilyEdit    Family = "edit"
	FamilyCheck   Family = "check"
	FamilyControl Family = "control"
)

func (family Family) Valid() bool {
	switch family {
	case FamilyDetect, FamilyRead, FamilyEdit, FamilyCheck, FamilyControl:
		return true
	default:
		return false
	}
}

// Cost 是工具单次调用的相对成本，只用于动态披露和可观测性，不进入模型 schema。
type Cost string

const (
	CostLow      Cost = "low"
	CostStandard Cost = "standard"
	CostHigh     Cost = "high"
)

func (cost Cost) Valid() bool {
	switch cost {
	case CostLow, CostStandard, CostHigh:
		return true
	default:
		return false
	}
}

// Surface 是模型工具面的阶段标签。一个工具可属于多个阶段，但阶段清单只保存在
// Registry Spec 中；agent 只选择阶段，不维护第二份工具目录。
type Surface uint32

const (
	SurfaceDiscovery Surface = 1 << iota
	SurfaceTalkingHead
	SurfaceBeatEdit
	SurfaceTimelineEdit
	SurfaceRender
	SurfacePreviewCheck
	SurfaceControl
)

const allSurfaces = SurfaceDiscovery |
	SurfaceTalkingHead |
	SurfaceBeatEdit |
	SurfaceTimelineEdit |
	SurfaceRender |
	SurfacePreviewCheck |
	SurfaceControl

func Surfaces(values ...Surface) Surface {
	var result Surface
	for _, value := range values {
		result |= value
	}
	return result
}

func (surface Surface) Includes(value Surface) bool { return surface&value != 0 }
func (surface Surface) Valid() bool                 { return surface != 0 && surface&^allSurfaces == 0 }
func (surface Surface) Single() bool                { return surface.Valid() && surface&(surface-1) == 0 }

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
	Family         Family
	Cost           Cost
	PrimarySurface Surface
	Surfaces       Surface
	Effect         Effect
	Optional       bool
	InputType      reflect.Type
	Implementation tool.BaseTool
}

// Parallelizable 从 Effect 单一事实源派生；不额外维护会漂移的并发布尔字段。
func (spec Spec) Parallelizable() bool { return spec.Effect == EffectReadOnly }

type specMetadata struct {
	family   Family
	cost     Cost
	primary  Surface
	surfaces Surface
}

func metadata(family Family, cost Cost, surfaces ...Surface) specMetadata {
	var primary Surface
	if len(surfaces) > 0 {
		primary = surfaces[0]
	}
	return specMetadata{
		family: family, cost: cost, primary: primary, surfaces: Surfaces(surfaces...),
	}
}

type Registry struct {
	database              *storage.DB
	executor              Executor
	specs                 map[string]Spec
	admissionInterceptors []Interceptor
	interceptors          []Interceptor
}

// Interceptor 可用于 guard 前的执行准入或 guard 后的策略检查。返回非 nil error 时该调用
// 不进入 executor。返回 *InterceptorRejection 表示策略拒绝：回灌模型一条结构化提示，
// 不算工具执行失败、不触发自动重试、不消耗恢复预算。
type Interceptor func(ctx context.Context, spec Spec, input any) error

// InterceptorRejection 是拦截器的策略拒绝载荷；agent 恢复中间件据此回灌模型一条结构化
// 提示，而不把它计入失败恢复账。
type InterceptorRejection struct {
	Observation string
	Data        map[string]any
}

func (rejection *InterceptorRejection) Error() string { return rejection.Observation }

// Use 追加一个执行拦截器；多个拦截器按注册序在执行链中运行。
func (registry *Registry) Use(interceptor Interceptor) {
	if interceptor != nil {
		registry.interceptors = append(registry.interceptors, interceptor)
	}
}

// UseAdmission 追加 guard 前的执行准入拦截器。它只适合不依赖工具前置条件的能力边界，
// 例如拒绝模型调用本轮未披露的工具；普通策略拦截仍应使用 Use 保持 guard 后语义。
func (registry *Registry) UseAdmission(interceptor Interceptor) {
	if interceptor != nil {
		registry.admissionInterceptors = append(registry.admissionInterceptors, interceptor)
	}
}

func NewRegistry(database *storage.DB, executor Executor) (*Registry, error) {
	if database == nil || executor == nil {
		return nil, errors.New("tool registry 缺少 database 或 executor")
	}
	registry := &Registry{database: database, executor: executor, specs: map[string]Spec{}}
	builders := []func(*Registry) error{
		registerAssetImport, registerAssetList, registerDetectShots, registerShotSearch, registerAudioBeatAnalysis,
		registerSpeechPauseAnalysis, registerSpeechTranscribe, registerSpeechSearch, registerAskUser,
		registerDecisionAnswer, registerPlanUpdate, registerMemoryUpdate,
		registerComposeInitial, registerApplyPatchBatch,
		registerTimelineInsert, registerTimelineDelete, registerTimelineUpdate, registerTimelineSplit,
		registerBeatRecut, registerTalkingHeadEdit,
		registerTimelineCheck, registerTimelineInspect, registerRenderPreview,
		registerRenderFinal, registerRenderStatus, registerPreviewCheck,
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
	if toolName, atomic := atomicTimelineToolForType(input); atomic {
		operation, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s 必须是 JSON 对象", path)
		}
		if _, err := TimelineAtomicOperation(toolName, atomicTimelineInputValue(input, operation)); err != nil {
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

func atomicTimelineToolForType(input reflect.Type) (string, bool) {
	switch input {
	case reflect.TypeFor[TimelineInsertInput]():
		return "timeline.insert", true
	case reflect.TypeFor[TimelineDeleteInput]():
		return "timeline.delete", true
	case reflect.TypeFor[TimelineUpdateInput]():
		return "timeline.update", true
	case reflect.TypeFor[TimelineSplitInput]():
		return "timeline.split", true
	default:
		return "", false
	}
}

func atomicTimelineInputValue(input reflect.Type, operation map[string]any) any {
	switch input {
	case reflect.TypeFor[TimelineInsertInput]():
		return TimelineInsertInput(operation)
	case reflect.TypeFor[TimelineDeleteInput]():
		return TimelineDeleteInput(operation)
	case reflect.TypeFor[TimelineUpdateInput]():
		return TimelineUpdateInput(operation)
	case reflect.TypeFor[TimelineSplitInput]():
		return TimelineSplitInput(operation)
	default:
		return nil
	}
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
		err := registry.guard(ctx, spec)
		if err == nil {
			result = append(result, spec)
			continue
		}
		if !errors.Is(err, errPreconditionNotMet) {
			return nil, fmt.Errorf("判断工具 %s 是否可用: %w", spec.Name, err)
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
	classification specMetadata,
) error {
	if _, exists := registry.specs[name]; exists {
		return fmt.Errorf("工具重复注册: %s", name)
	}
	if !effect.Valid() {
		return fmt.Errorf("工具 %s 缺少合法 Effect 风险分级: %q", name, effect)
	}
	if !classification.family.Valid() {
		return fmt.Errorf("工具 %s 缺少合法 Family 分类: %q", name, classification.family)
	}
	if !classification.cost.Valid() {
		return fmt.Errorf("工具 %s 缺少合法 Cost 分类: %q", name, classification.cost)
	}
	if exposure == ExposureLLM && !classification.surfaces.Valid() {
		return fmt.Errorf("模型工具 %s 缺少动态 Surface 阶段", name)
	}
	if exposure == ExposureLLM && !classification.primary.Single() {
		return fmt.Errorf("模型工具 %s 的 PrimarySurface 必须是单个合法阶段", name)
	}
	if exposure == ExposureLLM && !classification.surfaces.Includes(classification.primary) {
		return fmt.Errorf("模型工具 %s 的 PrimarySurface 不属于 Surfaces", name)
	}
	if err := validateFamilyEffect(name, classification.family, effect); err != nil {
		return err
	}
	inputType := reflect.TypeFor[I]()
	if exposure == ExposureLLM {
		if key := prohibitedField(inputType); key != "" {
			return fmt.Errorf("工具 %s 的字段被 PolicyGate 禁止: %s", name, key)
		}
	}
	implementation, err := utils.InferTool(name, description, func(ctx context.Context, input I) (O, error) {
		spec := registry.specs[name]
		for _, interceptor := range registry.admissionInterceptors {
			if err := interceptor(ctx, spec, input); err != nil {
				var zero O
				return zero, err
			}
		}
		if err := registry.guard(ctx, spec); err != nil {
			var zero O
			return zero, err
		}
		for _, interceptor := range registry.interceptors {
			if err := interceptor(ctx, spec, input); err != nil {
				var zero O
				return zero, err
			}
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
	}, utils.WithUnmarshalArguments(func(ctx context.Context, arguments string) (any, error) {
		return strictUnmarshalToolArguments[I](ctx, name, arguments)
	}))
	if err != nil {
		return err
	}
	registry.specs[name] = Spec{
		Name: name, Description: description, Requires: append([]string(nil), requires...),
		Exposure: exposure, Family: classification.family, Cost: classification.cost,
		PrimarySurface: classification.primary, Surfaces: classification.surfaces,
		Effect: effect, Optional: optional,
		InputType: inputType, Implementation: implementation,
	}
	return nil
}

func validateFamilyEffect(name string, family Family, effect Effect) error {
	valid := false
	switch family {
	case FamilyRead:
		valid = effect == EffectReadOnly
	case FamilyDetect, FamilyCheck:
		valid = effect == EffectReadOnly || effect == EffectReversible
	case FamilyEdit:
		valid = effect == EffectReversible || effect == EffectDestructive
	case FamilyControl:
		valid = effect == EffectReversible || effect == EffectDestructive
	}
	if !valid {
		return fmt.Errorf("工具 %s 的 Family=%q 与 Effect=%q 不一致", name, family, effect)
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

// Spec 返回指定工具的完整分类元数据；执行路由据此组合 Family 与 Effect，
// 不复制 detector 名单。返回值是副本，调用方无法修改 Registry 事实源。
func (registry *Registry) Spec(name string) (Spec, bool) {
	spec, exists := registry.specs[name]
	return spec, exists
}

func strictUnmarshalToolArguments[I any](_ context.Context, name, arguments string) (any, error) {
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
	if _, atomic := timelineAtomicKinds[name]; atomic {
		if _, err := TimelineAtomicOperation(name, any(input)); err != nil {
			var fieldErr *timeline.OpFieldError
			if !errors.As(err, &fieldErr) {
				return nil, err
			}
		}
	}
	return input, nil
}

var errPreconditionNotMet = errors.New("工具前置条件不满足")

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
			return fmt.Errorf("%w: 工具 %s 未满足 %s", errPreconditionNotMet, spec.Name, predicate)
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
	return addTool[AssetImportInput, ToolResult](registry, "asset.import_local_file", "导入用户已确认的本地素材", nil, ExposureHarness, EffectReversible, false,
		metadata(FamilyEdit, CostStandard))
}

func registerAssetList(registry *Registry) error {
	return addTool[AssetListInput, AssetListResult](registry, "asset.list_assets", "列出当前草稿可用素材", nil, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyRead, CostLow, SurfaceDiscovery, SurfaceTalkingHead, SurfaceBeatEdit))
}

func registerDetectShots(registry *Registry) error {
	return addTool[DetectShotsInput, DetectShotsResult](registry, "media.detect_shots", "为一个视频素材建立或刷新可检索的逐镜头证据；每次只接收一个 asset_id，多素材必须并行调用；相同参数默认复用持久化结果，deep 或 force_refresh 可能排队并在完成后自动续跑", []string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyDetect, CostHigh, SurfaceDiscovery, SurfaceTalkingHead, SurfaceBeatEdit))
}

func registerShotSearch(registry *Registry) error {
	return addTool[ShotSearchInput, ShotSearchResult](
		registry,
		"shot.search",
		"只读搜索既有镜头索引；按创作意图返回稳定 shot_id、精确源帧、语义与匹配证据。未建立索引的素材只会列为 detection_candidates；先并行调用 media.detect_shots，再用同一意图重搜，禁止把候选素材臆造为 shot_id",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyRead, CostStandard, SurfaceDiscovery, SurfaceTalkingHead, SurfaceBeatEdit),
	)
}

func registerAudioBeatAnalysis(registry *Registry) error {
	return addTool[AudioBeatAnalysisInput, AudioBeatAnalysisResult](
		registry,
		"audio.analyze_beats",
		"读取音频的 BPM、普通拍点、强瞬态、推断小节第一拍和按时间顺序压缩的 RMS 波形。拍点坐标使用整数帧；波形使用固定 0-100 编码并返回采样间隔，不标注高潮、低潮或剪辑好坏",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyDetect, CostStandard, SurfaceBeatEdit),
	)
}

func registerSpeechPauseAnalysis(registry *Registry) error {
	return addTool[SpeechPauseAnalysisInput, SpeechPauseAnalysisResult](
		registry,
		"audio.analyze_speech_pauses",
		"分析音频或视频内音轨的停顿/气口，返回源素材整数帧；传 timeline_clip_id 时同时映射为当前时间线帧，可用于剪口播。结果是 RMS 静音候选，不会把语义停顿或口头禅误报成已确认删除项",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyDetect, CostStandard, SurfaceTalkingHead),
	)
}

func registerSpeechTranscribe(registry *Registry) error {
	return addTool[SpeechTranscribeInput, SpeechTranscribeResult](
		registry,
		"speech.transcribe",
		"为一个音频或视频素材建立或刷新带词级整数帧坐标的 transcript 索引；每次只处理一个 asset_id，多素材必须并行调用；只生成证据，不搜索台词或编辑时间线",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyDetect, CostHigh, SurfaceTalkingHead),
	)
}

func registerSpeechSearch(registry *Registry) error {
	return addTool[SpeechSearchInput, SpeechSearchResult](
		registry,
		"speech.search",
		"只读搜索已有 transcript；按台词语义、稳定 ID 或源帧范围返回逐句、词级、气口和相似台词证据。缺少索引时返回 index_missing，并提示调用 speech.transcribe；绝不触发 ASR、创建 job 或写入 transcript",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyRead, CostStandard, SurfaceTalkingHead),
	)
}

func registerAskUser(registry *Registry) error {
	return addTool[AskUserInput, ToolResult](registry, "interaction.ask_user", "仅在缺少会实质改变成片目标、且无法从素材或上下文安全推断的关键决策时，通过简短结构化决策卡向用户提问；已有可用素材时，成片类型、时长、风格和节奏等可逆首剪细节必须结合 user_memory 与安全默认值自主决定，不得用此工具追问", nil, ExposureLLM, EffectReversible, false,
		metadata(FamilyControl, CostLow, SurfaceControl, SurfaceDiscovery))
}

func registerDecisionAnswer(registry *Registry) error {
	return addTool[DecisionAnswerInput, ToolResult](registry, "decision.answer", "提交结构化决策答案", nil, ExposureLLM, EffectReversible, false,
		metadata(FamilyControl, CostLow, SurfaceControl))
}

func registerPlanUpdate(registry *Registry) error {
	return addTool[PlanUpdateInput, ToolResult](
		registry,
		"plan.update",
		"以 RFC 7396 语义增量合并 plan；reset=true 时先清空旧计划再应用该对象，用于在跨回合继续工作前保存已确定的计划结构；素材可用但请求宽泛时，用此工具记录基于长期画像作出的首剪默认决定并继续执行，不要转去追问可回滚细节",
		nil, ExposureLLM, EffectReversible, false,
		metadata(FamilyControl, CostStandard,
			SurfaceControl,
			SurfaceDiscovery,
			SurfaceTalkingHead,
			SurfaceBeatEdit,
			SurfaceRender,
			SurfacePreviewCheck,
		),
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
		metadata(FamilyControl, CostStandard, SurfaceControl),
	)
}

func registerComposeInitial(registry *Registry) error {
	return addTool[ComposeInitialInput, ToolResult](registry, "timeline.compose_initial", "按整数帧源区间组装时间线；只传入 video/image 主视觉素材，不能传 audio/font；先从 asset.list_assets 读取 kind、duration_frames 与 timeline_fps", []string{"usable_asset_exists", "timeline_absent"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceDiscovery, SurfaceTalkingHead))
}

func registerApplyPatchBatch(registry *Registry) error {
	return addTool[TimelinePatchBatchInput, ToolResult](
		registry,
		"timeline.apply_patches",
		"应用旧版批量时间线补丁；仅供 REST/harness 迁移调用，模型使用 timeline.insert/delete/update/split",
		[]string{"timeline_exists"}, ExposureHarness, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceTimelineEdit),
	)
}

func registerTimelineInsert(registry *Registry) error {
	return addTool[TimelineInsertInput, ToolResult](
		registry,
		"timeline.insert",
		"插入一个素材 clip 或一条字幕；空时间线只允许先插入 visual_base clip，素材类型和原声联动由服务端派生",
		nil, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostStandard, SurfaceTimelineEdit),
	)
}

func registerTimelineDelete(registry *Registry) error {
	return addTool[TimelineDeleteInput, ToolResult](
		registry,
		"timeline.delete",
		"只删除一个 clip、一个连续帧范围或一个非主视觉轨内容集合；需要多个目标时按稳定顺序多次调用",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostStandard, SurfaceTimelineEdit),
	)
}

func registerTimelineUpdate(registry *Registry) error {
	return addTool[TimelineUpdateInput, ToolResult](
		registry,
		"timeline.update",
		"只更新一个 clip、track 或 subtitle 目标；kind 选择裁剪、移动、重排、替换、速率、音量、淡入淡出、联动、轨道状态或字幕内容",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostStandard, SurfaceTimelineEdit),
	)
}

func registerTimelineSplit(registry *Registry) error {
	return addTool[TimelineSplitInput, ToolResult](
		registry,
		"timeline.split",
		"只在一个 timeline_clip_id 的一个时间线整数帧位置切分片段",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostStandard, SurfaceTimelineEdit),
	)
}

func registerBeatRecut(registry *Registry) error {
	return addTool[TimelineBeatRecutInput, ToolResult](
		registry,
		"timeline.recut_to_beats",
		"从空时间线或已有时间线原子完成卡点混剪：传 bgm_asset_id 后按真实拍点重建主视频；优先传 shot.search 返回的有序 shot_ids，工具会解析精确源帧。cut_frames 可多于视频素材数，同一素材会使用不同且不重叠的源区间；use_all_video_assets=true 表示每个素材至少一次；cover_entire_bgm=true 覆盖整首音乐；SFX 始终独立分轨。禁止用 compose_initial 加几十个低层补丁替代",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceBeatEdit),
	)
}

func registerTalkingHeadEdit(registry *Registry) error {
	return addTool[TalkingHeadEditInput, ToolResult](
		registry,
		"timeline.edit_talking_head",
		"旧版口播复合编辑；仅供迁移期 harness 回归，生产模型使用 speech.search、shot.search 与 timeline.insert/delete/update/split 组合",
		[]string{"timeline_exists"}, ExposureHarness, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceTalkingHead),
	)
}

func registerTimelineCheck(registry *Registry) error {
	return addTool[TimelineCheckInput, ToolResult](registry, "timeline.check", "只读检查当前时间线的结构不变量、内容合同、节拍对齐与口播质量；不写 validation event、draft state 或 timeline version", []string{"timeline_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyCheck, CostStandard, SurfaceRender, SurfaceTimelineEdit))
}

func registerTimelineInspect(registry *Registry) error {
	return addTool[TimelineInspectInput, ToolResult](registry, "timeline.inspect", "读取可编辑的时间线摘要与完整 track/clip ID、素材、角色和帧范围；尚无时间线时返回 timeline_exists=false，而不是失败", nil, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyRead, CostLow, SurfaceTimelineEdit, SurfaceTalkingHead, SurfaceBeatEdit, SurfaceRender, SurfacePreviewCheck))
}

func registerRenderPreview(registry *Registry) error {
	return addTool[RenderPreviewInput, ToolResult](registry, "render.preview", "校验当前时间线并原子排队渲染预览；失败不创建任务", []string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceRender))
}

func registerRenderFinal(registry *Registry) error {
	return addTool[RenderFinalInput, ToolResult](registry, "render.final_mp4", "校验当前时间线并原子排队导出 MP4；失败不创建任务", []string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		metadata(FamilyEdit, CostHigh, SurfaceRender))
}

func registerRenderStatus(registry *Registry) error {
	return addTool[RenderStatusInput, ToolResult](registry, "render.status", "读取渲染任务与产物状态", []string{"timeline_exists"}, ExposureLLM, EffectReadOnly, false,
		metadata(FamilyRead, CostLow, SurfaceRender, SurfacePreviewCheck))
}

func registerPreviewCheck(registry *Registry) error {
	return addTool[PreviewCheckInput, PreviewInspectionResult](registry, "preview.check", "对一个 preview 执行一个明确检查；check 只能是 decode、black、freeze、silence、loudness 或 visual 之一，多个独立检查由模型并行调用", []string{"any_preview_exists"}, ExposureLLM, EffectReadOnly, true,
		metadata(FamilyCheck, CostHigh, SurfacePreviewCheck))
}

func registerConfirmAction(registry *Registry) error {
	// 创建确认决策是一次可逆写入（决策行）；G2 的强制确认拦截器读 EffectDestructive，
	// confirm_action 本身不是被拦截对象，故按其写行为归可逆。
	return addTool[ConfirmActionInput, ToolResult](registry, "interaction.confirm_action", "为破坏性动作创建确认决策", nil, ExposureLLM, EffectReversible, true,
		metadata(FamilyControl, CostLow, SurfaceControl))
}
