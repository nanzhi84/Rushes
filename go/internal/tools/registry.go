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
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type Exposure string

const (
	ExposureLLM     Exposure = "llm"
	ExposureMeta    Exposure = "model_meta"
	ExposureHarness Exposure = "harness_only"
)

type ActionCategory string

const (
	ActionCategoryMaterialEvidence ActionCategory = "material_evidence"
	ActionCategoryPlanning         ActionCategory = "planning_interaction_memory"
	ActionCategoryTimelineEdit     ActionCategory = "timeline_edit"
)

func (category ActionCategory) Valid() bool {
	switch category {
	case ActionCategoryMaterialEvidence, ActionCategoryPlanning, ActionCategoryTimelineEdit:
		return true
	default:
		return false
	}
}

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

// Cost 是工具单次调用的相对成本，只用于 Harness 治理和可观测性，不进入模型 schema。
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
	Name                string
	Description         string
	CatalogDescription  string
	Requires            []string
	Exposure            Exposure
	Family              Family
	Cost                Cost
	ActionCategory      ActionCategory
	Effect              Effect
	CompletionSemantics CompletionSemantics
	TypedSuccessAdapter bool
	Optional            bool
	InputType           reflect.Type
	Implementation      tool.BaseTool
}

// Parallelizable 从 Effect 单一事实源派生；不额外维护会漂移的并发布尔字段。
func (spec Spec) Parallelizable() bool { return spec.Effect == EffectReadOnly }

type specMetadata struct {
	family     Family
	cost       Cost
	category   ActionCategory
	catalog    string
	completion CompletionSemantics
}

func modelMetadata(
	family Family,
	cost Cost,
	completion CompletionSemantics,
	category ActionCategory,
	catalog string,
) specMetadata {
	return specMetadata{
		family: family, cost: cost, category: category, catalog: catalog,
		completion: completion,
	}
}

func terminalMetadata(family Family, cost Cost, category ActionCategory, catalog string) specMetadata {
	return modelMetadata(family, cost, CompletionTerminalOnly, category, catalog)
}

func waitingUserMetadata(family Family, cost Cost, category ActionCategory, catalog string) specMetadata {
	return modelMetadata(family, cost, CompletionTerminalOrWaitingUser, category, catalog)
}

func harnessMetadata(family Family, cost Cost) specMetadata {
	return specMetadata{family: family, cost: cost}
}

type Registry struct {
	database     *storage.DB
	executor     Executor
	specs        map[string]Spec
	interceptors []Interceptor
}

// Interceptor 用于 guard 通过后的策略检查。返回非 nil error 时该调用
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

// PreconditionRejection 表示 Registry 在进入策略拦截器与 Executor 前发现的确定性状态拒绝。
// 它保留 errPreconditionNotMet 的 errors.Is 兼容性，同时给 Agent loop 一份无需解析文案的
// 稳定自修复信封。
type PreconditionRejection struct {
	Observation string
	Data        map[string]any
}

func (rejection *PreconditionRejection) Error() string { return rejection.Observation }
func (rejection *PreconditionRejection) Unwrap() error { return errPreconditionNotMet }

// Use 追加一个执行拦截器；多个拦截器按注册序在执行链中运行。
func (registry *Registry) Use(interceptor Interceptor) {
	if interceptor != nil {
		registry.interceptors = append(registry.interceptors, interceptor)
	}
}

func NewRegistry(database *storage.DB, executor Executor) (*Registry, error) {
	if database == nil || executor == nil {
		return nil, errors.New("tool registry 缺少 database 或 executor")
	}
	registry := &Registry{database: database, executor: executor, specs: map[string]Spec{}}
	builders := []func(*Registry) error{
		registerAssetImport, registerAssetList, registerDetectShots, registerShotSearch, registerShotDeepSearch, registerAudioBeatAnalysis,
		registerSpeechPauseAnalysis, registerSpeechTranscribe, registerSpeechSearch, registerAskUser,
		registerDecisionAnswer, registerPlanUpdate, registerMemorySet, registerMemoryRemove,
		registerTimelineInsert, registerTimelineDelete, registerTimelineUpdate, registerTimelineSplit,
		registerTimelineCheck, registerTimelineInspect, registerPreviewGenerate, registerPreviewCheck,
		registerConfirmAction,
	}
	for _, builder := range builders {
		if err := builder(registry); err != nil {
			return nil, err
		}
	}
	if err := registerToolLoad(registry); err != nil {
		return nil, err
	}
	return registry, nil
}

type ModelActionCatalogEntry struct {
	Category    ActionCategory `json:"category"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Cost        Cost           `json:"cost"`
	Risk        Effect         `json:"risk"`
}

func (registry *Registry) ModelActionCatalog() ([]ModelActionCatalogEntry, error) {
	entries := make([]ModelActionCatalogEntry, 0, 13)
	for _, spec := range registry.Specs(true) {
		if spec.Exposure != ExposureLLM {
			continue
		}
		if !spec.ActionCategory.Valid() || strings.TrimSpace(spec.CatalogDescription) == "" {
			return nil, fmt.Errorf("模型 action %s 缺少 Catalog 分类或说明", spec.Name)
		}
		entries = append(entries, ModelActionCatalogEntry{
			Category: spec.ActionCategory, Name: spec.Name, Description: spec.CatalogDescription,
			Cost: spec.Cost, Risk: spec.Effect,
		})
	}
	return entries, nil
}

func (registry *Registry) ModelActionNames() []string {
	entries, err := registry.ModelActionCatalog()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
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
	if exposure == ExposureLLM && !classification.category.Valid() {
		return fmt.Errorf("模型工具 %s 缺少 Model Action Catalog 分类", name)
	}
	if exposure == ExposureLLM && (strings.TrimSpace(classification.catalog) == "" ||
		strings.ContainsAny(classification.catalog, "\r\n")) {
		return fmt.Errorf("模型工具 %s 缺少单行 Catalog 说明", name)
	}
	if exposure != ExposureLLM && classification.catalog != "" {
		return fmt.Errorf("非模型工具 %s 不得声明 Catalog 说明", name)
	}
	if exposure == ExposureLLM && !classification.completion.Valid() {
		return fmt.Errorf("模型工具 %s 缺少合法 CompletionSemantics: %q", name, classification.completion)
	}
	if exposure != ExposureLLM && classification.completion != "" {
		return fmt.Errorf("非模型工具 %s 不得声明 CompletionSemantics: %q", name, classification.completion)
	}
	if classification.completion == CompletionTerminalOrWaitingUser &&
		name != "interaction.ask_user" && name != "interaction.confirm_action" {
		return fmt.Errorf("模型工具 %s 不允许返回 waiting_user", name)
	}
	if err := validateFamilyEffect(name, classification.family, effect); err != nil {
		return err
	}
	inputType := reflect.TypeFor[I]()
	outputType := reflect.TypeFor[O]()
	if exposure == ExposureLLM {
		if key := prohibitedField(inputType); key != "" {
			return fmt.Errorf("工具 %s 的字段被 PolicyGate 禁止: %s", name, key)
		}
	}
	implementation, err := utils.InferTool(name, description, func(ctx context.Context, input I) (O, error) {
		spec := registry.specs[name]
		if failure, failed := atomicTimelinePreflightFailure(name, input); failed {
			return convertResult[O](failure)
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
		CatalogDescription: classification.catalog,
		Exposure:           exposure, Family: classification.family, Cost: classification.cost,
		ActionCategory: classification.category,
		Effect:         effect, CompletionSemantics: classification.completion,
		TypedSuccessAdapter: exposure == ExposureLLM && !jsonTypeHasField(outputType, "status"),
		Optional:            optional,
		InputType:           inputType, Implementation: implementation,
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

// ModelReceiptPolicy 返回 Registry 持有的模型完成合同；harness-only 工具
// 刻意不提供模型回执策略。
func (registry *Registry) ModelReceiptPolicy(name string) (ModelReceiptPolicy, bool) {
	spec, exists := registry.specs[name]
	if !exists || (spec.Exposure != ExposureLLM && spec.Exposure != ExposureMeta) ||
		!spec.CompletionSemantics.Valid() {
		return ModelReceiptPolicy{}, false
	}
	return ModelReceiptPolicy{
		Completion:          spec.CompletionSemantics,
		TypedSuccessAdapter: spec.TypedSuccessAdapter,
	}, true
}

const toolLoadDescription = "加载一个或多个已在 Model Action Catalog 中列出的 action 完整参数定义；首次需要这些能力或当前已加载工具不足时使用。"

func registerToolLoad(registry *Registry) error {
	if _, exists := registry.specs["tool.load"]; exists {
		return errors.New("工具重复注册: tool.load")
	}
	names := registry.ModelActionNames()
	items := &jsonschema.Schema{Type: "string", Description: "Model Action Catalog 中的准确 action 名称"}
	for _, name := range names {
		items.Enum = append(items.Enum, name)
	}
	properties := jsonschema.NewProperties()
	properties.Set("tool_names", &jsonschema.Schema{
		Type: "array", Items: items, MinItems: uint64Pointer(1), MaxItems: uint64Pointer(5),
		UniqueItems: true, Description: "本轮要加载完整 schema 的准确 action 名称；不得提交自然语言任务",
	})
	info := &schema.ToolInfo{
		Name: "tool.load", Desc: toolLoadDescription,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&jsonschema.Schema{
			Type: "object", Properties: properties, Required: []string{"tool_names"},
			AdditionalProperties: jsonschema.FalseSchema,
		}),
	}
	implementation := utils.NewTool[ToolLoadInput, ToolLoadResult](
		info,
		func(ctx context.Context, input ToolLoadInput) (ToolLoadResult, error) {
			if reporter, ok := ReporterFromContext(ctx); ok {
				reporter(ctx, "tool.load", "started", input, nil, nil)
			}
			raw, err := registry.executor.ExecuteTool(ctx, "tool.load", input)
			result, convertErr := convertResult[ToolLoadResult](raw)
			if err == nil {
				err = convertErr
			}
			if reporter, ok := ReporterFromContext(ctx); ok {
				reporter(ctx, "tool.load", "finished", input, result, err)
			}
			return result, err
		},
		utils.WithUnmarshalArguments(func(ctx context.Context, arguments string) (any, error) {
			decoded, err := strictUnmarshalToolArguments[ToolLoadInput](ctx, "tool.load", arguments)
			if err != nil {
				return nil, err
			}
			input := decoded.(ToolLoadInput)
			if err := ValidateToolLoadInput(input); err != nil {
				return nil, err
			}
			return input, nil
		}),
	)
	registry.specs["tool.load"] = Spec{
		Name: "tool.load", Description: toolLoadDescription, Exposure: ExposureMeta,
		Family: FamilyRead, Cost: CostLow, Effect: EffectReadOnly,
		CompletionSemantics: CompletionTerminalOnly, InputType: reflect.TypeFor[ToolLoadInput](),
		Implementation: implementation,
	}
	return nil
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
	if normalized, ok := normalizeTimelineAtomicFrameStrings(name, input).(I); ok {
		input = normalized
	}
	return input, nil
}

var errPreconditionNotMet = errors.New("工具前置条件不满足")

func preconditionRejection(toolName, predicate string) *PreconditionRejection {
	message := "工具 " + toolName + " 的前置状态尚未满足"
	errorCode := ErrCodeToolValidationFailed
	recovery := "满足前置条件 " + predicate + " 后重试。"
	currentState := map[string]any{predicate: false}
	switch predicate {
	case "usable_asset_exists":
		message = "当前没有可用素材"
		errorCode = ErrCodeUsableAssetNotExists
		recovery = "请用户先导入并等待至少一个素材变为可用状态"
	case "transcript_index_exists":
		message = "当前尚未建立可用的 transcript 索引"
		errorCode = ErrCodeTranscriptIndexNotExists
		recovery = "等待 Harness 为可用口播素材建立 transcript 索引后重试"
	case "timeline_exists":
		message = "当前尚未创建时间线"
		errorCode = ErrCodeTimelineNotExists
		recovery = "加载并使用 timeline.insert 插入首个 visual_base clip"
	case "any_preview_exists":
		message = "当前尚未生成可检查的预览"
		errorCode = ErrCodePreviewNotExists
		recovery = "先让 Stop Gate 对已通过 timeline.check 的精确版本生成预览"
	}
	return &PreconditionRejection{
		Observation: message,
		Data: map[string]any{
			"error_code":    string(errorCode),
			"message":       message,
			"current_state": currentState,
			"recovery":      recovery,
		},
	}
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
			return preconditionRejection(spec.Name, predicate)
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
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return result, errors.New("工具结果不得为 null")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("包含多个 JSON 值")
		}
		return result, err
	}
	return result, nil
}

func jsonTypeHasField(output reflect.Type, target string) bool {
	return jsonTypeHasFieldRecursive(output, target, map[reflect.Type]struct{}{})
}

func jsonTypeHasFieldRecursive(
	output reflect.Type,
	target string,
	active map[reflect.Type]struct{},
) bool {
	for output != nil && output.Kind() == reflect.Pointer {
		output = output.Elem()
	}
	if output == nil || output.Kind() != reflect.Struct {
		return false
	}
	if _, recursive := active[output]; recursive {
		return false
	}
	active[output] = struct{}{}
	defer delete(active, output)
	for index := range output.NumField() {
		field := output.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		tag := field.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if name == "" && !field.Anonymous {
			name = field.Name
		}
		if name == target {
			return true
		}
		if field.Anonymous && name == "" && jsonTypeHasFieldRecursive(field.Type, target, active) {
			return true
		}
	}
	return false
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
		harnessMetadata(FamilyEdit, CostStandard))
}

func registerAssetList(registry *Registry) error {
	return addTool[AssetListInput, AssetListResult](registry, "asset.list_assets", "Harness 读取当前草稿完整素材清单；模型使用 WorldState 自动注入的有界 material_catalog", nil, ExposureHarness, EffectReadOnly, false,
		harnessMetadata(FamilyRead, CostLow))
}

func registerDetectShots(registry *Registry) error {
	return addTool[DetectShotsInput, DetectShotsResult](registry, "media.detect_shots", "Harness 为视频素材建立或刷新可检索的持久基础镜头索引；普通导入完成后自动排队，同内容哈希跨草稿复用", []string{"usable_asset_exists"}, ExposureHarness, EffectReversible, false,
		harnessMetadata(FamilyDetect, CostHigh))
}

func registerShotSearch(registry *Registry) error {
	return addTool[ShotSearchInput, ShotSearchResult](
		registry,
		"shot.search",
		"在一次调用开始时冻结目标视频素材，等待其基础镜头索引全部 search_ready 后，对固定 index_snapshot_id 执行无 embedding 的只读文字检索；返回稳定 ShotRef、权威源帧、分数与字段证据，绝不返回部分索引或伪候选",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostStandard, ActionCategoryMaterialEvidence,
			"检索基础镜头索引并返回稳定源帧证据；普通选镜或查找候选镜头时使用。"),
	)
}

func registerShotDeepSearch(registry *Registry) error {
	return addTool[ShotDeepSearchInput, ShotDeepSearchResult](
		registry,
		"shot.deep_search",
		"对 shot.search 返回的 1 到 8 个精确 ShotRef 做高成本视觉复核；绑定原冻结快照，从权威镜头边界新增有序帧或高分辨率帧，持久化查询无关的通用事实，并确定性返回 requirements、exclusions、preferences 的逐项帧证据。不能传整批素材或源帧范围",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyDetect, CostHigh, ActionCategoryMaterialEvidence,
			"对 shot.search 返回的精确 ShotRef 做高成本视觉复核；基础证据不足或需确认 OCR、动作和细节时使用。"),
	)
}

func registerAudioBeatAnalysis(registry *Registry) error {
	return addTool[AudioBeatAnalysisInput, AudioBeatAnalysisResult](
		registry,
		"audio.analyze_beats",
		"Harness 按需建立并复用音频 BPM、拍点、强瞬态、小节相位与 RMS 波形证据",
		[]string{"usable_asset_exists"}, ExposureHarness, EffectReversible, false,
		harnessMetadata(FamilyDetect, CostStandard),
	)
}

func registerSpeechPauseAnalysis(registry *Registry) error {
	return addTool[SpeechPauseAnalysisInput, SpeechPauseAnalysisResult](
		registry,
		"audio.analyze_speech_pauses",
		"Harness 按需建立并复用音频或视频内音轨的停顿/气口证据",
		[]string{"usable_asset_exists"}, ExposureHarness, EffectReversible, false,
		harnessMetadata(FamilyDetect, CostStandard),
	)
}

func registerSpeechTranscribe(registry *Registry) error {
	return addTool[SpeechTranscribeInput, SpeechTranscribeResult](
		registry,
		"speech.transcribe",
		"Harness 为明确口播目标按需建立或复用带词级整数帧坐标的 transcript 索引",
		[]string{"usable_asset_exists"}, ExposureHarness, EffectReversible, false,
		harnessMetadata(FamilyDetect, CostHigh),
	)
}

func registerSpeechSearch(registry *Registry) error {
	return addTool[SpeechSearchInput, SpeechSearchResult](
		registry,
		"speech.search",
		"只读搜索 Harness 已确保就绪的 transcript；按台词语义、稳定 ID 或源帧范围返回逐句、词级、气口和相似台词证据",
		[]string{"usable_asset_exists"}, ExposureLLM, EffectReadOnly, false,
		terminalMetadata(FamilyRead, CostStandard, ActionCategoryMaterialEvidence,
			"检索已就绪转写并返回台词、词级帧位置和气口证据；口播选句、删改或对齐时使用。"),
	)
}

func registerAskUser(registry *Registry) error {
	return addTool[AskUserInput, ToolResult](registry, "interaction.ask_user", "仅在缺少会实质改变成片目标、且无法从素材或上下文安全推断的关键决策时，通过简短结构化决策卡向用户提问；已有可用素材时，成片类型、时长、风格和节奏等可逆首剪细节必须结合 user_memory 与安全默认值自主决定，不得用此工具追问", nil, ExposureLLM, EffectReversible, false,
		waitingUserMetadata(FamilyControl, CostLow, ActionCategoryPlanning,
			"创建一张关键决策卡向用户提问；只在缺失无法安全推断且会实质改变成片目标的信息时使用。"))
}

func registerDecisionAnswer(registry *Registry) error {
	return addTool[DecisionAnswerInput, ToolResult](registry, "decision.answer", "提交结构化决策答案", nil, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyControl, CostLow, ActionCategoryPlanning,
			"提交用户对结构化决策卡的答案；当待处理决策已获得用户回答时使用。"))
}

func registerPlanUpdate(registry *Registry) error {
	return addTool[PlanUpdateInput, ToolResult](
		registry,
		"plan.update",
		"以 RFC 7396 语义增量合并 plan；reset=true 时先清空旧计划再应用该对象，用于在跨回合继续工作前保存已确定的计划结构；素材可用但请求宽泛时，用此工具记录基于长期画像作出的首剪默认决定并继续执行，不要转去追问可回滚细节",
		nil, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyControl, CostStandard, ActionCategoryPlanning,
			"持久化跨回合的创作计划与已确定决策；任务需继续执行或需固化首剪默认时使用。"),
	)
}

func registerMemorySet(registry *Registry) error {
	return addTool[MemorySetInput, ToolResult](
		registry,
		"memory.set",
		"仅当当前用户明确表达跨项目稳定的偏好、习惯或纠正时写入用户画像；一次性草稿要求和模型自己的创作判断不得写入",
		nil, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyControl, CostStandard, ActionCategoryPlanning,
			"写入用户跨项目稳定的偏好、习惯或纠正；用户明确表达会影响未来项目的信息时使用。"),
	)
}

func registerMemoryRemove(registry *Registry) error {
	return addTool[MemoryRemoveInput, ToolResult](
		registry,
		"memory.remove",
		"仅当当前用户明确要求忘记已有长期记忆时删除指定键；此操作必须先获得破坏性确认",
		nil, ExposureLLM, EffectDestructive, false,
		terminalMetadata(FamilyControl, CostStandard, ActionCategoryPlanning,
			"删除指定的长期用户记忆；用户明确要求忘记某项已存记忆时使用。"),
	)
}

func registerTimelineInsert(registry *Registry) error {
	return addTool[TimelineInsertInput, ToolResult](
		registry,
		"timeline.insert",
		"插入一个素材 clip 或一条字幕；空时间线先插入一个 visual_base clip 即创建 v1，后续片段逐次追加。原声联动由服务端派生，只维护确定性音画不变量。插入 BGM 时只提交素材与放置参数；Harness 自动确保并投影完整拍点证据，再校验精确写入版本",
		nil, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyEdit, CostStandard, ActionCategoryTimelineEdit,
			"向时间线插入一个素材片段或一条字幕；创建首个视觉片段、追加镜头、字幕或 BGM 时使用。"),
	)
}

func registerTimelineDelete(registry *Registry) error {
	return addTool[TimelineDeleteInput, ToolResult](
		registry,
		"timeline.delete",
		"只删除一个 clip、一个连续帧范围、一个素材的连续源帧范围或一个非主视觉轨内容集合；多个目标必须分成多次调用",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyEdit, CostStandard, ActionCategoryTimelineEdit,
			"从时间线删除一个片段、连续帧范围或内容集合；需移除一个明确时间线目标时使用。"),
	)
}

func registerTimelineUpdate(registry *Registry) error {
	return addTool[TimelineUpdateInput, ToolResult](
		registry,
		"timeline.update",
		"只更新一个 clip、track 或 subtitle 目标；kind 选择裁剪、移动、重排、替换、速率、音量、淡入淡出、联动、轨道状态或字幕内容",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyEdit, CostStandard, ActionCategoryTimelineEdit,
			"更新一个时间线 clip、track 或 subtitle 目标；需裁剪、移动、替换或调整单个目标时使用。"),
	)
}

func registerTimelineSplit(registry *Registry) error {
	return addTool[TimelineSplitInput, ToolResult](
		registry,
		"timeline.split",
		"只在一个 timeline_clip_id 的一个时间线整数帧位置切分片段",
		[]string{"timeline_exists"}, ExposureLLM, EffectReversible, false,
		terminalMetadata(FamilyEdit, CostStandard, ActionCategoryTimelineEdit,
			"在一个时间线整数帧位置切分指定 clip；需对子段单独删除、移动或调整前使用。"),
	)
}

func registerTimelineCheck(registry *Registry) error {
	return addTool[TimelineCheckInput, ToolResult](registry, "timeline.check", "Harness 只读检查指定稳定 timeline_id 的结构不变量、内容合同、节拍对齐与口播质量；不写 validation event、draft state 或 timeline version", []string{"timeline_exists"}, ExposureHarness, EffectReadOnly, false,
		harnessMetadata(FamilyCheck, CostStandard))
}

func registerTimelineInspect(registry *Registry) error {
	return addTool[TimelineInspectInput, ToolResult](registry, "timeline.inspect", "Harness 读取当前时间线或指定稳定 timeline_id 的完整 track/clip ID、素材、角色和帧范围；尚无时间线时返回 timeline_exists=false，而不是失败", nil, ExposureHarness, EffectReadOnly, false,
		harnessMetadata(FamilyRead, CostLow))
}

func registerPreviewGenerate(registry *Registry) error {
	return addTool[PreviewGenerateInput, ToolResult](
		registry,
		"preview.generate",
		"Harness 在明确 Preview QA 边界提交指定 timeline_id 的离线预览任务，并在同一 turn 等到终态",
		[]string{"timeline_exists"}, ExposureHarness, EffectReversible, false,
		harnessMetadata(FamilyEdit, CostHigh),
	)
}

func registerPreviewCheck(registry *Registry) error {
	return addTool[PreviewCheckInput, PreviewInspectionResult](registry, "preview.check", "Harness 对 preview 执行 decode、black、freeze、silence、loudness 或按需 visual 检查，并汇总 PreviewQAReport", []string{"any_preview_exists"}, ExposureHarness, EffectReadOnly, false,
		harnessMetadata(FamilyCheck, CostHigh))
}

func registerConfirmAction(registry *Registry) error {
	// 创建确认决策是一次可逆写入（决策行）；G2 的强制确认拦截器读 EffectDestructive，
	// confirm_action 本身不是被拦截对象，故按其写行为归可逆。
	return addTool[ConfirmActionInput, ToolResult](registry, "interaction.confirm_action", "为破坏性动作创建确认决策", nil, ExposureLLM, EffectReversible, true,
		waitingUserMetadata(FamilyControl, CostLow, ActionCategoryPlanning,
			"为破坏性或会影响 Agent 之外的动作创建确认决策；PolicyGate 要求确认且尚无有效用户同意时使用。"))
}
