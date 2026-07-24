package agentexec

import (
	"context"
	"encoding/json"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

// PreviewAlreadyInspected 只读取原子 preview.check 的工具 trace，用于去重重复
// render_preview 终态通知；它不执行任何媒体检查，也不代替模型组合检查结果。
func (exec *Executor) PreviewAlreadyInspected(ctx context.Context, draftID string, result any) bool {
	resultMap, _ := result.(map[string]any)
	previewID := InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = InterfaceString(resultMap["preview_id"])
	}
	if previewID == "" {
		return false
	}
	messages, err := storage.ListMessages(ctx, exec.database.Read(), draftID, 200)
	if err != nil {
		return false
	}
	completed := map[string]bool{}
	observed := map[string]bool{}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Kind != "tool" {
			continue
		}
		var record struct {
			Tool         string `json:"tool"`
			PreviewID    string `json:"preview_id"`
			PreviewCheck string `json:"preview_check"`
			ArgsSummary  string `json:"args_summary"`
			Status       string `json:"status"`
		}
		if json.Unmarshal([]byte(messages[index].Content), &record) != nil ||
			record.Tool != "preview.check" {
			continue
		}
		var args struct {
			PreviewID string `json:"preview_id"`
			Check     string `json:"check"`
		}
		_ = json.Unmarshal([]byte(record.ArgsSummary), &args)
		if record.PreviewID == "" {
			record.PreviewID = args.PreviewID
		}
		if record.PreviewCheck == "" {
			record.PreviewCheck = args.Check
		}
		if record.PreviewID != previewID || record.PreviewCheck == "" || observed[record.PreviewCheck] {
			continue
		}
		observed[record.PreviewCheck] = true
		completed[record.PreviewCheck] = record.Status == "succeeded"
	}
	for _, required := range []string{"decode", "black", "freeze", "silence", "loudness"} {
		if !completed[required] {
			return false
		}
	}
	return true
}
