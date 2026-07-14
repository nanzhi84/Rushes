export type PreviewCoverLayout = { width: "100%" } | { height: "100%" };

// 视觉素材使用 cover 语义：宽素材铺满高度、窄素材铺满宽度，超出的另一轴裁掉。
// 这样横屏工程不会因为素材比例略有差异而在左右留下黑边。
export function previewCoverLayout(
  sourceWidth: number,
  sourceHeight: number,
  compositionWidth: number,
  compositionHeight: number
): PreviewCoverLayout {
  const sourceRatio = sourceWidth > 0 && sourceHeight > 0 ? sourceWidth / sourceHeight : 16 / 9;
  const compositionRatio = compositionWidth / compositionHeight;
  return sourceRatio >= compositionRatio ? { height: "100%" } : { width: "100%" };
}
