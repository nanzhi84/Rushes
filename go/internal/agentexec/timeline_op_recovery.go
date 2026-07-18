package agentexec

import (
	"errors"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

func timelineOpFieldFailure(
	fieldErr *timeline.OpFieldError,
	operation map[string]any,
	failedIndex int,
) rushestools.ToolResult {
	data := map[string]any{
		"error_code":                 "timeline_op_field_error",
		"op_kind":                    fieldErr.Kind,
		"invalid_field":              fieldErr.Field,
		"reason":                     fieldErr.Error(),
		"current_timeline_unchanged": true,
	}
	if failedIndex > 0 {
		data["failed_op_index"] = failedIndex
		data["failed_op"] = operation
	}
	if fieldErr.Spec != nil {
		data["expected_schema"] = timelineOpExpectedSchema(*fieldErr.Spec)
		data["correct_example"] = timeline.CorrectOpExample(*fieldErr.Spec)
		data["recovery"] = "只修正当前 op 的字段名与类型后重新调用；不要原样重发失败参数。"
	} else {
		data["op_catalog"] = timelineOpCatalogIndex()
		data["recovery"] = "从 op_catalog 选择受支持的 kind，再按该操作的字段约定重新调用。"
	}
	return rushestools.ToolResult{
		Status:      "failed",
		Observation: "时间线补丁字段预校验失败：" + fieldErr.Error(),
		Data:        data,
	}
}

func timelineOpExpectedSchema(spec timeline.OpSpec) map[string]any {
	correctExample := timeline.CorrectOpExample(spec)
	properties := map[string]any{
		"kind": map[string]any{
			"type": "string", "const": spec.Kind, "description": spec.Summary,
		},
	}
	required := []string{"kind"}
	aliasRequirements := make([]map[string]any, 0)
	for _, field := range spec.Fields {
		if field.Injected {
			continue
		}
		property := map[string]any{
			"type":        timelineOpJSONType(field.Type),
			"description": field.Desc,
		}
		if field.Type == "string_array" {
			property["items"] = map[string]any{"type": "string"}
		}
		if example, exists := correctExample[field.Name]; exists {
			property["examples"] = []any{example}
		}
		properties[field.Name] = property
		for _, alias := range field.Aliases {
			properties[alias] = map[string]any{
				"type":        timelineOpJSONType(field.Type),
				"description": field.Desc + "（兼容别名，对应 " + field.Name + "）",
			}
		}
		if !field.Required {
			continue
		}
		if len(field.Aliases) == 0 {
			required = append(required, field.Name)
			continue
		}
		choices := make([]map[string]any, 0, len(field.Aliases)+1)
		for _, name := range append([]string{field.Name}, field.Aliases...) {
			choices = append(choices, map[string]any{"required": []string{name}})
		}
		aliasRequirements = append(aliasRequirements, map[string]any{"anyOf": choices})
	}
	if len(spec.RequireAny) > 0 {
		choices := make([]map[string]any, 0, len(spec.RequireAny))
		for _, name := range spec.RequireAny {
			choices = append(choices, map[string]any{"required": []string{name}})
		}
		aliasRequirements = append(aliasRequirements, map[string]any{"anyOf": choices})
	}
	schema := map[string]any{
		"type": "object", "properties": properties, "required": required,
		"additionalProperties": false,
	}
	if len(aliasRequirements) > 0 {
		schema["allOf"] = aliasRequirements
	}
	return schema
}

func timelineOpJSONType(fieldType string) string {
	switch fieldType {
	case "integer", "number", "boolean", "object":
		return fieldType
	case "string_array":
		return "array"
	default:
		return "string"
	}
}

func timelineOpCatalogIndex() []map[string]string {
	index := make([]map[string]string, 0, len(timeline.Catalog))
	for _, spec := range timeline.Catalog {
		index = append(index, map[string]string{
			"kind": spec.Kind, "summary": spec.Summary,
		})
	}
	return index
}

func timelineOpFieldError(err error) (*timeline.OpFieldError, bool) {
	var fieldErr *timeline.OpFieldError
	if errors.As(err, &fieldErr) {
		return fieldErr, true
	}
	return nil, false
}

func timelineClipIDsByTrack(document timeline.Document) map[string][]string {
	result := make(map[string][]string, len(document.Tracks))
	for _, track := range document.Tracks {
		ids := make([]string, 0, len(track.Clips))
		for _, clip := range track.Clips {
			ids = append(ids, clip.TimelineClipID)
		}
		result[track.TrackID] = ids
	}
	return result
}
