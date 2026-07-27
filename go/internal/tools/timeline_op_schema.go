package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// 运行时仍保留 map 语义；JSONSchema 则把 timeline.Catalog 编译成 oneOf，
// 让模型只看到所选 kind 的合法字段，并隐藏由服务端注入的字段。
type TimelineOp map[string]any

// 四种原子时间线输入直接承载一个 Catalog op，不再使用 ops[] 外壳。
// Go 运行时保留 map 以复用 timeline.ApplyPatch；模型侧 schema 则按工具职责
// 只编译允许的 kind，避免让一次调用跨 target 或跨操作族。
type TimelineInsertInput map[string]any
type TimelineDeleteInput map[string]any
type TimelineUpdateInput map[string]any
type TimelineSplitInput map[string]any

var timelineAtomicKinds = map[string][]string{
	"timeline.insert": {"insert_clip", "insert_subtitle"},
	"timeline.delete": {"delete_clip", "delete_range", "delete_source_range", "remove_track_clips"},
	"timeline.update": {
		"trim_clip",
		"reorder_clip",
		"move_clip",
		"trim_clip_edge",
		"set_track_state",
		"set_track_ducking",
		"set_clip_linked",
		"replace_clip",
		"set_playback_rate",
		"adjust_gain",
		"set_clip_fades",
		"edit_subtitle_text",
	},
	"timeline.split": {"split_clip"},
}

func (TimelineInsertInput) JSONSchema() *jsonschema.Schema {
	return timelineAtomicOpSchema(timelineAtomicKinds["timeline.insert"])
}

func (TimelineDeleteInput) JSONSchema() *jsonschema.Schema {
	return timelineAtomicOpSchema(timelineAtomicKinds["timeline.delete"])
}

func (TimelineUpdateInput) JSONSchema() *jsonschema.Schema {
	schema := timelineAtomicOpSchema(timelineAtomicKinds["timeline.update"])
	schema.Not = &jsonschema.Schema{Required: []string{"timeline_clip_id", "track_id"}}
	return schema
}

func (TimelineSplitInput) JSONSchema() *jsonschema.Schema {
	return timelineAtomicOpSchema(timelineAtomicKinds["timeline.split"])
}

func timelineAtomicOpSchema(kinds []string) *jsonschema.Schema {
	branches := make([]*jsonschema.Schema, 0, len(kinds))
	properties := jsonschema.NewProperties()
	fieldSemantics := map[string][]timelineAtomicFieldSemantic{}
	properties.Set("kind", &jsonschema.Schema{
		Type:        "string",
		Description: "要执行的单个原子操作类型；必须选择 oneOf 中一个 kind，并只提交该分支允许的字段",
	})
	for _, kind := range kinds {
		spec, exists := timeline.LookupOpSpec(kind)
		if !exists {
			continue
		}
		for _, field := range spec.Fields {
			if field.Injected || field.Generated {
				continue
			}
			semantics, description := mergeTimelineAtomicFieldSemantic(
				fieldSemantics[field.Name], kind, field.Desc,
			)
			fieldSemantics[field.Name] = semantics
			if existing, exists := properties.Get(field.Name); exists {
				existing.Description = description
				continue
			}
			fieldSchema := &jsonschema.Schema{
				Type:        timelineOpJSONType(field.Type),
				Description: description,
			}
			if field.Type == timeline.OpFieldStringArray {
				fieldSchema.Items = &jsonschema.Schema{Type: "string"}
			}
			properties.Set(field.Name, fieldSchema)
		}
		branches = append(branches, timelineAtomicOpBranchSchema(*spec))
	}
	return &jsonschema.Schema{
		Type:       "object",
		Properties: properties,
		OneOf:      branches,
	}
}

type timelineAtomicFieldSemantic struct {
	kinds       []string
	description string
}

func mergeTimelineAtomicFieldSemantic(
	current []timelineAtomicFieldSemantic,
	kind, description string,
) ([]timelineAtomicFieldSemantic, string) {
	for index, semantic := range current {
		if semantic.description == description {
			for _, existingKind := range semantic.kinds {
				if existingKind == kind {
					return current, timelineAtomicFieldDescription(current)
				}
			}
			current[index].kinds = append(current[index].kinds, kind)
			return current, timelineAtomicFieldDescription(current)
		}
	}
	current = append(current, timelineAtomicFieldSemantic{
		kinds: []string{kind}, description: description,
	})
	return current, timelineAtomicFieldDescription(current)
}

func timelineAtomicFieldDescription(semantics []timelineAtomicFieldSemantic) string {
	if len(semantics) == 0 {
		return ""
	}
	if len(semantics) == 1 {
		return semantics[0].description
	}
	parts := make([]string, 0, len(semantics))
	for _, semantic := range semantics {
		parts = append(parts, strings.Join(semantic.kinds, "/")+"："+semantic.description)
	}
	return "按 kind 解释：" + strings.Join(parts, "；")
}

// timelineAtomicOpBranchSchema 用 required + maxProperties 锁住无可选字段的
// kind；存在可选字段时再用 propertyNames pattern 声明精确白名单。字段类型仍在根层
// 只声明一次，避免 12 个 update 分支重复后突破模型 schema 预算。
func timelineAtomicOpBranchSchema(spec timeline.OpSpec) *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("kind", &jsonschema.Schema{
		Const:       spec.Kind,
		Description: spec.Summary,
	})
	branch := &jsonschema.Schema{
		Properties: properties,
		Required:   []string{"kind"},
	}
	allowedNames := []string{"kind"}
	for _, field := range spec.Fields {
		if field.Injected || field.Generated {
			continue
		}
		allowedNames = append(allowedNames, field.Name)
		if field.Required {
			branch.Required = append(branch.Required, field.Name)
		}
	}
	maxProperties := uint64(len(allowedNames))
	branch.MaxProperties = &maxProperties
	if len(branch.Required) < len(allowedNames) {
		escaped := make([]string, 0, len(allowedNames))
		for _, name := range allowedNames {
			escaped = append(escaped, regexp.QuoteMeta(name))
		}
		branch.PropertyNames = &jsonschema.Schema{
			Pattern: "^(?:" + strings.Join(escaped, "|") + ")$",
		}
	}
	if spec.Kind == "insert_clip" {
		branch.Properties.Set("track_id", &jsonschema.Schema{
			Type: "string",
			Enum: []any{"visual_base", "visual_overlay", "voiceover", "bgm", "sfx"},
		})
	}
	if len(spec.RequireAny) > 0 {
		choices := make([]*jsonschema.Schema, 0, len(spec.RequireAny))
		for _, name := range spec.RequireAny {
			choices = append(choices, &jsonschema.Schema{Required: []string{name}})
		}
		branch.AllOf = []*jsonschema.Schema{{AnyOf: choices}}
	}
	return branch
}

// TimelineAtomicOperation 校验工具与 kind 的归属，并返回一份独立 op map。
// 注入字段和服务端生成字段都不属于模型输入。
func TimelineAtomicOperation(toolName string, input any) (TimelineOp, error) {
	operation, err := timelineAtomicInputMap(toolName, input)
	if err != nil {
		return nil, err
	}
	if err := timeline.ValidateOpFields(operation); err != nil {
		return TimelineOp(operation), err
	}
	kind, _ := operation["kind"].(string)
	spec, _ := timeline.LookupOpSpec(kind)
	allowed := false
	for _, candidate := range timelineAtomicKinds[toolName] {
		if candidate == kind {
			allowed = true
			break
		}
	}
	if !allowed {
		owner, _ := TimelineAtomicToolForKind(kind)
		return TimelineOp(operation), &timeline.OpFieldError{
			Kind: kind, Field: "kind", Spec: spec,
			Reason: fmt.Sprintf("属于 %s，不属于 %s", owner, toolName),
		}
	}
	stableFields := map[string]bool{"kind": true}
	for _, field := range spec.Fields {
		if !field.Injected && !field.Generated {
			stableFields[field.Name] = true
		}
	}
	for name := range operation {
		if !stableFields[name] {
			return nil, fmt.Errorf("%s 的 Catalog op %s 不接受字段 %s", toolName, kind, name)
		}
	}
	if kind == "insert_clip" {
		trackID, _ := operation["track_id"].(string)
		if trackID == "" {
			trackID = "visual_base"
		}
		if !atomicInsertTrackAllowed(trackID) {
			return nil, fmt.Errorf("timeline.insert 的 insert_clip 不允许写入轨道 %s", trackID)
		}
	}
	cloned := make(TimelineOp, len(operation))
	for key, value := range operation {
		cloned[key] = cloneTimelineOpSchemaExample(value)
	}
	return cloned, nil
}

func TimelineAtomicToolForKind(kind string) (string, bool) {
	for toolName, kinds := range timelineAtomicKinds {
		for _, candidate := range kinds {
			if candidate == kind {
				return toolName, true
			}
		}
	}
	return "", false
}

func atomicTimelinePreflightFailure(toolName string, input any) (ToolResult, bool) {
	if _, atomic := timelineAtomicKinds[toolName]; !atomic {
		return ToolResult{}, false
	}
	operation, err := TimelineAtomicOperation(toolName, input)
	if err == nil {
		return ToolResult{}, false
	}
	var fieldErr *timeline.OpFieldError
	if errors.As(err, &fieldErr) {
		return TimelineAtomicFieldFailure(toolName, fieldErr, map[string]any(operation)), true
	}
	return ToolResult{
		Status:      string(StatusFailed),
		Observation: "原子时间线编辑输入不属于当前工具",
		Data: map[string]any{
			"reason":                     err.Error(),
			"current_timeline_unchanged": true,
			"recovery":                   "按当前工具 schema 只提交一个受支持的 kind；多个目标必须拆成多个工具调用。",
		},
	}, true
}

// TimelineAtomicFieldFailure is shared by Registry preflight and direct
// executor calls. Model invocations receive structured repair evidence without
// entering the executor; internal calls retain the same recovery contract.
func TimelineAtomicFieldFailure(
	toolName string,
	fieldErr *timeline.OpFieldError,
	operation map[string]any,
) ToolResult {
	data := map[string]any{
		"error_code":                 string(ErrCodeTimelineOpFieldError),
		"op_kind":                    fieldErr.Kind,
		"invalid_field":              fieldErr.Field,
		"failed_op":                  operation,
		"reason":                     fieldErr.Error(),
		"current_timeline_unchanged": true,
	}
	if fieldErr.Spec != nil {
		if expected := TimelineOpExpectedSchema(toolName, *fieldErr.Spec); expected != nil {
			data["expected_schema"] = expected
			data["correct_example"] = timeline.CorrectOpExample(*fieldErr.Spec)
			data["recovery"] = "只修正当前 op 的字段名与类型后重新调用；不要原样重发失败参数。"
		} else if owner, exists := TimelineAtomicToolForKind(fieldErr.Spec.Kind); exists {
			data["required_tool"] = owner
			data["op_catalog"] = timelineAtomicCatalogIndex(toolName)
			data["recovery"] = "该 kind 属于 required_tool；不要在当前工具中修补或重发。改用 required_tool，并按其 schema 补齐字段。"
		} else {
			data["op_catalog"] = timelineAtomicCatalogIndex(toolName)
			data["recovery"] = "从当前工具的 op_catalog 选择受支持的 kind，再按字段约定重新调用。"
		}
	} else {
		data["op_catalog"] = timelineAtomicCatalogIndex(toolName)
		data["recovery"] = "从 op_catalog 选择受支持的 kind，再按该操作的字段约定重新调用。"
	}
	return ToolResult{
		Status:      string(StatusFailed),
		Observation: "时间线补丁字段预校验失败：" + fieldErr.Error(),
		Data:        data,
	}
}

func timelineAtomicCatalogIndex(toolName string) []map[string]string {
	specs := TimelineAtomicCatalog(toolName)
	index := make([]map[string]string, 0, len(specs))
	for _, spec := range specs {
		index = append(index, map[string]string{
			"kind": spec.Kind, "summary": spec.Summary,
		})
	}
	return index
}

func atomicInsertTrackAllowed(trackID string) bool {
	switch trackID {
	case "visual_base", "visual_overlay", "voiceover", "bgm", "sfx":
		return true
	default:
		return false
	}
}

func timelineAtomicInputMap(toolName string, input any) (map[string]any, error) {
	switch typed := input.(type) {
	case TimelineInsertInput:
		if toolName == "timeline.insert" {
			return map[string]any(typed), nil
		}
	case TimelineDeleteInput:
		if toolName == "timeline.delete" {
			return map[string]any(typed), nil
		}
	case TimelineUpdateInput:
		if toolName == "timeline.update" {
			return map[string]any(typed), nil
		}
	case TimelineSplitInput:
		if toolName == "timeline.split" {
			return map[string]any(typed), nil
		}
	}
	return nil, fmt.Errorf("%s 输入类型不匹配: %T", toolName, input)
}

// TimelineOpExpectedSchema 为当前原子工具的失败恢复生成 expected_schema。
// 它只暴露该工具实际接受的正式模型字段，不包含兼容别名、注入字段或生成字段。
func TimelineOpExpectedSchema(toolName string, spec timeline.OpSpec) map[string]any {
	owner, exposed := TimelineAtomicToolForKind(spec.Kind)
	if !exposed || owner != toolName {
		return nil
	}
	correctExample := timeline.CorrectOpExample(spec)
	properties := map[string]any{
		"kind": map[string]any{
			"type": "string", "const": spec.Kind, "description": spec.Summary,
		},
	}
	required := []string{"kind"}
	for _, opField := range spec.Fields {
		if opField.Injected || opField.Generated {
			continue
		}
		field := map[string]any{
			"type": timelineOpJSONType(opField.Type), "description": opField.Desc,
		}
		if opField.Type == timeline.OpFieldStringArray {
			field["items"] = map[string]any{"type": "string"}
		}
		if example, exists := correctExample[opField.Name]; exists {
			field["examples"] = []any{example}
		}
		properties[opField.Name] = field
		if opField.Required {
			required = append(required, opField.Name)
		}
	}
	schema := map[string]any{
		"type": "object", "properties": properties,
		"required":             required,
		"additionalProperties": false,
	}
	if len(spec.RequireAny) > 0 {
		choices := make([]map[string]any, 0, len(spec.RequireAny))
		for _, name := range spec.RequireAny {
			choices = append(choices, map[string]any{"required": []string{name}})
		}
		schema["allOf"] = []map[string]any{{"anyOf": choices}}
	}
	return schema
}

// TimelineAtomicCatalog 返回当前原子工具可接受的 Catalog 子集。
func TimelineAtomicCatalog(toolName string) []timeline.OpSpec {
	kinds := timelineAtomicKinds[toolName]
	result := make([]timeline.OpSpec, 0, len(kinds))
	for _, kind := range kinds {
		if spec, exists := timeline.LookupOpSpec(kind); exists {
			result = append(result, *spec)
		}
	}
	return result
}

func cloneTimelineOpSchemaExample(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneTimelineOpSchemaExample(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneTimelineOpSchemaExample(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func timelineOpJSONType(fieldType timeline.OpFieldType) string {
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
		// Catalog 现有类型均命中上面的 case；未来新增未登记类型时仍产出合法 JSON type。
		return "string"
	}
}
