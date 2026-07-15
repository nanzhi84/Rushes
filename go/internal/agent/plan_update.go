package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

const contentPlanRuneLimit = 8000

func (service *Service) toolPlanUpdate(
	ctx context.Context,
	draftID string,
	input rushestools.PlanUpdateInput,
) (rushestools.ToolResult, error) {
	if input.Plan == nil {
		return planUpdateFailure("plan.update 缺少 plan 对象", map[string]any{
			"reason": "plan_required",
		}), nil
	}
	patch, err := canonicalContentPlan(input.Plan)
	if err != nil {
		return planUpdateFailure("创作计划本只能包含可编码为 JSON 的内容", map[string]any{
			"reason": "plan_not_json",
		}), nil
	}
	if key := reservedContentPlanKey(patch); key != "" {
		return planUpdateFailure(
			fmt.Sprintf("创作计划本不能使用保留键 %q；该键属于 WorldState 客观状态", key),
			map[string]any{"reason": "reserved_key", "reserved_key": key},
		), nil
	}

	draft, err := storage.GetDraft(ctx, service.database.Read(), draftID)
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	reset := input.Reset != nil && *input.Reset
	updated := mergeContentPlan(nil, patch)
	mode := "reset"
	if !reset {
		updated = mergeContentPlan(draft.ContentPlan, patch)
		mode = "merge"
	}
	if key := reservedContentPlanKey(updated); key != "" {
		return planUpdateFailure(
			fmt.Sprintf("现有创作计划本含保留键 %q；请用 reset=true 写入不含保留键的干净计划", key),
			map[string]any{"reason": "stored_reserved_key", "reserved_key": key},
		), nil
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return planUpdateFailure("创作计划本只能包含可编码为 JSON 的内容", map[string]any{
			"reason": "plan_not_json",
		}), nil
	}
	runes := utf8.RuneCount(encoded)
	if runes > contentPlanRuneLimit {
		return planUpdateFailure(
			"创作计划本超出 8000 字上限；请只记纲要，细节留在对应工具按需检索",
			map[string]any{
				"reason": "plan_too_large", "plan_runes": runes,
				"limit_runes": contentPlanRuneLimit, "current_plan_unchanged": true,
			},
		), nil
	}

	result, err := reducer.Apply(ctx, service.database, nil, reducer.Options{
		Actor: contracts.ActorAgent,
		ResultRows: reducer.ResultRows{DraftPlanUpdate: &reducer.DraftPlanUpdateRow{
			DraftID: draftID, ContentPlan: updated,
		}},
	})
	if err != nil {
		return rushestools.ToolResult{}, err
	}
	if result.Status != reducer.StatusApplied {
		return rushestools.ToolResult{}, fmt.Errorf("创作计划本写入状态异常: %s", result.Status)
	}
	return rushestools.ToolResult{
		Status:      "succeeded",
		Observation: "已更新持久创作计划本；下一个用户回合重建 WorldState 后会从 draft.content_plan 读取最新内容",
		Data: map[string]any{
			"mode": mode, "plan_runes": runes,
		},
	}, nil
}

func canonicalContentPlan(input map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var canonical map[string]any
	if err := json.Unmarshal(encoded, &canonical); err != nil {
		return nil, err
	}
	if canonical == nil {
		canonical = map[string]any{}
	}
	return canonical, nil
}

// mergeContentPlan implements the object branch of RFC 7396 without mutating
// either input. A null patch value deletes a key; objects merge recursively;
// arrays and scalar values replace the previous value.
func mergeContentPlan(target, patch map[string]any) map[string]any {
	result := cloneContentPlanMap(target)
	for key, patchValue := range patch {
		if patchValue == nil {
			delete(result, key)
			continue
		}
		patchObject, isObject := patchValue.(map[string]any)
		if !isObject {
			result[key] = cloneContentPlanValue(patchValue)
			continue
		}
		targetObject, _ := result[key].(map[string]any)
		result[key] = mergeContentPlan(targetObject, patchObject)
	}
	return result
}

func cloneContentPlanMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = cloneContentPlanValue(value)
	}
	return result
}

func cloneContentPlanValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneContentPlanMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneContentPlanValue(item)
		}
		return result
	default:
		return typed
	}
}

func reservedContentPlanKey(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if isReservedContextKey(key) {
				return key
			}
			if nested := reservedContentPlanKey(typed[key]); nested != "" {
				return nested
			}
		}
	case []any:
		for _, item := range typed {
			if nested := reservedContentPlanKey(item); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func planUpdateFailure(observation string, data map[string]any) rushestools.ToolResult {
	if reason, _ := data["reason"].(string); reason != "" {
		data["error_code"] = reason
	}
	return rushestools.ToolResult{Status: "failed", Observation: observation, Data: data}
}
