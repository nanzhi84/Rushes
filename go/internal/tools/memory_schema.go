package tools

import "github.com/eino-contrib/jsonschema"

// JSONSchema mirrors the runtime limits so the model cannot produce a shape
// that is schema-valid but guaranteed to fail memory.update validation.
func (MemoryUpdateInput) JSONSchema() *jsonschema.Schema {
	keySchema := &jsonschema.Schema{
		Type: "string", Pattern: "^[a-z0-9_]{2,40}$",
		MinLength: uint64Pointer(2), MaxLength: uint64Pointer(40),
		Description: "稳定语义主键，例如 pacing、subtitle_style；同键覆盖旧记忆",
	}
	entryProperties := jsonschema.NewProperties()
	entryProperties.Set("key", keySchema)
	entryProperties.Set("kind", &jsonschema.Schema{
		Type: "string", Enum: []any{"preference", "correction", "habit"},
		Description: "preference 长期偏好，correction 用户纠正，habit 稳定使用习惯",
	})
	entryProperties.Set("statement", &jsonschema.Schema{
		Type: "string", MinLength: uint64Pointer(1), MaxLength: uint64Pointer(200),
		Description: "一句简体中文陈述用户当前明确表达的跨项目稳定偏好、纠正或习惯；不得写模型判断",
	})
	entrySchema := &jsonschema.Schema{
		Type: "object", Properties: entryProperties,
		Required:             []string{"key", "kind", "statement"},
		AdditionalProperties: jsonschema.FalseSchema,
	}
	properties := jsonschema.NewProperties()
	properties.Set("entries", &jsonschema.Schema{
		Type: "array", Items: entrySchema,
		MinItems: uint64Pointer(1), MaxItems: uint64Pointer(8), UniqueItems: true,
		Description: "要写入或覆盖的长期记忆；一次性草稿要求不要入库",
	})
	properties.Set("remove_keys", &jsonschema.Schema{
		Type: "array", Items: keySchema,
		MinItems: uint64Pointer(1), MaxItems: uint64Pointer(50), UniqueItems: true,
		Description: "用户当前明确要求忘记的长期记忆键；不得与 entries 中的 key 重复",
	})
	return &jsonschema.Schema{
		Type: "object", Properties: properties,
		AnyOf: []*jsonschema.Schema{
			{Required: []string{"entries"}},
			{Required: []string{"remove_keys"}},
		},
		AdditionalProperties: jsonschema.FalseSchema,
	}
}

func uint64Pointer(value uint64) *uint64 { return &value }
