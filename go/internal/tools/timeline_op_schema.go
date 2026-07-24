package tools

import (
	"fmt"

	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// TimelineOp 是 timeline.apply_patches harness 路径使用的单个语义补丁。
//
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
	"timeline.delete": {"delete_clip", "delete_range", "remove_track_clips"},
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
	return timelineAtomicOpSchema("插入一个 clip 或一条字幕", timelineAtomicKinds["timeline.insert"])
}

func (TimelineDeleteInput) JSONSchema() *jsonschema.Schema {
	return timelineAtomicOpSchema("删除一个 clip、一个连续范围或一条非主视觉轨的内容", timelineAtomicKinds["timeline.delete"])
}

func (TimelineUpdateInput) JSONSchema() *jsonschema.Schema {
	schema := timelineAtomicOpSchema("只更新一个 clip、track 或 subtitle 目标", timelineAtomicKinds["timeline.update"])
	schema.Not = &jsonschema.Schema{Required: []string{"timeline_clip_id", "track_id"}}
	return schema
}

func (TimelineSplitInput) JSONSchema() *jsonschema.Schema {
	return timelineAtomicOpSchema("在一个时间线帧位置切分一个 clip", timelineAtomicKinds["timeline.split"])
}

func timelineAtomicOpSchema(description string, kinds []string) *jsonschema.Schema {
	branches := make([]*jsonschema.Schema, 0, len(kinds))
	properties := jsonschema.NewProperties()
	kindValues := make([]any, 0, len(kinds))
	properties.Set("kind", &jsonschema.Schema{Type: "string"})
	for _, kind := range kinds {
		spec, exists := timeline.LookupOpSpec(kind)
		if !exists {
			continue
		}
		kindValues = append(kindValues, kind)
		for _, field := range spec.Fields {
			if field.Injected || field.Generated {
				continue
			}
			if _, exists := properties.Get(field.Name); exists {
				continue
			}
			fieldSchema := &jsonschema.Schema{Type: timelineOpJSONType(field.Type)}
			if field.Type == timeline.OpFieldStringArray {
				fieldSchema.Items = &jsonschema.Schema{Type: "string"}
			}
			properties.Set(field.Name, fieldSchema)
		}
		branches = append(branches, timelineAtomicOpBranchSchema(*spec))
	}
	kindSchema, _ := properties.Get("kind")
	kindSchema.Enum = kindValues
	return &jsonschema.Schema{
		Type:                 "object",
		Description:          description + "；kind 决定唯一 Catalog op",
		Properties:           properties,
		Required:             []string{"kind"},
		AdditionalProperties: jsonschema.FalseSchema,
		OneOf:                branches,
	}
}

// timelineAtomicOpBranchSchema 不复用旧批量工具的教学型 schema（title、分支摘要、
// 完整示例和兼容别名），只保留 provider 约束执行所需的信息。字段类型在根级只声明
// 一次；每个分支只声明 kind const 与 required。额外组合由同源 Catalog 在 Registry
// 解码阶段、进入 executor 前拒绝，不为 12 个 update 分支重复字段白名单。
func timelineAtomicOpBranchSchema(spec timeline.OpSpec) *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("kind", &jsonschema.Schema{Type: "string", Const: spec.Kind})
	branch := &jsonschema.Schema{
		Properties: properties,
		Required:   []string{"kind"},
	}
	for _, field := range spec.Fields {
		if field.Injected || field.Generated {
			continue
		}
		if field.Required {
			branch.Required = append(branch.Required, field.Name)
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
	if err := validateTimelineOp(operation); err != nil {
		return nil, err
	}
	kind, _ := operation["kind"].(string)
	allowed := false
	for _, candidate := range timelineAtomicKinds[toolName] {
		if candidate == kind {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("%s 不接受 Catalog op %s", toolName, kind)
	}
	spec, _ := timeline.LookupOpSpec(kind)
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

// JSONSchema 为 Eino 的参数反射提供 Catalog 驱动的主动 schema。
// 必须使用值接收者：jsonschema v1.0.3 通过非指针类型的方法集识别自定义 schema。
func (TimelineOp) JSONSchema() *jsonschema.Schema {
	branches := make([]*jsonschema.Schema, 0, len(timeline.Catalog))
	for _, spec := range timeline.Catalog {
		branches = append(branches, timelineOpBranchSchema(spec))
	}
	return &jsonschema.Schema{
		Type:        "object",
		Description: "从 oneOf 选择一种扁平时间线语义补丁；kind 与字段由同一 Catalog 约束",
		OneOf:       branches,
	}
}

// —— 单一 Catalog→schema 派生（#95 T4）——
//
// timelineOpBranchPlan 把「遍历非注入字段、展开别名、归组 required / anyOf」这套结构逻辑
// 收敛为一次遍历。前向 oneOf schema（*jsonschema.Schema，模型可见）与失败恢复 expected_schema
// （map[string]any，回灌模型的纠错提示）两个渲染器都从同一个 plan 生成，不再各写一份同构遍历。
// 两侧输出的格式差异（前向带 title/branch example/别名 example，恢复 additionalProperties:false、
// 别名描述后缀且不带 example）由各自的渲染器保留，逐字不变。

type timelineOpProperty struct {
	name     string
	jsonType string
	desc     string // 基础描述；别名前缀/后缀由各渲染器套用
	isArray  bool
	aliasFor string // "" 表示主字段，否则为所对应的主字段名
	example  any    // 原始 field.Example（可能为 nil）；前向渲染器使用
}

type timelineOpBranchPlan struct {
	kind        string
	summary     string
	properties  []timelineOpProperty // 顺序：每个字段先主字段、再其别名
	required    []string             // 无别名的必填字段，按字段顺序
	anyOfGroups [][]string           // 别名组（字段顺序）后接 RequireAny 组
}

func timelineOpBranchPlanFor(spec timeline.OpSpec) timelineOpBranchPlan {
	plan := timelineOpBranchPlan{kind: spec.Kind, summary: spec.Summary}
	for _, field := range spec.Fields {
		if field.Injected {
			continue
		}
		jsonType := timelineOpJSONType(field.Type)
		isArray := field.Type == timeline.OpFieldStringArray
		plan.properties = append(plan.properties, timelineOpProperty{
			name: field.Name, jsonType: jsonType, desc: field.Desc,
			isArray: isArray, aliasFor: "", example: field.Example,
		})
		for _, alias := range field.Aliases {
			plan.properties = append(plan.properties, timelineOpProperty{
				name: alias, jsonType: jsonType, desc: field.Desc,
				isArray: isArray, aliasFor: field.Name, example: field.Example,
			})
		}
		if !field.Required {
			continue
		}
		if len(field.Aliases) == 0 {
			plan.required = append(plan.required, field.Name)
			continue
		}
		plan.anyOfGroups = append(plan.anyOfGroups, append([]string{field.Name}, field.Aliases...))
	}
	if len(spec.RequireAny) > 0 {
		plan.anyOfGroups = append(plan.anyOfGroups, append([]string(nil), spec.RequireAny...))
	}
	return plan
}

// timelineOpBranchSchema 是前向 oneOf 分支渲染器：把 plan 编译成模型可见的 *jsonschema.Schema。
func timelineOpBranchSchema(spec timeline.OpSpec) *jsonschema.Schema {
	plan := timelineOpBranchPlanFor(spec)
	properties := jsonschema.NewProperties()
	properties.Set("kind", &jsonschema.Schema{
		Type:        "string",
		Const:       plan.kind,
		Description: plan.summary,
	})
	branch := &jsonschema.Schema{
		Type:                 "object",
		Title:                plan.kind,
		Description:          plan.summary,
		Properties:           properties,
		Required:             append([]string{"kind"}, plan.required...),
		AdditionalProperties: jsonschema.FalseSchema,
		Examples:             []any{timeline.CorrectOpExample(spec)},
	}
	for _, property := range plan.properties {
		properties.Set(property.name, timelineOpForwardFieldSchema(property))
	}
	for _, group := range plan.anyOfGroups {
		alternatives := make([]*jsonschema.Schema, 0, len(group))
		for _, name := range group {
			alternatives = append(alternatives, &jsonschema.Schema{Required: []string{name}})
		}
		branch.AllOf = append(branch.AllOf, &jsonschema.Schema{AnyOf: alternatives})
	}
	return branch
}

func timelineOpForwardFieldSchema(property timelineOpProperty) *jsonschema.Schema {
	description := property.desc
	if property.aliasFor != "" {
		description = "兼容别名，对应 " + property.aliasFor + "；" + description
	}
	schema := &jsonschema.Schema{
		Type:        property.jsonType,
		Description: description,
	}
	if property.example != nil {
		schema.Examples = []any{cloneTimelineOpSchemaExample(property.example)}
	}
	if property.isArray {
		schema.Items = &jsonschema.Schema{Type: "string"}
	}
	return schema
}

// TimelineOpExpectedSchema 是失败恢复渲染器：把同一个 plan 渲染成 expected_schema map，
// 供 apply_patches 字段/语义失败时回灌模型。与前向 schema 同源于 timelineOpBranchPlanFor，
// 因此新增 op 字段只需改 timeline.Catalog 一处。
func TimelineOpExpectedSchema(spec timeline.OpSpec) map[string]any {
	plan := timelineOpBranchPlanFor(spec)
	// 示例值沿用 Catalog 的正确示例并逐次深拷贝，保持与前向一致、且回灌 map 不暴露可变引用。
	correctExample := timeline.CorrectOpExample(spec)
	properties := map[string]any{
		"kind": map[string]any{
			"type": "string", "const": plan.kind, "description": plan.summary,
		},
	}
	for _, property := range plan.properties {
		if property.aliasFor != "" {
			properties[property.name] = map[string]any{
				"type":        property.jsonType,
				"description": property.desc + "（兼容别名，对应 " + property.aliasFor + "）",
			}
			continue
		}
		field := map[string]any{"type": property.jsonType, "description": property.desc}
		if property.isArray {
			field["items"] = map[string]any{"type": "string"}
		}
		if example, exists := correctExample[property.name]; exists {
			field["examples"] = []any{example}
		}
		properties[property.name] = field
	}
	schema := map[string]any{
		"type": "object", "properties": properties,
		"required":             append([]string{"kind"}, plan.required...),
		"additionalProperties": false,
	}
	if len(plan.anyOfGroups) > 0 {
		allOf := make([]map[string]any, 0, len(plan.anyOfGroups))
		for _, group := range plan.anyOfGroups {
			choices := make([]map[string]any, 0, len(group))
			for _, name := range group {
				choices = append(choices, map[string]any{"required": []string{name}})
			}
			allOf = append(allOf, map[string]any{"anyOf": choices})
		}
		schema["allOf"] = allOf
	}
	return schema
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
		// 保留恢复侧旧兜底：未知字段类型回退 "string"。Catalog 现有类型均命中上面的
		// case，此分支不影响任何 golden 字节，只是让未来新增未登记类型时仍产出合法 JSON type。
		return "string"
	}
}
