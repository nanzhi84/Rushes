import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import type { ReactElement } from "react";
import { api, type MaterialAsset } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import { FsBrowserDialog } from "./FsBrowserDialog";
import { StatusBadge, understandingBadgeProps } from "./StatusBadge";
import { useMaterialsEvents } from "./useMaterialsEvents";

type AssetsPanelProps = {
  projectId: string;
  /** 点击素材瓦片时触发，工作台用来在预览区试看。 */
  onPreviewAsset?: (asset: MaterialAsset) => void;
  previewingAssetId?: string | null;
  enableEvents?: boolean;
};

/** Case 工作台右上的素材面板：网格瓦片 + 单一本地导入入口。 */
export function AssetsPanel({
  projectId,
  onPreviewAsset,
  previewingAssetId = null,
  enableEvents = true
}: AssetsPanelProps): ReactElement {
  const queryClient = useQueryClient();
  const [browserOpen, setBrowserOpen] = useState(false);

  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(projectId),
    queryFn: () => api.listMaterials(projectId),
    refetchInterval: 5_000
  });
  useMaterialsEvents(projectId, enableEvents);

  const importLocal = useMutation({
    mutationFn: (path: string) =>
      api.importLocalMaterial(projectId, { path, storage_mode: "reference" }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.materials(projectId) });
    }
  });

  const assets = materialsQuery.data?.assets ?? [];

  return (
    <section className="flex h-full min-h-0 flex-col" aria-label="素材面板">
      <header className="flex shrink-0 items-center justify-between gap-2 border-b border-line px-3 py-2">
        <span className="text-sm font-semibold text-fg">
          素材 <span className="font-normal text-fg-muted">{assets.length}</span>
        </span>
        <button
          className="rounded-md bg-raised px-2.5 py-1.5 text-xs font-medium text-fg hover:bg-hover disabled:opacity-40"
          type="button"
          disabled={importLocal.isPending}
          onClick={() => setBrowserOpen(true)}
        >
          {importLocal.isPending ? "导入中…" : "＋ 本地导入"}
        </button>
      </header>

      <div className="min-h-0 flex-1 overflow-y-auto p-3">
        {materialsQuery.isLoading ? (
          <p className="text-sm text-fg-muted">正在读取素材</p>
        ) : materialsQuery.error ? (
          <p className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
            素材列表加载失败
          </p>
        ) : assets.length === 0 ? (
          <button
            className="grid w-full place-items-center rounded-lg border border-dashed border-line-strong px-4 py-10 text-center text-sm text-fg-muted hover:border-accent"
            type="button"
            onClick={() => setBrowserOpen(true)}
          >
            还没有素材，点击本地导入。
          </button>
        ) : (
          <div className="grid grid-cols-2 gap-2 xl:grid-cols-3">
            {assets.map((asset) => (
              <AssetTile
                key={asset.asset_id}
                asset={asset}
                active={previewingAssetId === asset.asset_id}
                onClick={onPreviewAsset ? () => onPreviewAsset(asset) : undefined}
              />
            ))}
          </div>
        )}
        {importLocal.error ? (
          <p className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            导入失败，请检查路径是否可访问。
          </p>
        ) : null}
      </div>

      <FsBrowserDialog
        open={browserOpen}
        title="本地导入素材"
        submitLabel="导入此路径"
        onClose={() => setBrowserOpen(false)}
        onSelect={(path) => {
          setBrowserOpen(false);
          importLocal.mutate(path);
        }}
      />
    </section>
  );
}

function AssetTile({
  asset,
  active,
  onClick
}: {
  asset: MaterialAsset;
  active: boolean;
  onClick?: () => void;
}): ReactElement {
  const [thumbFailed, setThumbFailed] = useState(false);
  const understanding = understandingBadgeProps(asset.understanding_status);
  return (
    <button
      className={`group overflow-hidden rounded-md border text-left transition-colors ${
        active ? "border-accent" : "border-line hover:border-line-strong"
      } ${asset.enabled && asset.usable ? "" : "opacity-50"}`}
      type="button"
      title={asset.filename || asset.asset_id}
      onClick={onClick}
    >
      <div className="relative aspect-video bg-ink">
        {asset.thumbnail_ready && !thumbFailed ? (
          <img
            src={api.mediaThumbnailUrl(asset.asset_id)}
            alt={`${asset.filename || asset.asset_id} 缩略图`}
            className="h-full w-full object-cover"
            loading="lazy"
            onError={() => setThumbFailed(true)}
          />
        ) : (
          <div className="grid h-full w-full place-items-center text-xs text-fg-faint">
            {kindLabel(asset.kind)}
          </div>
        )}
        {asset.duration_sec !== null ? (
          <span className="absolute bottom-1 right-1 rounded bg-black/70 px-1 py-0.5 text-[10px] tabular-nums text-white">
            {formatDuration(asset.duration_sec)}
          </span>
        ) : null}
        {asset.invalid ? (
          <span className="absolute left-1 top-1">
            <StatusBadge label="失效" tone="danger" />
          </span>
        ) : null}
      </div>
      <div className="flex items-center justify-between gap-1 px-1.5 py-1">
        <span className="truncate text-[11px] text-fg-muted">
          {asset.filename || asset.asset_id}
        </span>
        <span
          aria-label={`理解状态：${understanding.label}`}
          className={`h-1.5 w-1.5 shrink-0 rounded-full ${understandingDotClass(asset.understanding_status)}`}
          title={understanding.label}
        />
      </div>
    </button>
  );
}

function understandingDotClass(status: string): string {
  if (status === "ready") {
    return "bg-ok";
  }
  if (status === "running") {
    return "bg-info";
  }
  if (status === "failed") {
    return "bg-danger";
  }
  return "bg-fg-faint";
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

function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const minutes = Math.floor(total / 60)
    .toString()
    .padStart(2, "0");
  const rest = (total % 60).toString().padStart(2, "0");
  return `${minutes}:${rest}`;
}
