import * as ContextMenu from "@radix-ui/react-context-menu";
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Copy, Film, MoreHorizontal, PencilLine, Trash2 } from "lucide-react";
import { useState } from "react";
import type { ElementType, ReactElement } from "react";
import { api, type DraftListItem } from "../api/client";
import { queryKeys } from "../app/query_client";
import { useWorkspaceEvents } from "../app/use_workspace_events";
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

  const drafts = draftsQuery.data?.drafts ?? [];

  const startCreation = (): void => {
    if (createMutation.isPending) {
      return;
    }
    createMutation.mutate();
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
          <button
            className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
            type="button"
            onClick={startCreation}
            disabled={createMutation.isPending}
          >
            {createMutation.isPending ? "创建中" : "开始创作"}
          </button>
        </div>

        <div className="mt-6">
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
                  onOpen={() =>
                    void navigate({ to: "/drafts/$draftId", params: { draftId: draft.draft_id } })
                  }
                  onAction={(kind) => openEntityDialog({ kind, draftId: draft.draft_id })}
                />
              ))}
            </div>
          )}
        </div>
      </main>

      <EntityActionDialog dialog={entityDialog} drafts={drafts} onClose={closeEntityDialog} />
      <WorkspaceSettingsDialog open={settingsOpen} onClose={() => setSettingsOpen(false)} />
    </div>
  );
}

type DraftCardProps = {
  draft: DraftListItem;
  onOpen: () => void;
  onAction: (kind: EntityDialogKind) => void;
};

function DraftCard({ draft, onOpen, onAction }: DraftCardProps): ReactElement {
  const coverAssetIds = draft.cover_asset_ids.slice(0, 4);

  return (
    <ContextMenu.Root>
      <ContextMenu.Trigger asChild>
        <div className="group relative rounded-lg border border-line bg-panel shadow-raised transition-colors ease-standard hover:border-line-strong">
          <button className="block w-full text-left" type="button" onClick={onOpen}>
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
        </div>
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
