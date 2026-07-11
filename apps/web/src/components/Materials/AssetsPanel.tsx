import * as ContextMenu from "@radix-ui/react-context-menu";
import * as Dialog from "@radix-ui/react-dialog";
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  FileText,
  Film,
  Folder,
  FolderOpen,
  Image as ImageIcon,
  Loader2,
  MapPin,
  MoreHorizontal,
  Music,
  Plus,
  RotateCw,
  Square,
  Trash2,
  Type
} from "lucide-react";
import { useMemo, useState } from "react";
import type { ElementType, ReactElement } from "react";
import { api, type MaterialAsset } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import { FsBrowserDialog } from "./FsBrowserDialog";
import { MaterialSummaryPanel } from "./MaterialSummaryPanel";
import { StatusBadge, understandingBadgeProps } from "./StatusBadge";
import { useMaterialsEvents } from "./useMaterialsEvents";

type AssetsPanelProps = {
  draftId: string;
  /** 点击素材瓦片时触发，工作台用来在预览区试看。 */
  onPreviewAsset?: (asset: MaterialAsset) => void;
  previewingAssetId?: string | null;
  enableEvents?: boolean;
  /** 管理模式：瓦片菜单 + 摘要详情 + 失效重检。 */
  management?: boolean;
  gridClassName?: string;
  understandingProgress?: { completed: number; total: number } | null;
  onCancelUnderstanding?: () => void;
  cancellingUnderstanding?: boolean;
};

/** 素材面板：文件夹分组网格；导入只走「系统原生选择框 → reference 零拷贝索引」。 */
export function AssetsPanel({
  draftId,
  onPreviewAsset,
  previewingAssetId = null,
  enableEvents = true,
  management = false,
  gridClassName,
  understandingProgress = null,
  onCancelUnderstanding,
  cancellingUnderstanding = false
}: AssetsPanelProps): ReactElement {
  const queryClient = useQueryClient();
  const [currentDir, setCurrentDir] = useState("");
  const [picking, setPicking] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [skippedFiles, setSkippedFiles] = useState<string[]>([]);
  const [failedFiles, setFailedFiles] = useState<string[]>([]);
  const [duplicateFiles, setDuplicateFiles] = useState<string[]>([]);
  const [activeAssetId, setActiveAssetId] = useState<string | null>(null);
  const [relocatingAsset, setRelocatingAsset] = useState<MaterialAsset | null>(null);
  const [deletingAsset, setDeletingAsset] = useState<MaterialAsset | null>(null);

  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(draftId),
    queryFn: () => api.listMaterials(draftId),
    refetchInterval: 5_000
  });
  useMaterialsEvents(draftId, enableEvents);

  const invalidateMaterials = async (): Promise<void> => {
    await queryClient.invalidateQueries({ queryKey: queryKeys.materials(draftId) });
  };

  /** 后端弹 macOS 原生选择框拿绝对路径 → reference 原地索引（零拷贝，不占双份磁盘）。 */
  async function pickAndImport(mode: "files" | "folder"): Promise<void> {
    setPicking(true);
    setImportError(null);
    try {
      const picked = await api.pickLocalPaths(mode);
      if (!picked.available) {
        setImportError("当前环境无法弹出系统选择框。可以在对话里告诉代理要导入的本地路径。");
        return;
      }
      if (picked.paths.length === 0) {
        return; // 用户取消
      }
      const response = await api.importLocalMaterial(draftId, {
        paths: picked.paths,
        storage_mode: "reference"
      });
      setSkippedFiles(response.skipped ?? []);
      setFailedFiles(response.failed ?? []);
      setDuplicateFiles(response.duplicates ?? []);
      await invalidateMaterials();
    } catch {
      setImportError("导入失败，请重试。");
    } finally {
      setPicking(false);
    }
  }

  const deleteMaterial = useMutation({
    mutationFn: (asset: MaterialAsset) => api.deleteMaterial(draftId, asset.asset_id),
    onSuccess: invalidateMaterials
  });

  // 后端已无原地改 reference 的 PATCH；失效素材「重新定位」= 从新路径重新原地索引。
  const relocateMaterial = useMutation({
    mutationFn: (path: string) =>
      api.importLocalMaterial(draftId, { paths: [path], storage_mode: "reference" }),
    onSuccess: async (response) => {
      setRelocatingAsset(null);
      setSkippedFiles(response.skipped ?? []);
      setFailedFiles(response.failed ?? []);
      setDuplicateFiles(response.duplicates ?? []);
      await invalidateMaterials();
    }
  });

  const revalidateMaterials = useMutation({
    mutationFn: () => api.revalidateMaterials(draftId),
    onSuccess: (response) => {
      queryClient.setQueryData(queryKeys.materials(draftId), response);
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
    picking ||
    deleteMaterial.isPending ||
    relocateMaterial.isPending ||
    revalidateMaterials.isPending;

  return (
    <section className="flex h-full min-h-0 flex-col" aria-label="素材面板">
      <header className="flex min-h-8 shrink-0 items-center justify-between gap-2 border-b border-line px-2">
        <span className="whitespace-nowrap text-xs font-semibold text-fg">
          我的素材 <span className="font-normal text-fg-muted">{assets.length}</span>
        </span>
        <div className="flex min-w-0 items-center justify-end gap-1">
          {picking ? <span className="text-xs text-fg-muted">等待选择…</span> : null}
          {understandingProgress ? (
            <div
              className="flex items-center gap-1.5 rounded-md border border-info/40 bg-info/10 px-2 py-1 text-xs text-info"
              role="status"
              aria-label={`素材理解中 ${understandingProgress.completed}/${understandingProgress.total}`}
            >
              <Loader2 size={13} className="animate-spin" aria-hidden />
              <span>
                理解中 {understandingProgress.completed}/{understandingProgress.total}
              </span>
              {onCancelUnderstanding ? (
                <button
                  className="ml-1 inline-flex items-center gap-1 rounded px-1 py-0.5 text-info hover:bg-info/15 disabled:opacity-40"
                  type="button"
                  aria-label="取消素材理解"
                  disabled={cancellingUnderstanding}
                  onClick={onCancelUnderstanding}
                >
                  <Square size={10} fill="currentColor" aria-hidden />
                  取消
                </button>
              ) : null}
            </div>
          ) : null}
          {management ? (
            <button
              className="grid size-7 shrink-0 place-items-center rounded-sm text-fg-muted transition-colors ease-standard hover:bg-hover disabled:opacity-40"
              type="button"
              aria-label="重新检测失效素材"
              title="重新检测失效素材"
              disabled={revalidateMaterials.isPending}
              onClick={() => revalidateMaterials.mutate()}
            >
              <RotateCw size={14} strokeWidth={1.75} aria-hidden />
            </button>
          ) : null}
          <button
            className="grid size-7 shrink-0 place-items-center rounded-sm text-fg-muted transition-colors ease-standard hover:bg-hover disabled:opacity-40"
            type="button"
            aria-label="导入文件夹"
            title="导入文件夹"
            disabled={picking}
            onClick={() => void pickAndImport("folder")}
          >
            <FolderOpen size={14} strokeWidth={1.75} aria-hidden />
          </button>
          <button
            className="flex shrink-0 items-center gap-1 whitespace-nowrap rounded-sm bg-accent px-2 py-1 text-2xs font-semibold text-white transition-colors ease-standard hover:bg-accent-strong disabled:opacity-40"
            type="button"
            disabled={picking}
            onClick={() => void pickAndImport("files")}
          >
            <Plus size={14} strokeWidth={2} aria-hidden />
            导入素材
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

      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {materialsQuery.isLoading ? (
          <p className="text-sm text-fg-muted">正在读取素材</p>
        ) : materialsQuery.error ? (
          <p className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
            素材列表加载失败
          </p>
        ) : assets.length === 0 ? (
          <div className="grid min-h-48 place-items-center px-5 text-center">
            <div>
              <Film size={22} strokeWidth={1.5} className="mx-auto mb-3 text-fg-faint" aria-hidden />
              <p className="text-xs leading-5 text-fg-muted">还没有素材</p>
              <button
                className="mt-3 rounded-sm bg-raised px-3 py-1.5 text-xs text-fg hover:bg-hover"
                type="button"
                onClick={() => void pickAndImport("files")}
              >
                从 Finder 导入
              </button>
              <p className="mt-2 text-2xs text-fg-faint">原地索引，不复制文件</p>
            </div>
          </div>
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
                active={previewingAssetId === asset.asset_id}
                management={management}
                actionPending={actionPending}
                // 单击 = 选中 + 右栏试看（对齐剪映）；摘要抽屉只从右键菜单进。
                onClick={onPreviewAsset ? () => onPreviewAsset(asset) : undefined}
                onViewSummary={() => setActiveAssetId(asset.asset_id)}
                onRelocate={() => setRelocatingAsset(asset)}
                onRequestDelete={() => setDeletingAsset(asset)}
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
        {duplicateFiles.length > 0 ? (
          <p className="mt-3 rounded-md border border-line bg-raised px-3 py-2 text-xs text-fg-muted">
            {duplicateFiles.length} 个文件已在素材库中，未重复导入。
          </p>
        ) : null}
        {failedFiles.length > 0 ? (
          <p className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            {failedFiles.length} 个文件导入失败：{failedFiles.slice(0, 5).join("、")}
            {failedFiles.length > 5 ? " 等" : ""}
          </p>
        ) : null}
        {importError ? (
          <p className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            {importError}
          </p>
        ) : null}
      </div>

      {management && activeAsset ? (
        <div className="max-h-[45%] shrink-0 overflow-y-auto border-t border-line p-3">
          <MaterialSummaryPanel
            draftId={draftId}
            asset={activeAsset}
            onClose={() => setActiveAssetId(null)}
          />
        </div>
      ) : null}

      {management ? (
        <FsBrowserDialog
          open={relocatingAsset !== null}
          title="重新定位失效素材"
          submitLabel="使用此路径"
          onClose={() => setRelocatingAsset(null)}
          onSelect={(path) => relocateMaterial.mutate(path)}
        />
      ) : null}

      <Dialog.Root
        open={deletingAsset !== null}
        onOpenChange={(next) => {
          if (!next) {
            setDeletingAsset(null);
          }
        }}
      >
        <Dialog.Portal>
          <Dialog.Overlay className="rx-overlay fixed inset-0 z-30 bg-black/60 backdrop-blur-sm" />
          <Dialog.Content
            aria-describedby={undefined}
            className="rx-content fixed left-1/2 top-1/2 z-40 w-[calc(100%-2rem)] max-w-sm -translate-x-1/2 -translate-y-1/2 rounded-xl bg-raised p-5 shadow-overlay focus:outline-none"
          >
            <Dialog.Title className="text-base font-semibold text-fg">删除素材引用</Dialog.Title>
            <p className="mt-3 text-sm text-fg-muted">
              将从本草稿移除
              <span className="mx-1 text-fg">
                {deletingAsset?.filename || deletingAsset?.asset_id}
              </span>
              的引用。物理文件与全局索引保留，之后重新导入可秒级回链。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <Dialog.Close asChild>
                <button
                  className="rounded-md border border-line px-3 py-2 text-sm text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
                  type="button"
                >
                  取消
                </button>
              </Dialog.Close>
              <button
                className="rounded-md bg-danger px-3 py-2 text-sm font-medium text-white transition-colors ease-standard hover:bg-danger/80 disabled:opacity-40"
                type="button"
                disabled={deleteMaterial.isPending}
                onClick={() => {
                  if (deletingAsset) {
                    deleteMaterial.mutate(deletingAsset);
                  }
                  setDeletingAsset(null);
                }}
              >
                删除引用
              </button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
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
      className="group overflow-hidden rounded-md border border-line text-left transition-colors ease-standard hover:border-line-strong"
      type="button"
      title={folder.name}
      onClick={onOpen}
    >
      <div className="grid aspect-video place-items-center bg-ink text-fg-faint">
        <Folder size={34} strokeWidth={1.5} aria-hidden />
      </div>
      <div className="flex items-center justify-between gap-1 px-1.5 py-1">
        <span className="truncate text-2xs text-fg">{folder.name}</span>
        <span className="shrink-0 text-2xs tabular-nums text-fg-faint">{folder.count}</span>
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
  onViewSummary,
  onRelocate,
  onRequestDelete
}: {
  asset: MaterialAsset;
  active: boolean;
  management: boolean;
  actionPending: boolean;
  onClick?: () => void;
  onViewSummary: () => void;
  onRelocate: () => void;
  onRequestDelete: () => void;
}): ReactElement {
  const [thumbFailed, setThumbFailed] = useState(false);
  const understanding = understandingBadgeProps(asset.understanding_status);
  // 未理解（none）时不渲染状态点：理解是对话里按需调用的工具，不是导入待办。
  const showUnderstandingDot = asset.understanding_status !== "none";
  // 缩略图（秒级 poster 任务）就绪即换真图；未就绪时用 kind 占位图标 + 呼吸脉冲表示处理中。
  const thumbReady = asset.thumbnail_ready && !thumbFailed;
  const KindIcon = kindIcon(asset.kind);
  const hasDuration = asset.duration_sec !== null && asset.duration_sec > 0;
  // 转码/索引后台任务仍在跑 → 右下角「处理中」旋转点，完成即消失。
  const ingesting = isIngestProcessing(asset);
  const tileClass = `group relative overflow-hidden rounded-md border transition-colors ease-standard ${
    active ? "border-selected bg-selected" : "border-line hover:border-line-strong"
  } ${asset.usable ? "" : "opacity-50"}`;

  const filename = asset.filename || asset.asset_id;
  const body = (
    <button
      className="block w-full text-left"
      type="button"
      title={filename}
      // 主点击面即试看：给显性可访问名（区别于 ⋯ 的「更多操作」），键盘/读屏可达且不歧义。
      aria-label={onClick ? `试看 ${filename}` : filename}
      onClick={onClick}
    >
      <div className="relative aspect-video bg-ink">
        {thumbReady ? (
          <img
            src={api.mediaThumbnailUrl(asset.asset_id)}
            alt={`${asset.filename || asset.asset_id} 缩略图`}
            className="h-full w-full object-cover"
            loading="lazy"
            onError={() => setThumbFailed(true)}
          />
        ) : (
          <div
            className="tile-pulse grid h-full w-full place-items-center text-fg-faint"
            aria-label={`${kindLabel(asset.kind)}处理中`}
          >
            <KindIcon size={24} strokeWidth={1.5} aria-hidden />
          </div>
        )}
        {asset.invalid ? (
          <span className="absolute left-1 top-1">
            <StatusBadge label="失效" tone="danger" />
          </span>
        ) : null}
        {ingesting || hasDuration ? (
          <div className="absolute bottom-1 right-1 flex items-center gap-1">
            {ingesting ? (
              <span
                className="grid h-5 w-5 place-items-center rounded bg-black/70 text-white"
                aria-label="转码与索引处理中"
                title="处理中"
              >
                <Loader2 size={14} strokeWidth={2} className="animate-spin" aria-hidden />
              </span>
            ) : null}
            {asset.duration_sec !== null && asset.duration_sec > 0 ? (
              <span className="rounded bg-black/70 px-1 py-0.5 text-2xs tabular-nums text-white">
                {formatDuration(asset.duration_sec)}
              </span>
            ) : null}
          </div>
        ) : null}
      </div>
      <div className="flex items-center justify-between gap-1 px-1.5 py-1">
        <span className="truncate text-2xs text-fg-muted">
          {asset.filename || asset.asset_id}
        </span>
        {showUnderstandingDot ? (
          <span
            aria-label={`理解状态：${understanding.label}`}
            className={`h-1.5 w-1.5 shrink-0 rounded-full ${understandingDotClass(asset.understanding_status)}`}
            title={understanding.label}
          />
        ) : null}
      </div>
    </button>
  );

  if (!management) {
    return <div className={tileClass}>{body}</div>;
  }

  const items = (Item: ElementType): ReactElement => (
    <AssetMenuItems
      item={Item}
      asset={asset}
      actionPending={actionPending}
      onViewSummary={onViewSummary}
      onRelocate={onRelocate}
      onRequestDelete={onRequestDelete}
    />
  );

  return (
    <ContextMenu.Root>
      <ContextMenu.Trigger asChild>
        <div className={tileClass}>
          {body}
          <DropdownMenu.Root>
            <DropdownMenu.Trigger asChild>
              <button
                className="absolute right-1 top-1 hidden h-6 w-6 place-items-center rounded-md bg-black/60 text-fg transition-colors ease-standard hover:bg-black/80 group-hover:grid data-[state=open]:grid"
                type="button"
                aria-label={`素材 ${asset.filename || asset.asset_id} 更多操作`}
              >
                <MoreHorizontal size={14} strokeWidth={1.75} aria-hidden />
              </button>
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content
                className={TILE_MENU_CONTENT_CLASS}
                align="end"
                sideOffset={6}
                loop
              >
                {items(DropdownMenu.Item)}
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
          </DropdownMenu.Root>
        </div>
      </ContextMenu.Trigger>
      <ContextMenu.Portal>
        <ContextMenu.Content className={TILE_MENU_CONTENT_CLASS} loop>
          {items(ContextMenu.Item)}
        </ContextMenu.Content>
      </ContextMenu.Portal>
    </ContextMenu.Root>
  );
}

const TILE_MENU_CONTENT_CLASS =
  "rx-menu z-40 min-w-[8.5rem] overflow-hidden rounded-lg bg-raised p-1 text-xs shadow-pop";
const TILE_MENU_ITEM_CLASS =
  "flex cursor-pointer select-none items-center gap-2 rounded-md px-2.5 py-1.5 text-fg outline-none data-[highlighted]:bg-hover data-[disabled]:pointer-events-none data-[disabled]:opacity-40";
const TILE_MENU_ITEM_DANGER_CLASS =
  "flex cursor-pointer select-none items-center gap-2 rounded-md px-2.5 py-1.5 text-danger outline-none data-[highlighted]:bg-danger/10 data-[disabled]:pointer-events-none data-[disabled]:opacity-40";

/** 瓦片菜单三项（查看理解摘要/重新定位/删除引用）：DropdownMenu 与 ContextMenu 共用。 */
function AssetMenuItems({
  item: Item,
  asset,
  actionPending,
  onViewSummary,
  onRelocate,
  onRequestDelete
}: {
  item: ElementType;
  asset: MaterialAsset;
  actionPending: boolean;
  onViewSummary: () => void;
  onRelocate: () => void;
  onRequestDelete: () => void;
}): ReactElement {
  return (
    <>
      <Item className={TILE_MENU_ITEM_CLASS} onSelect={onViewSummary}>
        <FileText size={15} strokeWidth={1.75} aria-hidden />
        查看理解摘要
      </Item>
      {asset.invalid ? (
        <Item className={TILE_MENU_ITEM_CLASS} disabled={actionPending} onSelect={onRelocate}>
          <MapPin size={15} strokeWidth={1.75} aria-hidden />
          重新定位
        </Item>
      ) : null}
      <Item
        className={TILE_MENU_ITEM_DANGER_CLASS}
        disabled={actionPending}
        onSelect={onRequestDelete}
      >
        <Trash2 size={15} strokeWidth={1.75} aria-hidden />
        删除引用
      </Item>
    </>
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

/** 缩略图未就绪时的占位图标：按 kind 取 lucide 图标，兜底 FileText。 */
function kindIcon(kind: string): ElementType {
  const icons: Record<string, ElementType> = {
    video: Film,
    audio: Music,
    image: ImageIcon,
    font: Type
  };
  return icons[kind] ?? FileText;
}

/** proxy/index 后台任务仍在排队或执行——用 payload 现有的 jobs 状态字段判定，完成即为假。 */
function isIngestProcessing(asset: MaterialAsset): boolean {
  return asset.jobs.some(
    (job) =>
      (job.kind === "proxy" || job.kind === "index") &&
      (job.status === "pending" || job.status === "running")
  );
}

function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const minutes = Math.floor(total / 60)
    .toString()
    .padStart(2, "0");
  const rest = (total % 60).toString().padStart(2, "0");
  return `${minutes}:${rest}`;
}
