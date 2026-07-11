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
	TypedResult    bool
	Optional       bool
	InputType      reflect.Type
	ResultType     reflect.Type
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
		registerAssetImport, registerAssetList, registerUnderstand, registerAskUser,
		registerDecisionAnswer, registerComposeInitial, registerApplyPatch,
		registerTimelineValidate, registerTimelineInspect, registerRenderPreview,
		registerRenderFinal, registerRenderStatus, registerInspectPreview,
		registerRestoreVersion, registerConfirmAction,
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
	typedResult, optional bool,
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
		Exposure: exposure, TypedResult: typedResult, Optional: optional,
		InputType: inputType, ResultType: reflect.TypeFor[O](), Implementation: implementation,
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

var prohibitedParts = []string{"frame", "timecode", "ffmpeg", "filter_complex", "codec", "bitrate", "crf", "preset", "pix_fmt"}
var prohibitedNames = map[string]struct{}{
	"path": {}, "file": {}, "file_path": {}, "source_path": {}, "reference_path": {},
	"workspace_object_uri": {}, "local_path": {}, "argv": {}, "vf": {}, "af": {},
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
	return addTool[AssetImportInput, ToolResult](registry, "asset.import_local_file", "导入用户已确认的本地素材", nil, ExposureHarness, false, false)
}

func registerAssetList(registry *Registry) error {
	return addTool[AssetListInput, AssetListResult](registry, "asset.list_assets", "列出当前草稿可用素材", nil, ExposureLLM, true, false)
}

func registerUnderstand(registry *Registry) error {
	return addTool[UnderstandInput, UnderstandResult](registry, "understand.materials", "理解所选素材并生成带时间证据的摘要", nil, ExposureLLM, true, false)
}

func registerAskUser(registry *Registry) error {
	return addTool[AskUserInput, ToolResult](registry, "interaction.ask_user", "通过结构化决策卡向用户提问", nil, ExposureLLM, false, false)
}

func registerDecisionAnswer(registry *Registry) error {
	return addTool[DecisionAnswerInput, ToolResult](registry, "decision.answer", "提交结构化决策答案", nil, ExposureLLM, false, false)
}

func registerComposeInitial(registry *Registry) error {
	return addTool[ComposeInitialInput, ToolResult](registry, "timeline.compose_initial", "从摘要级片段选择组装时间线 v1", []string{"usable_asset_exists"}, ExposureLLM, false, false)
}

func registerApplyPatch(registry *Registry) error {
	return addTool[TimelinePatchInput, ToolResult](registry, "timeline.apply_patch", "对当前时间线应用语义补丁", []string{"timeline_exists"}, ExposureLLM, false, false)
}

func registerTimelineValidate(registry *Registry) error {
	return addTool[TimelineValidateInput, ToolResult](registry, "timeline.validate", "验证当前时间线不变量", []string{"timeline_exists"}, ExposureLLM, false, false)
}

func registerTimelineInspect(registry *Registry) error {
	return addTool[TimelineInspectInput, ToolResult](registry, "timeline.inspect", "读取提示词安全的时间线摘要", []string{"timeline_exists"}, ExposureLLM, false, false)
}

func registerRenderPreview(registry *Registry) error {
	return addTool[RenderPreviewInput, ToolResult](registry, "render.preview", "排队渲染当前已验证时间线预览", []string{"timeline_validated"}, ExposureLLM, false, false)
}

func registerRenderFinal(registry *Registry) error {
	return addTool[RenderFinalInput, ToolResult](registry, "render.final_mp4", "排队导出最终 MP4", []string{"timeline_validated"}, ExposureLLM, false, false)
}

func registerRenderStatus(registry *Registry) error {
	return addTool[RenderStatusInput, ToolResult](registry, "render.status", "读取渲染任务与产物状态", []string{"timeline_exists"}, ExposureLLM, false, false)
}

func registerInspectPreview(registry *Registry) error {
	return addTool[RenderInspectInput, PreviewInspectionResult](registry, "render.inspect_preview", "检查预览的流、解码、黑帧、静帧、静音和响度", []string{"any_preview_exists"}, ExposureLLM, true, true)
}

func registerRestoreVersion(registry *Registry) error {
	return addTool[TimelineRestoreInput, ToolResult](registry, "timeline.restore_version", "将旧时间线恢复成新版本", []string{"timeline_exists"}, ExposureLLM, false, true)
}

func registerConfirmAction(registry *Registry) error {
	return addTool[ConfirmActionInput, ToolResult](registry, "interaction.confirm_action", "为破坏性动作创建确认决策", nil, ExposureLLM, false, true)
}
