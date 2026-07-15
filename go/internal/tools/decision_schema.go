package tools

import "github.com/eino-contrib/jsonschema"

// JSONSchema makes the cross-field answer requirement visible to the model.
// Runtime validation still checks the selected decision's state and policy.
func (DecisionAnswerInput) JSONSchema() *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("decision_id", &jsonschema.Schema{Type: "string", Description: "已有待答决策的 decision_id；不能回答本回合由 interaction.ask_user 刚创建的决策，必须等待真实用户"})
	properties.Set("option_id", &jsonschema.Schema{Type: "string", Description: "从该决策 options 中选择的 option_id；与用户自由文本至少提供一项"})
	properties.Set("free_text", &jsonschema.Schema{Type: "string", Description: "用户明确提供的自由文本答案；不得由模型代替用户编造"})
	properties.Set("payload", &jsonschema.Schema{Type: "object", Description: "可选结构化补充数据；仅透传真实用户或受信任上游已给出的字段"})
	return &jsonschema.Schema{
		Type:       "object",
		Properties: properties,
		Required:   []string{"decision_id"},
		AnyOf: []*jsonschema.Schema{
			{Required: []string{"option_id"}},
			{Required: []string{"free_text"}},
		},
		AdditionalProperties: jsonschema.FalseSchema,
	}
}
