import * as ContextMenu from "@radix-ui/react-context-menu";
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Check, Copy, Film, ListChecks, MoreHorizontal, PencilLine, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import type { ElementType, ReactElement } from "react";
import { api, type DraftListItem, type DraftListResponse } from "../api/client";
import { queryKeys } from "../app/query_client";
import { useWorkspaceEvents } from "../app/use_workspace_events";
import { BatchDeleteDraftsDialog } from "../components/Shell/BatchDeleteDraftsDialog";
import { EntityActionDialog } from "../components/Shell/EntityActionDialog";
import { TopBar } from "../components/Shell/TopBar";
import { WorkspaceSettingsDialog } from "../components/Shell/WorkspaceSettingsDialog";
import { useUiStore, type EntityDialogKind } from "../state/ui_store";

export function DraftsHomePage(): ReactElement {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const connectionState = useWorkspaceEvents();
  const { entityDialog, openEntityDialog, closeEntityDialog } = useUiStore();
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [batchMode, setBatchMode] = useState(false);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [selectedDraftIds, setSelectedDraftIds] = useState<Set<string>>(() => new Set());

  const draftsQuery = useQuery({
    queryKey: queryKeys.drafts,
    queryFn: api.listDrafts
  });

  const createMutation = useMutation({
    mutationFn: () => api.createDraft(),
    onSuccess: async (res) => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.drafts });
      await navigate({ to: "/drafts/$draftId", params: { draftId: res.draft.draft_id } });
    }
  });

  const batchDeleteMutation = useMutation({
    mutationFn: (draftIds: string[]) => api.trashDrafts(draftIds),
    onSuccess: (response, requestedDraftIds) => {
      // 后端已经原子确认整批请求；立即移除本地卡片，避免等待重新拉取时产生“没有删掉”的错觉。
      // 后台失效重取仍然保留，用服务端状态校准并覆盖并发变更。
      const removed = new Set([...requestedDraftIds, ...response.deleted_draft_ids]);
      queryClient.setQueryData<DraftListResponse>(queryKeys.drafts, (current) =>
        current
          ? {
              ...current,
              drafts: current.drafts.filter((draft) => !removed.has(draft.draft_id))
            }
          : current
      );
      setBatchDeleteOpen(false);
      setBatchMode(false);
      setSelectedDraftIds(new Set());
      void queryClient.invalidateQueries({ queryKey: queryKeys.drafts });
    }
  });

  const drafts = draftsQuery.data?.drafts ?? [];
  const selectedDrafts = drafts.filter((draft) => selectedDraftIds.has(draft.draft_id));
  const allDraftsSelected = drafts.length > 0 && selectedDrafts.length === drafts.length;

  useEffect(() => {
    const available = new Set((draftsQuery.data?.drafts ?? []).map((draft) => draft.draft_id));
    setSelectedDraftIds((current) => {
      const next = new Set([...current].filter((draftId) => available.has(draftId)));
      if (next.size === current.size && [...next].every((draftId) => current.has(draftId))) {
        return current;
      }
      return next;
    });
  }, [draftsQuery.data?.drafts]);

  const startCreation = (): void => {
    if (createMutation.isPending) {
      return;
    }
    createMutation.mutate();
  };

  const exitBatchMode = (): void => {
    if (batchDeleteMutation.isPending) {
      return;
    }
    setBatchDeleteOpen(false);
    setBatchMode(false);
    setSelectedDraftIds(new Set());
    batchDeleteMutation.reset();
  };

  const toggleDraftSelection = (draftId: string): void => {
    setSelectedDraftIds((current) => {
      const next = new Set(current);
      if (next.has(draftId)) {
        next.delete(draftId);
      } else {
        next.add(draftId);
      }
      return next;
    });
  };

  const toggleSelectAll = (): void => {
    setSelectedDraftIds(
      allDraftsSelected ? new Set() : new Set(drafts.map((draft) => draft.draft_id))
    );
  };

  return (
    <div className="flex min-h-screen flex-col bg-ink text-fg">
      <TopBar connectionState={connectionState} onSettingsClick={() => setSettingsOpen(true)} />

      <main className="mx-auto w-full max-w-6xl flex-1 px-6 py-8">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold">草稿</h1>
            <p className="mt-1 text-sm text-fg-muted">每个草稿是一次创作：把一堆素材聊成一条成片。</p>
          </div>
          {!batchMode ? (
            <div className="flex items-center gap-2">
              {!draftsQuery.isLoading && drafts.length > 0 ? (
                <button
                  className="inline-flex items-center gap-2 rounded-md border border-line px-4 py-2 text-sm font-medium text-fg-muted transition-colors ease-standard hover:border-line-strong hover:bg-hover hover:text-fg"
                  type="button"
                  onClick={() => setBatchMode(true)}
                >
                  <ListChecks size={16} strokeWidth={1.75} aria-hidden />
                  批量管理
                </button>
              ) : null}
              <button
                className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
                type="button"
                onClick={startCreation}
                disabled={createMutation.isPending}
              >
                {createMutation.isPending ? "创建中" : "开始创作"}
              </button>
            </div>
          ) : null}
        </div>

        <div className="mt-6">
          {batchMode ? (
            <div
              className="sticky top-2 z-20 mb-4 flex flex-wrap items-center gap-2 rounded-lg border border-line bg-raised px-3 py-2 shadow-pop"
              role="toolbar"
              aria-label="草稿批量管理"
            >
              <span className="inline-flex min-w-32 items-center gap-2 text-sm text-fg-muted" aria-live="polite">
                <ListChecks size={16} strokeWidth={1.75} aria-hidden />
                已选择 <strong className="font-semibold text-fg">{selectedDrafts.length}</strong> 条
              </span>
              <button
                className="rounded-md px-3 py-1.5 text-sm text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
                type="button"
                onClick={toggleSelectAll}
              >
                {allDraftsSelected ? "取消全选" : "全选"}
              </button>
              <div className="min-w-4 flex-1" />
              <button
                className="rounded-md px-3 py-1.5 text-sm text-fg-muted transition-colors ease-standard hover:bg-hover hover:text-fg"
                type="button"
                onClick={exitBatchMode}
              >
                退出
              </button>
              <button
                className="inline-flex items-center gap-2 rounded-md bg-danger px-3 py-1.5 text-sm font-medium text-white transition-colors ease-standard hover:bg-danger/80 disabled:opacity-40"
                type="button"
                disabled={selectedDrafts.length === 0}
                onClick={() => {
                  batchDeleteMutation.reset();
                  setBatchDeleteOpen(true);
                }}
              >
                <Trash2 size={15} strokeWidth={1.75} aria-hidden />
                删除所选
              </button>
            </div>
          ) : null}
          {draftsQuery.isLoading ? (
            <p className="text-sm text-fg-muted">正在读取草稿</p>
          ) : draftsQuery.error ? (
            <p className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
              草稿列表加载失败
            </p>
          ) : drafts.length === 0 ? (
            <button
              className="grid w-full place-items-center rounded-lg border border-dashed border-line-strong px-6 py-16 text-center hover:border-accent disabled:opacity-40"
              type="button"
              onClick={startCreation}
              disabled={createMutation.isPending}
            >
              <span className="text-base font-medium text-fg">还没有草稿</span>
              <span className="mt-2 text-sm text-fg-muted">点「开始创作」，导入素材聊成一条成片。</span>
            </button>
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              {drafts.map((draft) => (
                <DraftCard
                  key={draft.draft_id}
                  draft={draft}
                  selectionMode={batchMode}
                  selected={selectedDraftIds.has(draft.draft_id)}
                  onOpen={() =>
                    void navigate({ to: "/drafts/$draftId", params: { draftId: draft.draft_id } })
                  }
                  onToggleSelection={() => toggleDraftSelection(draft.draft_id)}
                  onAction={(kind) => openEntityDialog({ kind, draftId: draft.draft_id })}
                />
              ))}
            </div>
          )}
        </div>
      </main>

      <EntityActionDialog dialog={entityDialog} drafts={drafts} onClose={closeEntityDialog} />
      <BatchDeleteDraftsDialog
        open={batchDeleteOpen}
        drafts={selectedDrafts}
        pending={batchDeleteMutation.isPending}
        failed={batchDeleteMutation.isError}
        onCancel={() => {
          if (!batchDeleteMutation.isPending) {
            setBatchDeleteOpen(false);
            batchDeleteMutation.reset();
          }
        }}
        onConfirm={() => batchDeleteMutation.mutate(selectedDrafts.map((draft) => draft.draft_id))}
      />
      <WorkspaceSettingsDialog open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </div>
  );
}

type DraftCardProps = {
  draft: DraftListItem;
  selectionMode: boolean;
  selected: boolean;
  onOpen: () => void;
  onToggleSelection: () => void;
  onAction: (kind: EntityDialogKind) => void;
};

function DraftCard({
  draft,
  selectionMode,
  selected,
  onOpen,
  onToggleSelection,
  onAction
}: DraftCardProps): ReactElement {
  const coverAssetIds = draft.cover_asset_ids.slice(0, 4);

  const card = (
    <div
      className={`group relative rounded-lg border bg-panel shadow-raised transition-colors ease-standard ${
        selected
          ? "border-accent ring-2 ring-accent/25"
          : "border-line hover:border-line-strong"
      }`}
    >
          <button
            className="block w-full text-left"
            type="button"
            onClick={selectionMode ? onToggleSelection : onOpen}
            aria-label={selectionMode ? `${selected ? "取消选择" : "选择"}草稿 ${draft.name}` : undefined}
            aria-pressed={selectionMode ? selected : undefined}
          >
            <div className="grid aspect-video grid-cols-2 grid-rows-2 gap-px overflow-hidden rounded-t-lg bg-ink">
              {coverAssetIds.length === 0 ? (
                <div className="col-span-2 row-span-2 grid place-items-center text-fg-faint">
                  <Film size={40} strokeWidth={1.5} aria-hidden />
                </div>
              ) : (
                coverAssetIds.map((assetId, index) => (
                  <img
                    key={assetId}
                    alt=""
                    className={`h-full w-full object-cover ${collageCellClass(coverAssetIds.length, index)}`}
                    src={api.mediaThumbnailUrl(assetId)}
                    loading="lazy"
                  />
                ))
              )}
            </div>
            <div className="px-4 py-3">
              <span className="block truncate text-sm font-semibold text-fg">{draft.name}</span>
              <span className="mt-1 block text-xs text-fg-muted">
                {draft.material_count} 个素材 · {formatDate(draft.updated_at)}
              </span>
            </div>
          </button>

          {selectionMode ? (
            <span
              className={`pointer-events-none absolute left-2 top-2 grid h-7 w-7 place-items-center rounded-md border text-white transition-colors ease-standard ${
                selected ? "border-accent bg-accent" : "border-white/70 bg-black/55"
              }`}
              aria-hidden
            >
              {selected ? <Check size={17} strokeWidth={2.25} /> : null}
            </span>
          ) : (
            <DropdownMenu.Root>
            <DropdownMenu.Trigger asChild>
              <button
                className="absolute right-2 top-2 hidden h-7 w-7 place-items-center rounded-md bg-black/60 text-fg transition-colors ease-standard hover:bg-black/80 group-hover:grid data-[state=open]:grid"
                type="button"
                aria-label={`草稿 ${draft.name} 更多操作`}
              >
                <MoreHorizontal size={16} strokeWidth={1.75} aria-hidden />
              </button>
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content
                className={MENU_CONTENT_CLASS}
                align="end"
                sideOffset={6}
                loop
              >
                <DraftMenuItems item={DropdownMenu.Item} onAction={onAction} />
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
            </DropdownMenu.Root>
          )}
        </div>
  );

  if (selectionMode) {
    return card;
  }

  return (
    <ContextMenu.Root>
      <ContextMenu.Trigger asChild>
        {card}
      </ContextMenu.Trigger>
      <ContextMenu.Portal>
        <ContextMenu.Content className={MENU_CONTENT_CLASS} loop>
          <DraftMenuItems item={ContextMenu.Item} onAction={onAction} />
        </ContextMenu.Content>
      </ContextMenu.Portal>
    </ContextMenu.Root>
  );
}

/** 卡片菜单三项（重命名/复制/删除）：DropdownMenu 与 ContextMenu 共用，避免两处漂移。 */
const MENU_CONTENT_CLASS =
  "rx-menu z-40 min-w-[9rem] overflow-hidden rounded-lg bg-raised p-1 text-sm shadow-pop";
const MENU_ITEM_CLASS =
  "flex cursor-pointer select-none items-center gap-2 rounded-md px-2.5 py-1.5 text-fg outline-none data-[highlighted]:bg-hover";
const MENU_ITEM_DANGER_CLASS =
  "flex cursor-pointer select-none items-center gap-2 rounded-md px-2.5 py-1.5 text-danger outline-none data-[highlighted]:bg-danger/10";

function DraftMenuItems({
  item: Item,
  onAction
}: {
  item: ElementType;
  onAction: (kind: EntityDialogKind) => void;
}): ReactElement {
  return (
    <>
      <Item className={MENU_ITEM_CLASS} onSelect={() => onAction("renameDraft")}>
        <PencilLine size={16} strokeWidth={1.75} aria-hidden />
        重命名
      </Item>
      <Item className={MENU_ITEM_CLASS} onSelect={() => onAction("copyDraft")}>
        <Copy size={16} strokeWidth={1.75} aria-hidden />
        复制
      </Item>
      <Item className={MENU_ITEM_DANGER_CLASS} onSelect={() => onAction("deleteDraft")}>
        <Trash2 size={16} strokeWidth={1.75} aria-hidden />
        删除
      </Item>
    </>
  );
}

function collageCellClass(count: number, index: number): string {
  if (count === 1) {
    return "col-span-2 row-span-2";
  }
  if (count === 2) {
    return "row-span-2";
  }
  if (count === 3 && index === 0) {
    return "row-span-2";
  }
  return "";
}

function formatDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) {
    return iso;
  }
  const month = String(date.getMonth() + 1).padStart(2, "0");
  const day = String(date.getDate()).padStart(2, "0");
  return `${date.getFullYear()}-${month}-${day}`;
}
