package agentexec

import (
	"context"

	"github.com/nanzhi84/Rushes/go/internal/timeline"
	rushestools "github.com/nanzhi84/Rushes/go/internal/tools"
)

// RunReportedFunc 由引擎注入:执行一个工具并发出 tool_step started/finished SSE。
// fallback 主线用它保持与引擎侧完全一致的上报行为(领域只管编排序列,上报仍归引擎)。
type RunReportedFunc func(ctx context.Context, name string, input any) (any, error)

// FallbackMainline 是无模型密钥兜底下的确定性「混剪主线」:列可用视觉素材 → 逐个理解
// → 组装初版时间线 → 起预览。领域编排归领域包,引擎侧只保留关键词委托与非域兜底。
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
			if _, err := runReported(ctx, "media.detect_shots", rushestools.DetectShotsInput{
				AssetID: assetID, Depth: "scan", Focus: "混剪可用画面",
			}); err != nil {
				return "", err
			}
		}
	}
	clips := make([]rushestools.ComposeClip, 0, len(visualAssets))
	for _, asset := range visualAssets {
		endFrame := asset.DurationFrames
		if endFrame <= 0 {
			endFrame = timeline.DefaultFPS
		}
		endFrame = min(endFrame, 5*timeline.DefaultFPS)
		clips = append(clips, rushestools.ComposeClip{
			AssetID: asset.AssetID, SourceStartFrame: 0, SourceEndFrame: endFrame, Role: "b_roll",
		})
	}
	if _, err := runReported(ctx, "timeline.compose_initial", rushestools.ComposeInitialInput{Clips: clips}); err != nil {
		return "", err
	}
	if _, err := runReported(ctx, "render.preview", rushestools.RenderPreviewInput{}); err != nil {
		return "", err
	}
	return "已完成素材理解与初版时间线，并开始渲染预览。", nil
}
