import { useQuery } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { api, type MaterialAsset, type MaterialSummarySegment } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import { StatusBadge } from "./StatusBadge";

type MaterialSummaryPanelProps = {
  projectId: string;
  asset: MaterialAsset | null;
  onClose: () => void;
};

export function MaterialSummaryPanel({
  projectId,
  asset,
  onClose
}: MaterialSummaryPanelProps): ReactElement | null {
  const ready = asset?.understanding_status === "ready";
  const summaryQuery = useQuery({
    queryKey: asset ? queryKeys.materialSummary(projectId, asset.asset_id) : ["material-summary", "none"],
    queryFn: () => api.getAssetSummary(projectId, asset!.asset_id),
    enabled: Boolean(asset) && ready
  });

  if (!asset) {
    return null;
  }

  const summary = summaryQuery.data?.summary;
  const segments = summary?.segments ?? [];

  return (
    <section className="rounded-lg border border-[#d9dee7] bg-white p-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="text-sm font-medium text-[#64748b]">素材理解摘要</p>
          <h2 className="mt-1 text-lg font-semibold text-[#17202a]">
            {asset.filename || asset.asset_id}
          </h2>
        </div>
        <button
          className="rounded-md border border-[#cbd5e1] px-2 py-1 text-xs hover:bg-[#f1f5f9]"
          type="button"
          onClick={onClose}
        >
          关闭
        </button>
      </header>

      {!ready ? (
        <p className="mt-3 text-sm text-[#64748b]">该素材尚未完成理解，暂无摘要。</p>
      ) : summaryQuery.isLoading ? (
        <p className="mt-3 text-sm text-[#64748b]">正在读取摘要</p>
      ) : summaryQuery.error ? (
        <p className="mt-3 rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">摘要加载失败</p>
      ) : summary ? (
        <div className="mt-3 space-y-4">
          <div className="flex flex-wrap items-center gap-2">
            {summary.semantic_role ? (
              <StatusBadge label={roleLabel(summary.semantic_role)} tone="info" />
            ) : null}
            {summary.language ? (
              <span className="text-xs text-[#64748b]">语言：{summary.language}</span>
            ) : null}
          </div>
          {summary.overall ? (
            <p className="text-sm leading-6 text-[#334155]">{summary.overall}</p>
          ) : null}
          {segments.length > 0 ? (
            <div className="overflow-x-auto rounded-lg border border-[#e2e8f0]">
              <table className="min-w-[640px] w-full text-left text-sm">
                <thead className="border-b border-[#e2e8f0] bg-[#f8fafc] text-xs font-semibold text-[#475569]">
                  <tr>
                    <th className="px-3 py-2">时间段</th>
                    <th className="px-3 py-2">描述</th>
                    <th className="px-3 py-2">质量</th>
                    <th className="px-3 py-2">标签</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-[#eef2f7]">
                  {segments.map((segment, index) => (
                    <tr key={`${segment.start_s}-${segment.end_s}-${index}`}>
                      <td className="whitespace-nowrap px-3 py-2 font-mono text-xs text-[#334155]">
                        {formatTimecode(segment.start_s)} - {formatTimecode(segment.end_s)}
                      </td>
                      <td className="px-3 py-2 text-[#334155]">{segment.description}</td>
                      <td className="px-3 py-2">
                        <StatusBadge label={qualityLabel(segment.quality)} tone={qualityTone(segment.quality)} />
                      </td>
                      <td className="px-3 py-2 text-xs text-[#64748b]">{formatTags(segment)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="text-sm text-[#64748b]">该摘要没有分段信息。</p>
          )}
        </div>
      ) : (
        <p className="mt-3 text-sm text-[#64748b]">暂无摘要内容。</p>
      )}
    </section>
  );
}

function formatTags(segment: MaterialSummarySegment): string {
  const tags = segment.tags ?? [];
  return tags.length > 0 ? tags.join("、") : "—";
}

function formatTimecode(seconds: number): string {
  if (!Number.isFinite(seconds)) {
    return "—";
  }
  const total = Math.max(0, seconds);
  const minutes = Math.floor(total / 60)
    .toString()
    .padStart(2, "0");
  const rest = (total % 60).toFixed(1).padStart(4, "0");
  return `${minutes}:${rest}`;
}

function roleLabel(role: string): string {
  const labels: Record<string, string> = {
    speech_footage: "有声素材",
    footage: "画面素材",
    music: "音乐",
    voiceover: "旁白",
    ambient: "环境音",
    photo: "照片",
    font: "字体",
    other: "其他"
  };
  return labels[role] ?? role;
}

function qualityLabel(quality: string): string {
  const labels: Record<string, string> = {
    good: "优质",
    usable: "可用",
    avoid: "避免"
  };
  return labels[quality] ?? quality;
}

function qualityTone(quality: string): "success" | "warning" | "danger" | "neutral" {
  if (quality === "good") {
    return "success";
  }
  if (quality === "usable") {
    return "warning";
  }
  if (quality === "avoid") {
    return "danger";
  }
  return "neutral";
}
