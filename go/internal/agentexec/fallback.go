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

// FallbackMainline 是无模型密钥兜底下的确定性「混剪主线」:列可用视觉素材 → 逐个理解
// → 原子插入初版时间线 → 起预览。领域编排归领域包,引擎侧只保留关键词委托与非域兜底。
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
	understandIDs := []string{}
	for _, asset := range visualAssets {
		if asset.UnderstandingStatus != "ready" {
			understandIDs = append(understandIDs, asset.AssetID)
		}
	}
	if len(understandIDs) > 0 {
		for _, assetID := range understandIDs {
			output, err := runReported(ctx, "media.detect_shots", rushestools.DetectShotsInput{
				AssetID: assetID, Depth: "scan", Focus: "混剪可用画面",
			})
			if err != nil {
				return "", err
			}
			if err := requireFallbackToolStatus(
				"media.detect_shots", output,
				"completed", string(rushestools.StatusQueued),
			); err != nil {
				return "", err
			}
		}
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
	document, err := timeline.Latest(ctx, exec.database, draftID)
	if err != nil {
		return "", err
	}
	output, err := runReported(ctx, "render.start", rushestools.RenderStartInput{
		Kind: "preview", TimelineID: document.TimelineID,
	})
	if err != nil {
		return "", err
	}
	if err := requireFallbackToolStatus(
		"render.start", output,
		string(rushestools.StatusQueued), string(rushestools.StatusSucceeded),
	); err != nil {
		return "", err
	}
	return "已完成素材理解与初版时间线，并开始渲染预览。", nil
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
