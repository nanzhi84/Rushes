package tools

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
)

// TestForwardAndRecoverySchemasShareCatalogDerivation 对拍前向 oneOf schema 与失败恢复
// expected_schema（#95 T4）：两者都从 timelineOpBranchPlanFor 派生，必须在结构上完全一致
// ——属性集合、required 集合、anyOf 必填组三项零 diff。任一渲染器漏字段、改分组或与 Catalog
// 脱节都会让本测试变红，坐实「单一派生、双向同源」。前向/恢复各自的格式差异（title、
// additionalProperties 形态、别名描述、示例）由各自的 golden 单独锁定，不在本对拍范围内。
func TestForwardAndRecoverySchemasShareCatalogDerivation(t *testing.T) {
	t.Parallel()
	for _, spec := range timeline.Catalog {
		// 两侧都经 JSON 归一，抹平前向 *jsonschema.Schema 与恢复原生 map 的 Go 类型差异，
		// 只比较结构（属性/required/anyOf），格式差异各自 golden 单独锁定。
		forward := marshalToMap(t, timelineOpBranchSchema(spec))
		recovery := marshalToMap(t, TimelineOpExpectedSchema(spec))

		if got, want := schemaPropertyNames(forward), schemaPropertyNames(recovery); !equalStringSets(got, want) {
			t.Errorf("%s 属性集合前向/恢复不一致: forward=%v recovery=%v", spec.Kind, got, want)
		}
		if got, want := schemaRequired(forward), schemaRequired(recovery); !equalStringSets(got, want) {
			t.Errorf("%s required 前向/恢复不一致: forward=%v recovery=%v", spec.Kind, got, want)
		}
		if got, want := schemaAnyOfGroups(forward), schemaAnyOfGroups(recovery); !equalGroupSets(got, want) {
			t.Errorf("%s anyOf 组前向/恢复不一致: forward=%v recovery=%v", spec.Kind, got, want)
		}
	}
}

func marshalToMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return result
}

func schemaPropertyNames(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	return names
}

func schemaRequired(schema map[string]any) []string {
	return anyStringSlice(schema["required"])
}

// schemaAnyOfGroups 把 allOf 里每个 anyOf(required) 约束归一成排序后的名字组，便于集合比较。
func schemaAnyOfGroups(schema map[string]any) [][]string {
	allOf, _ := schema["allOf"].([]any)
	groups := make([][]string, 0, len(allOf))
	for _, entry := range allOf {
		entryMap, _ := entry.(map[string]any)
		anyOf, _ := entryMap["anyOf"].([]any)
		group := make([]string, 0, len(anyOf))
		for _, choice := range anyOf {
			choiceMap, _ := choice.(map[string]any)
			group = append(group, anyStringSlice(choiceMap["required"])...)
		}
		sort.Strings(group)
		groups = append(groups, group)
	}
	return groups
}

// anyStringSlice 同时接受 []string（恢复 map）与 []any（前向 JSON 回解）。
func anyStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				result = append(result, name)
			}
		}
		return result
	default:
		return nil
	}
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, name := range a {
		seen[name]++
	}
	for _, name := range b {
		seen[name]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func equalGroupSets(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(groups [][]string) []string {
		encoded := make([]string, 0, len(groups))
		for _, group := range groups {
			sorted := append([]string(nil), group...)
			sort.Strings(sorted)
			data, _ := json.Marshal(sorted)
			encoded = append(encoded, string(data))
		}
		sort.Strings(encoded)
		return encoded
	}
	aKeys, bKeys := key(a), key(b)
	for index := range aKeys {
		if aKeys[index] != bKeys[index] {
			return false
		}
	}
	return true
}
