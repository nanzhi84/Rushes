// Package agentexec 承载与编排引擎解耦的视频领域执行算法与数据类型。
package agentexec

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
	"github.com/nanzhi84/Rushes/go/internal/understanding"
)

func canonicalContentPlanValue(input any) (map[string]any, error) {
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
	if limit <= 0 {
		return value
	}
	return TruncateRunes(value, limit)
}

// One frame is about 33ms at 30fps, below the perceptible threshold for beat alignment.
const beatSnapToleranceFrames = 1

func ContainsFrame(frames []int, target int) bool {
	index := sort.SearchInts(frames, target-beatSnapToleranceFrames)
	return index < len(frames) && frames[index] <= target+beatSnapToleranceFrames
}

func RandomID(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(data)
}

func BoolPointer(value bool) *bool { return &value }

func TruncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func IsReservedContextKey(key string) bool {
	switch key {
	case "timeline_version", "timeline_revision", "version", "timeline_id", "draft_id":
		return true
	default:
		return false
	}
}

func TimelineOpFailureAt(
	err error,
	operation map[string]any,
	failedIndex int,
	document timeline.Document,
) (rushestools.ToolResult, bool) {
	fieldErr, ok := TimelineOpFieldError(err)
	if ok {
		return timelineOpFieldFailure(fieldErr, operation, failedIndex), true
	}
	var semanticErr *timeline.SemanticError
	if !errors.As(err, &semanticErr) {
		return rushestools.ToolResult{}, false
	}
	data := map[string]any{
		"error_code":                 string(rushestools.ErrCodeTimelineOpSemanticError),
		"semantic_error_kind":        semanticErr.Kind,
		"failed_op":                  operation,
		"reason":                     semanticErr.Error(),
		"current_timeline_unchanged": true,
		"recovery":                   "根据当前时间线事实修正 failed_op 后重新调用；不要猜测 clip ID 或帧范围。",
	}
	if failedIndex > 0 {
		data["failed_op_index"] = failedIndex
	}
	if spec, exists := timeline.LookupOpSpec(InterfaceString(operation["kind"])); exists {
		data["expected_schema"] = rushestools.TimelineOpExpectedSchema(*spec)
		data["correct_example"] = timeline.CorrectOpExample(*spec)
	}
	switch semanticErr.Kind {
	case timeline.SemanticClipNotFound:
		data["available_timeline_clip_ids"] = timelineClipIDsByTrack(document)
	case timeline.SemanticFrameRange:
		data["actual_clip_range"] = map[string]any{
			"timeline_clip_id":     semanticErr.ClipID,
			"timeline_start_frame": semanticErr.TimelineStartFrame,
			"timeline_end_frame":   semanticErr.TimelineEndFrame,
			"source_start_frame":   semanticErr.SourceStartFrame,
			"source_end_frame":     semanticErr.SourceEndFrame,
			"provided_frame":       semanticErr.ProvidedFrame,
		}
	case timeline.SemanticTrackLocked:
		data["locked_track_id"] = semanticErr.TrackID
	}
	return rushestools.ToolResult{
		Status: string(rushestools.StatusFailed), Observation: "时间线补丁语义校验失败：" + semanticErr.Error(), Data: data,
	}, true
}

func CatalogSemanticTags(segments []understanding.Segment, limit int) []string {
	values := make([]string, 0, limit)
	seen := map[string]struct{}{}
	for _, segment := range segments {
		groups := [][]string{segment.Tags, segment.Subjects, segment.Actions, segment.Setting, segment.Mood}
		for _, group := range groups {
			for _, value := range group {
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				if _, duplicate := seen[value]; duplicate {
					continue
				}
				seen[value] = struct{}{}
				values = append(values, value)
				if len(values) >= limit {
					return values
				}
			}
		}
	}
	return values
}
