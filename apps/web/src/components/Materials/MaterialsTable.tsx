import type { ReactElement } from "react";
import { api, type MaterialAsset } from "../../api/client";
import { StatusBadge, understandingBadgeProps } from "./StatusBadge";

type MaterialsTableProps = {
  assets: MaterialAsset[];
  actionPending: boolean;
  activeAssetId?: string | null;
  onSelect?: (asset: MaterialAsset) => void;
  onRelocate: (asset: MaterialAsset) => void;
  onToggleEnabled: (asset: MaterialAsset) => void;
  onUnlink: (asset: MaterialAsset) => void;
};

export function MaterialsTable({
  assets,
  actionPending,
  activeAssetId,
  onSelect,
  onRelocate,
  onToggleEnabled,
  onUnlink
}: MaterialsTableProps): ReactElement {
  if (assets.length === 0) {
    return (
      <p className="rounded-lg border border-dashed border-[#cbd5e1] bg-white px-4 py-8 text-sm text-[#64748b]">
        还没有素材。可以上传文件、从本机路径 reference 导入，或从 URL 创建导入确认项。
      </p>
    );
  }

  return (
    <div className="overflow-x-auto rounded-lg border border-[#d9dee7] bg-white">
      <table className="min-w-[1120px] w-full text-left text-sm">
        <thead className="border-b border-[#d9dee7] bg-[#f8fafc] text-xs font-semibold text-[#475569]">
          <tr>
            <th className="px-3 py-3">缩略图</th>
            <th className="px-3 py-3">文件名</th>
            <th className="px-3 py-3">类型</th>
            <th className="px-3 py-3">时长</th>
            <th className="px-3 py-3">理解状态</th>
            <th className="px-3 py-3">可用</th>
            <th className="px-3 py-3">运行中任务</th>
            <th className="px-3 py-3">操作</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-[#eef2f7]">
          {assets.map((asset) => {
            const understanding = understandingBadgeProps(asset.understanding_status);
            const active = activeAssetId === asset.asset_id;
            const rowTone = active ? "bg-[#eff6ff]" : asset.invalid ? "bg-[#fff7ed]" : "bg-white";
            return (
              <tr
                key={asset.asset_id}
                className={`${rowTone} ${onSelect ? "cursor-pointer hover:bg-[#f8fafc]" : ""}`}
                onClick={onSelect ? () => onSelect(asset) : undefined}
              >
                <td className="px-3 py-3 align-top">{thumbnailCell(asset)}</td>
                <td className="max-w-[240px] px-3 py-3 align-top">
                  <div className="font-medium text-[#17202a]">{asset.filename || asset.asset_id}</div>
                  <div className="mt-1 truncate text-xs text-[#64748b]" title={asset.asset_id}>
                    {asset.asset_id}
                  </div>
                  {asset.invalid ? (
                    <div className="mt-2">
                      <StatusBadge label="源文件失效" tone="danger" />
                    </div>
                  ) : null}
                </td>
                <td className="px-3 py-3 align-top">
                  <div>{kindLabel(asset.kind)}</div>
                  <div className="mt-1 text-xs text-[#64748b]">{storageLabel(asset.storage_mode)}</div>
                </td>
                <td className="px-3 py-3 align-top text-[#334155]">
                  <div>{formatDuration(asset.duration_sec)}</div>
                  {resolutionLabel(asset) ? (
                    <div className="mt-1 text-xs text-[#64748b]">{resolutionLabel(asset)}</div>
                  ) : null}
                </td>
                <td className="px-3 py-3 align-top">
                  <StatusBadge label={understanding.label} tone={understanding.tone} />
                </td>
                <td className="px-3 py-3 align-top">
                  <StatusBadge label={asset.usable ? "可用" : "不可用"} tone={asset.usable ? "success" : "danger"} />
                  <div className="mt-1 text-xs text-[#64748b]">{asset.enabled ? "已启用" : "已禁用"}</div>
                </td>
                <td className="min-w-[150px] px-3 py-3 align-top">{jobSummary(asset)}</td>
                <td
                  className="min-w-[220px] px-3 py-3 align-top"
                  onClick={(event) => event.stopPropagation()}
                >
                  <div className="flex flex-wrap gap-2">
                    <button
                      className="rounded-md border border-[#cbd5e1] px-2 py-1 text-xs hover:bg-[#f1f5f9] disabled:text-[#94a3b8]"
                      type="button"
                      disabled={actionPending}
                      onClick={() => onToggleEnabled(asset)}
                    >
                      {asset.enabled ? "禁用" : "启用"}
                    </button>
                    <button
                      className="rounded-md border border-[#cbd5e1] px-2 py-1 text-xs text-[#b42318] hover:bg-[#fee4e2] disabled:text-[#94a3b8]"
                      type="button"
                      disabled={actionPending}
                      onClick={() => onUnlink(asset)}
                    >
                      删除引用
                    </button>
                    {asset.invalid ? (
                      <button
                        className="rounded-md bg-[#17202a] px-2 py-1 text-xs font-medium text-white disabled:bg-[#94a3b8]"
                        type="button"
                        disabled={actionPending}
                        onClick={() => onRelocate(asset)}
                      >
                        重新定位
                      </button>
                    ) : null}
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function thumbnailCell(asset: MaterialAsset): ReactElement {
  if (asset.thumbnail_ready) {
    return (
      <img
        src={api.mediaThumbnailUrl(asset.asset_id)}
        alt={`${asset.filename || asset.asset_id} 缩略图`}
        className="h-12 w-20 rounded border border-[#e2e8f0] object-cover"
        loading="lazy"
      />
    );
  }
  return (
    <div className="flex h-12 w-20 items-center justify-center rounded border border-dashed border-[#cbd5e1] bg-[#f8fafc] text-xs text-[#94a3b8]">
      {kindLabel(asset.kind)}
    </div>
  );
}

function kindLabel(kind: string): string {
  const labels: Record<string, string> = {
    video: "视频",
    audio: "音频",
    image: "图片",
    font: "字体"
  };
  return labels[kind] ?? kind;
}

function storageLabel(mode: string): string {
  return mode === "reference" ? "reference" : mode === "copy" ? "copy" : mode;
}

function resolutionLabel(asset: MaterialAsset): string | null {
  const probe = asset.probe;
  if (!probe) {
    return null;
  }
  const width = numberValue(probe.width);
  const height = numberValue(probe.height);
  return width !== null && height !== null ? `${width}x${height}` : null;
}

function numberValue(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

// duration_sec → mm:ss；缺省显示占位符。
function formatDuration(seconds: number | null): string {
  if (seconds === null || !Number.isFinite(seconds)) {
    return "—";
  }
  const total = Math.max(0, Math.round(seconds));
  const minutes = Math.floor(total / 60)
    .toString()
    .padStart(2, "0");
  const rest = (total % 60).toString().padStart(2, "0");
  return `${minutes}:${rest}`;
}

function jobSummary(asset: MaterialAsset): ReactElement {
  const active = asset.jobs.find((job) => ["pending", "running"].includes(job.status));
  if (!active) {
    return <span className="text-xs text-[#94a3b8]">无运行中任务</span>;
  }
  const progress = typeof active.progress === "number" ? Math.max(0, Math.min(1, active.progress)) : null;
  return (
    <div className="space-y-1">
      <div className="text-xs text-[#334155]">
        {active.kind} / {active.status}
      </div>
      <div className="h-1.5 rounded bg-[#e2e8f0]">
        <div
          className="h-1.5 rounded bg-[#2563eb]"
          style={{ width: `${progress === null ? 15 : Math.round(progress * 100)}%` }}
        />
      </div>
      <div className="text-xs text-[#64748b]">{progress === null ? "等待进度" : `${Math.round(progress * 100)}%`}</div>
    </div>
  );
}
