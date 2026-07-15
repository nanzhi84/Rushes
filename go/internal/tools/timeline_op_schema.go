package tools

import (
	"github.com/eino-contrib/jsonschema"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// TimelineOp 是 timeline.apply_patch 面向模型的单个语义补丁。
//
// 运行时仍保留 map 语义；JSONSchema 则把 timeline.Catalog 编译成 oneOf，
// 让模型只看到所选 kind 的合法字段，并隐藏由服务端注入的字段。
type TimelineOp map[string]any

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

func timelineOpBranchSchema(spec timeline.OpSpec) *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("kind", &jsonschema.Schema{
		Type:        "string",
		Const:       spec.Kind,
		Description: spec.Summary,
	})
	branch := &jsonschema.Schema{
		Type:                 "object",
		Title:                spec.Kind,
		Description:          spec.Summary,
		Properties:           properties,
		Required:             []string{"kind"},
		AdditionalProperties: jsonschema.FalseSchema,
		Examples:             []any{timeline.CorrectOpExample(spec)},
	}

	for _, field := range spec.Fields {
		if field.Injected {
			continue
		}
		properties.Set(field.Name, timelineOpFieldSchema(field, ""))
		for _, alias := range field.Aliases {
			properties.Set(alias, timelineOpFieldSchema(field, field.Name))
		}
		if !field.Required {
			continue
		}
		if len(field.Aliases) == 0 {
			branch.Required = append(branch.Required, field.Name)
			continue
		}
		alternatives := make([]*jsonschema.Schema, 0, len(field.Aliases)+1)
		alternatives = append(alternatives, &jsonschema.Schema{Required: []string{field.Name}})
		for _, alias := range field.Aliases {
			alternatives = append(alternatives, &jsonschema.Schema{Required: []string{alias}})
		}
		branch.AllOf = append(branch.AllOf, &jsonschema.Schema{AnyOf: alternatives})
	}
	if len(spec.RequireAny) > 0 {
		alternatives := make([]*jsonschema.Schema, 0, len(spec.RequireAny))
		for _, name := range spec.RequireAny {
			alternatives = append(alternatives, &jsonschema.Schema{Required: []string{name}})
		}
		branch.AllOf = append(branch.AllOf, &jsonschema.Schema{AnyOf: alternatives})
	}
	return branch
}

func timelineOpFieldSchema(field timeline.OpField, aliasFor string) *jsonschema.Schema {
	description := field.Desc
	if aliasFor != "" {
		description = "兼容别名，对应 " + aliasFor + "；" + description
	}
	schema := &jsonschema.Schema{
		Type:        timelineOpJSONType(field.Type),
		Description: description,
	}
	if field.Example != nil {
		schema.Examples = []any{cloneTimelineOpSchemaExample(field.Example)}
	}
	if field.Type == timeline.OpFieldStringArray {
		schema.Items = &jsonschema.Schema{Type: "string"}
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
		return ""
	}
}
