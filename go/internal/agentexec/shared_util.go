// Package agentexec 承载与编排引擎解耦的视频领域执行算法与数据类型。
package agentexec

import (
	"encoding/json"
	"sort"
	"strings"
)

func CanonicalContentPlanValue(input any) (map[string]any, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func NumericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}

func InterfaceString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case *string:
		if typed != nil {
			return *typed
		}
	}
	return ""
}

func TruncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// One frame is about 33ms at 30fps, below the perceptible threshold for beat alignment.
const beatSnapToleranceFrames = 1

func ContainsFrame(frames []int, target int) bool {
	index := sort.SearchInts(frames, target-beatSnapToleranceFrames)
	return index < len(frames) && frames[index] <= target+beatSnapToleranceFrames
}
