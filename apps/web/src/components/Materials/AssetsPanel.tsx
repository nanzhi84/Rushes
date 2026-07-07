import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type MaterialAsset } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import { FsBrowserDialog } from "./FsBrowserDialog";
import { MaterialSummaryPanel } from "./MaterialSummaryPanel";
import { StatusBadge, understandingBadgeProps } from "./StatusBadge";
import { useMaterialsEvents } from "./useMaterialsEvents";

type AssetsPanelProps = {
  projectId: string;
  /** 点击素材瓦片时触发，工作台用来在预览区试看。 */
  onPreviewAsset?: (asset: MaterialAsset) => void;
  previewingAssetId?: string | null;
  enableEvents?: boolean;
  /** 管理模式（项目详情素材 tab）：瓦片菜单 + 摘要详情 + 失效重检。 */
  management?: boolean;
  gridClassName?: string;
};

/** 素材面板：文件夹分组网格 + 单一「本地导入」入口；工作台与项目详情共用。 */
export function AssetsPanel({
  projectId,
  onPreviewAsset,
  previewingAssetId = null,
  enableEvents = true,
  management = false,
  gridClassName
}: AssetsPanelProps): ReactElement {
  const queryClient = useQueryClient();
  const [browserOpen, setBrowserOpen] = useState(false);
  const [currentDir, setCurrentDir] = useState("");
  const [skippedFiles, setSkippedFiles] = useState<string[]>([]);
  const [activeAssetId, setActiveAssetId] = useState<string | null>(null);
  const [relocatingAsset, setRelocatingAsset] = useState<MaterialAsset | null>(null);

  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(projectId),
    queryFn: () => api.listMaterials(projectId),
    refetchInterval: 5_000
  });
  useMaterialsEvents(projectId, enableEvents);

  const invalidateMaterials = async (): Promise<void> => {
    await queryClient.invalidateQueries({ queryKey: queryKeys.materials(projectId) });
  };

  const importLocal = useMutation({
    mutationFn: (paths: string[]) =>
      api.importLocalMaterial(projectId, { paths, storage_mode: "reference" }),
    onSuccess: async (response) => {
      setSkippedFiles(response.skipped ?? []);
      await invalidateMaterials();
    }
  });

  const unlinkMaterial = useMutation({
    mutationFn: (asset: MaterialAsset) =>
      api.unlinkMaterial(projectId, { asset_id: asset.asset_id }),
    onSuccess: invalidateMaterials
  });

  const patchMaterial = useMutation({
    mutationFn: ({
      asset,
      payload
    }: {
      asset: MaterialAsset;
      payload: { enabled?: boolean; reference_path?: string };
    }) => api.patchMaterial(projectId, asset.asset_id, payload),
    onSuccess: async () => {
      setRelocatingAsset(null);
      await invalidateMaterials();
    }
  });

  const revalidateMaterials = useMutation({
    mutationFn: () => api.revalidateMaterials(projectId),
    onSuccess: (response) => {
      queryClient.setQueryData(queryKeys.materials(projectId), response);
    }
  });

  const assets = materialsQuery.data?.assets ?? [];
  const folders = useMemo(() => foldersAt(assets, currentDir), [assets, currentDir]);
  const currentAssets = useMemo(() => assetsAt(assets, currentDir), [assets, currentDir]);
  // 从最新列表按 id 反查，保证摘要面板跟随理解状态刷新。
  const activeAsset = activeAssetId
    ? (assets.find((asset) => asset.asset_id === activeAssetId) ?? null)
    : null;
  const actionPending =
    importLocal.isPending ||
    unlinkMaterial.isPending ||
    patchMaterial.isPending ||
    revalidateMaterials.isPending;

  return (
    <section className="flex h-full min-h-0 flex-col" aria-label="素材面板">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b border-line px-3 py-2">
        <span className="text-sm font-semibold text-fg">
          素材 <span className="font-normal text-fg-muted">{assets.length}</span>
        </span>
        <div className="flex items-center gap-2">
          {management ? (
            <button
              className="rounded-md border border-line px-2.5 py-1.5 text-xs text-fg-muted hover:bg-hover disabled:opacity-40"
              type="button"
              disabled={revalidateMaterials.isPending}
              onClick={() => revalidateMaterials.mutate()}
            >
              重新检测失效
            </button>
          ) : null}
          <button
            className="rounded-md bg-raised px-2.5 py-1.5 text-xs font-medium text-fg hover:bg-hover disabled:opacity-40"
            type="button"
            disabled={importLocal.isPending}
            onClick={() => setBrowserOpen(true)}
          >
            {importLocal.isPending ? "导入中…" : "＋ 本地导入"}
          </button>
        </div>
      </header>

      {currentDir !== "" ? (
        <nav
          className="flex shrink-0 flex-wrap items-center gap-1 border-b border-line px-3 py-1.5 text-xs"
          aria-label="素材文件夹路径"
        >
          <button
            className="rounded px-1.5 py-0.5 text-fg-muted hover:bg-hover hover:text-fg"
            type="button"
            onClick={() => setCurrentDir("")}
          >
            全部素材
          </button>
          {breadcrumbSegments(currentDir).map((segment) => (
            <span key={segment.path} className="flex items-center gap-1">
              <span className="text-fg-faint">/</span>
              <button
                className="rounded px-1.5 py-0.5 text-fg-muted hover:bg-hover hover:text-fg"
                type="button"
                onClick={() => setCurrentDir(segment.path)}
              >
                {segment.name}
              </button>
            </span>
          ))}
        </nav>
      ) : null}

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
            还没有素材，点击本地导入。可以直接选整个素材文件夹。
          </button>
        ) : (
          <div className={gridClassName ?? "grid grid-cols-2 gap-2 xl:grid-cols-3"}>
            {folders.map((folder) => (
              <FolderTile
                key={folder.path}
                folder={folder}
                onOpen={() => setCurrentDir(folder.path)}
              />
            ))}
            {currentAssets.map((asset) => (
              <AssetTile
                key={asset.asset_id}
                asset={asset}
                active={previewingAssetId === asset.asset_id || activeAssetId === asset.asset_id}
                management={management}
                actionPending={actionPending}
                onClick={
                  management
                    ? () => setActiveAssetId(asset.asset_id)
                    : onPreviewAsset
                      ? () => onPreviewAsset(asset)
                      : undefined
                }
                onToggleEnabled={() =>
                  patchMaterial.mutate({ asset, payload: { enabled: !asset.enabled } })
                }
                onRelocate={() => setRelocatingAsset(asset)}
                onUnlink={() => {
                  if (window.confirm(`删除素材引用：${asset.filename || asset.asset_id}？`)) {
                    unlinkMaterial.mutate(asset);
                  }
                }}
              />
            ))}
          </div>
        )}
        {skippedFiles.length > 0 ? (
          <p className="mt-3 rounded-md border border-warn/40 bg-warn/10 px-3 py-2 text-xs text-warn">
            已跳过 {skippedFiles.length} 个不支持的文件：{skippedFiles.slice(0, 5).join("、")}
            {skippedFiles.length > 5 ? " 等" : ""}
          </p>
        ) : null}
        {importLocal.error ? (
          <p className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            导入失败，请检查路径是否可访问。
          </p>
        ) : null}
      </div>

      {management && activeAsset ? (
        <div className="max-h-[45%] shrink-0 overflow-y-auto border-t border-line p-3">
          <MaterialSummaryPanel
            projectId={projectId}
            asset={activeAsset}
            onClose={() => setActiveAssetId(null)}
          />
        </div>
      ) : null}

      <FsBrowserDialog
        open={browserOpen}
        title="本地导入素材"
        submitLabel="导入所选"
        onClose={() => setBrowserOpen(false)}
        onSelectMany={(paths) => {
          setBrowserOpen(false);
          if (paths.length > 0) {
            importLocal.mutate(paths);
          }
        }}
      />

      {management ? (
        <FsBrowserDialog
          open={relocatingAsset !== null}
          title="重新定位失效素材"
          submitLabel="使用此路径"
          onClose={() => setRelocatingAsset(null)}
          onSelect={(path) => {
            if (relocatingAsset) {
              patchMaterial.mutate({
                asset: relocatingAsset,
                payload: { reference_path: path }
              });
            }
          }}
        />
      ) : null}
    </section>
  );
}

type FolderNode = {
  name: string;
  path: string;
  count: number;
};

function FolderTile({ folder, onOpen }: { folder: FolderNode; onOpen: () => void }): ReactElement {
  return (
    <button
      className="group overflow-hidden rounded-md border border-line text-left transition-colors hover:border-line-strong"
      type="button"
      title={folder.name}
      onClick={onOpen}
    >
      <div className="grid aspect-video place-items-center bg-ink text-fg-faint">
        <FolderGlyph />
      </div>
      <div className="flex items-center justify-between gap-1 px-1.5 py-1">
        <span className="truncate text-[11px] text-fg">{folder.name}</span>
        <span className="shrink-0 text-[10px] tabular-nums text-fg-faint">{folder.count}</span>
      </div>
    </button>
  );
}

function AssetTile({
  asset,
  active,
  management,
  actionPending,
  onClick,
  onToggleEnabled,
  onRelocate,
  onUnlink
}: {
  asset: MaterialAsset;
  active: boolean;
  management: boolean;
  actionPending: boolean;
  onClick?: () => void;
  onToggleEnabled: () => void;
  onRelocate: () => void;
  onUnlink: () => void;
}): ReactElement {
  const [thumbFailed, setThumbFailed] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const understanding = understandingBadgeProps(asset.understanding_status);
  return (
    <div
      className={`group relative overflow-hidden rounded-md border transition-colors ${
        active ? "border-accent" : "border-line hover:border-line-strong"
      } ${asset.enabled && asset.usable ? "" : "opacity-50"}`}
      onMouseLeave={() => setMenuOpen(false)}
    >
      <button
        className="block w-full text-left"
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
          {asset.duration_sec !== null && asset.duration_sec > 0 ? (
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

      {management ? (
        <>
          <button
            className="absolute right-1 top-1 hidden h-6 w-6 place-items-center rounded-md bg-black/60 text-xs text-fg hover:bg-black/80 group-hover:grid"
            type="button"
            aria-label={`素材 ${asset.filename || asset.asset_id} 更多操作`}
            onClick={() => setMenuOpen((open) => !open)}
          >
            ⋯
          </button>
          {menuOpen ? (
            <div className="absolute right-1 top-8 z-10 w-28 overflow-hidden rounded-md border border-line bg-raised py-1 text-xs">
              <TileMenuItem
                label={asset.enabled ? "禁用" : "启用"}
                disabled={actionPending}
                onClick={onToggleEnabled}
              />
              {asset.invalid ? (
                <TileMenuItem label="重新定位" disabled={actionPending} onClick={onRelocate} />
              ) : null}
              <TileMenuItem label="删除引用" danger disabled={actionPending} onClick={onUnlink} />
            </div>
          ) : null}
        </>
      ) : null}
    </div>
  );
}

function TileMenuItem({
  label,
  danger = false,
  disabled,
  onClick
}: {
  label: string;
  danger?: boolean;
  disabled: boolean;
  onClick: () => void;
}): ReactElement {
  return (
    <button
      className={`block w-full px-3 py-1.5 text-left hover:bg-hover disabled:opacity-40 ${
        danger ? "text-danger" : "text-fg"
      }`}
      type="button"
      disabled={disabled}
      onClick={onClick}
    >
      {label}
    </button>
  );
}

/** rel_dir 为空的素材挂在根；文件夹层级由 rel_dir 的 "/" 分段还原。 */
function assetDir(asset: MaterialAsset): string {
  return asset.rel_dir ?? "";
}

function assetsAt(assets: MaterialAsset[], dir: string): MaterialAsset[] {
  return assets.filter((asset) => assetDir(asset) === dir);
}

function foldersAt(assets: MaterialAsset[], dir: string): FolderNode[] {
  const prefix = dir === "" ? "" : `${dir}/`;
  const nodes = new Map<string, FolderNode>();
  for (const asset of assets) {
    const assetPath = assetDir(asset);
    if (assetPath === "" || assetPath === dir || !assetPath.startsWith(prefix)) {
      continue;
    }
    const nextSegment = assetPath.slice(prefix.length).split("/")[0];
    if (!nextSegment) {
      continue;
    }
    const path = `${prefix}${nextSegment}`;
    const existing = nodes.get(path);
    if (existing) {
      existing.count += 1;
    } else {
      nodes.set(path, { name: nextSegment, path, count: 1 });
    }
  }
  return [...nodes.values()].sort((a, b) => a.name.localeCompare(b.name, "zh-Hans-CN"));
}

function breadcrumbSegments(dir: string): Array<{ name: string; path: string }> {
  const parts = dir.split("/");
  return parts.map((name, index) => ({
    name,
    path: parts.slice(0, index + 1).join("/")
  }));
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

function FolderGlyph(): ReactElement {
  return (
    <svg aria-hidden width="34" height="34" viewBox="0 0 24 24" fill="none">
      <path
        d="M3.5 6.5A1.5 1.5 0 0 1 5 5h4l2 2.5h8A1.5 1.5 0 0 1 20.5 9v8A1.5 1.5 0 0 1 19 18.5H5A1.5 1.5 0 0 1 3.5 17V6.5Z"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
      />
    </svg>
  );
}
