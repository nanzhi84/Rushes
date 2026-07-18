package agentexec

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nanzhi84/Rushes/go/internal/storage"
	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// PreviewVerification 是引擎侧 job 续跑对 render_preview 领域质检的单一入口:
// 若已质检过返回 skip=true;否则生成自动质检报告(失败降级),报告落库/格式化仍归引擎。
func (exec *Executor) PreviewVerification(ctx context.Context, draftID string, details any) (bool, map[string]any) {
	if exec.PreviewAlreadyInspected(ctx, draftID, details) {
		return true, nil
	}
	report, reportErr := exec.PreviewVerificationReport(ctx, draftID, details)
	if reportErr != nil {
		slog.Warn("预览自动质检失败", "draft_id", draftID, "error", reportErr)
		report = degradedPreviewVerificationReport(details)
	}
	return false, report
}

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
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Kind != "tool" {
			continue
		}
		var record struct {
			Tool        string `json:"tool"`
			PreviewID   string `json:"preview_id"`
			ArgsSummary string `json:"args_summary"`
			Status      string `json:"status"`
		}
		if json.Unmarshal([]byte(messages[index].Content), &record) != nil ||
			record.Tool != "render.inspect_preview" || record.Status != "succeeded" {
			continue
		}
		if record.PreviewID == previewID {
			return true
		}
		// Pre-D3 traces only stored preview_id inside an untruncated args_summary.
		// New traces use the top-level field and never depend on this compatibility path.
		if record.PreviewID == "" {
			var legacyArgs struct {
				PreviewID string `json:"preview_id"`
			}
			if json.Unmarshal([]byte(record.ArgsSummary), &legacyArgs) == nil && legacyArgs.PreviewID == previewID {
				return true
			}
		}
	}
	return false
}

func (exec *Executor) PreviewVerificationReport(
	ctx context.Context,
	draftID string,
	result any,
) (map[string]any, error) {
	resultMap, _ := result.(map[string]any)
	previewID := InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = InterfaceString(resultMap["preview_id"])
	}
	if previewID == "" {
		return nil, nil
	}
	inspection, err := exec.ToolInspectPreview(ctx, draftID, rushestools.RenderInspectInput{PreviewID: previewID})
	if err != nil {
		return nil, err
	}
	var timelineVersion int
	if err := exec.database.Read().QueryRowContext(ctx,
		"SELECT timeline_version FROM previews WHERE preview_id=? AND draft_id=?", previewID, draftID,
	).Scan(&timelineVersion); err != nil {
		return nil, err
	}
	document, err := timeline.Get(ctx, exec.database, draftID, timelineVersion)
	if err != nil {
		return nil, err
	}
	contractReport, hasContract, err := exec.VerifyContentContract(ctx, draftID, document)
	if err != nil {
		return nil, err
	}
	report := map[string]any{
		"preview_id":        previewID,
		"timeline_version":  timelineVersion,
		"render_inspection": inspection,
	}
	if hasContract {
		report["content_contract"] = contractReport
	}
	return report, nil
}

func degradedPreviewVerificationReport(result any) map[string]any {
	resultMap, _ := result.(map[string]any)
	previewID := InterfaceString(resultMap["artifact_id"])
	if previewID == "" {
		previewID = InterfaceString(resultMap["preview_id"])
	}
	issue := map[string]any{
		"check":      "inspection",
		"severity":   "warning",
		"error_code": "preview_inspection_unavailable",
		"message":    "自动质检暂不可用，请稍后重试。",
	}
	return map[string]any{
		"preview_id": previewID,
		"degraded":   true,
		"issues":     []map[string]any{issue},
	}
}
