import { useEffect, useState } from "react";
import type { ReactElement } from "react";
import { api, type MaterialAsset } from "../../api/client";

type AssetMediaPreviewProps = {
  asset: MaterialAsset;
  className?: string;
};

// 播放源档位：先原片（导入即刻可播，浏览器硬解 H.264/HEVC），播不动再回落 proxy（若已转码）。
type PlaybackSource = "source" | "proxy";

/**
 * 素材试看：对齐剪映体感——默认直连原片，浏览器解不动的格式（如 ProRes）触发 onError 时
 * 回落 proxy（若就绪），proxy 还没转好则给「转码中」轻提示。图片仍走原图直连。
 */
export function AssetMediaPreview({ asset, className }: AssetMediaPreviewProps): ReactElement {
  const [source, setSource] = useState<PlaybackSource>("source");
  const [failed, setFailed] = useState(false);

  // 换素材时回到「原片优先」并清掉失败态。
  useEffect(() => {
    setSource("source");
    setFailed(false);
  }, [asset.asset_id]);

  if (asset.kind === "image") {
    return (
      <img
        src={api.mediaSourceUrl(asset.asset_id)}
        alt={asset.filename || asset.asset_id}
        className={className ?? "max-h-full max-w-full object-contain"}
      />
    );
  }

  if (asset.kind !== "video" && asset.kind !== "audio") {
    return <PreviewNotice text="该素材类型不支持试看。" />;
  }

  // 原片播不动时回落：proxy 就绪换 proxy，否则说明还在转码。
  const handleError = (): void => {
    if (source === "source" && asset.proxy_ready) {
      setSource("proxy");
      return;
    }
    setFailed(true);
  };

  if (failed) {
    // 可播格式已不再生成代理（性能专项），「转码中」只在确有代理任务在跑时说。
    const proxyJobActive = (asset.jobs ?? []).some(
      (job) => job.kind === "proxy" && (job.status === "pending" || job.status === "running"),
    );
    return (
      <PreviewNotice
        text={
          asset.proxy_ready
            ? "该素材暂时无法预览。"
            : proxyJobActive
              ? "转码中，稍候可预览。"
              : "此素材格式暂不支持预览。"
        }
      />
    );
  }

  const src =
    source === "proxy" ? api.mediaProxyUrl(asset.asset_id) : api.mediaSourceUrl(asset.asset_id);
  const label = `${asset.filename || asset.asset_id} ${asset.kind === "audio" ? "音频" : "视频"}试看`;

  // key 绑档位：切 source→proxy 时强制重挂媒体元素，清掉上一档的解码错误态。
  if (asset.kind === "audio") {
    return (
      <audio
        key={source}
        src={src}
        controls
        onError={handleError}
        className={className ?? "w-full"}
        aria-label={label}
      />
    );
  }
  return (
    <video
      key={source}
      src={src}
      controls
      playsInline
      onError={handleError}
      className={className ?? "max-h-full max-w-full"}
      aria-label={label}
    />
  );
}

function PreviewNotice({ text }: { text: string }): ReactElement {
  return (
    <div className="grid h-full w-full place-items-center p-4 text-center text-sm text-fg-muted">
      {text}
    </div>
  );
}
