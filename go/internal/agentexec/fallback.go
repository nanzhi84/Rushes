package agentexec

import (
	"context"
	"fmt"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// RunReportedFunc 由引擎注入:执行一个工具并发出 tool_step started/finished SSE。
// fallback 主线用它保持与引擎侧完全一致的上报行为(领域只管编排序列,上报仍归引擎)。
type RunReportedFunc func(ctx context.Context, name string, input any) (any, error)

// FallbackMainline 是无模型密钥兜底下的确定性「混剪编辑主线」:Harness 读取
// 可用视觉素材并逐个原子插入初版时间线。它不自行检查或生成预览；调用方在
// 明确交付边界统一进入自动 Preview QA，避免保留第二套预览编排路径。
func (exec *Executor) FallbackMainline(ctx context.Context, draftID string, runReported RunReportedFunc) (string, error) {
	listed, err := exec.ToolListAssets(ctx, draftID, rushestools.AssetListInput{OnlyUsable: BoolPointer(true)})
	if err != nil {
		return "", err
	}
	visualAssets := make([]rushestools.AssetManifest, 0, len(listed.Assets))
	for _, asset := range listed.Assets {
		if asset.Kind == "video" || asset.Kind == "image" {
			visualAssets = append(visualAssets, asset)
		}
	}
	if len(visualAssets) == 0 {
		return "当前草稿还没有可用的视频或图片素材，请先导入素材。", nil
	}
	for _, asset := range visualAssets {
		endFrame := asset.DurationFrames
		if endFrame <= 0 {
			endFrame = timeline.DefaultFPS
		}
		endFrame = min(endFrame, 5*timeline.DefaultFPS)
		output, err := runReported(ctx, "timeline.insert", rushestools.TimelineInsertInput{
			"kind": "insert_clip", "asset_id": asset.AssetID, "role": "b_roll",
			"source_start_frame": 0, "source_end_frame": endFrame,
		})
		if err != nil {
			return "", err
		}
		if err := requireFallbackToolStatus(
			"timeline.insert", output, string(rushestools.StatusSucceeded),
		); err != nil {
			return "", err
		}
	}
	return "已完成初版时间线；基础镜头索引由后台独立建立。", nil
}

func requireFallbackToolStatus(
	name string,
	output any,
	allowed ...string,
) error {
	var status, observation string
	switch result := output.(type) {
	case rushestools.ToolResult:
		status, observation = result.Status, result.Observation
	case rushestools.DetectShotsResult:
		status, observation = result.Status, result.UsageNote
	default:
		return fmt.Errorf("%s 返回类型无效", name)
	}
	for _, allowedStatus := range allowed {
		if status == allowedStatus {
			return nil
		}
	}
	return fmt.Errorf("%s 未完成: status=%s observation=%s", name, status, observation)
}
