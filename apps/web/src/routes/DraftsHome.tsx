import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import type { ReactElement } from "react";
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
  const [menuOpen, setMenuOpen] = useState(false);
  const coverAssetIds = draft.cover_asset_ids.slice(0, 4);

  return (
    <div
      className="group relative rounded-lg border border-line bg-panel transition-colors hover:border-line-strong"
      onMouseLeave={() => setMenuOpen(false)}
    >
      <button className="block w-full text-left" type="button" onClick={onOpen}>
        <div className="grid aspect-video grid-cols-2 grid-rows-2 gap-px overflow-hidden rounded-t-lg bg-ink">
          {coverAssetIds.length === 0 ? (
            <div className="col-span-2 row-span-2 grid place-items-center text-fg-faint">
              <FilmGlyph />
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

      <button
        className="absolute right-2 top-2 hidden h-7 w-7 place-items-center rounded-md bg-black/60 text-sm text-fg hover:bg-black/80 group-hover:grid"
        type="button"
        aria-label={`草稿 ${draft.name} 更多操作`}
        onClick={() => setMenuOpen((open) => !open)}
      >
        ⋯
      </button>
      {menuOpen ? (
        <div className="absolute right-2 top-10 z-10 w-32 overflow-hidden rounded-md border border-line bg-raised py-1 text-sm">
          <MenuItem label="重命名" onClick={() => onAction("renameDraft")} />
          <MenuItem label="复制" onClick={() => onAction("copyDraft")} />
          <MenuItem label="删除" danger onClick={() => onAction("deleteDraft")} />
        </div>
      ) : null}
    </div>
  );
}

function MenuItem({
  label,
  danger = false,
  onClick
}: {
  label: string;
  danger?: boolean;
  onClick: () => void;
}): ReactElement {
  return (
    <button
      className={`block w-full px-3 py-1.5 text-left hover:bg-hover ${
        danger ? "text-danger" : "text-fg"
      }`}
      type="button"
      onClick={onClick}
    >
      {label}
    </button>
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

function FilmGlyph(): ReactElement {
  return (
    <svg aria-hidden width="40" height="40" viewBox="0 0 24 24" fill="none">
      <rect x="3" y="5" width="18" height="14" rx="2" stroke="currentColor" strokeWidth="1.5" />
      <path d="M7 5v14M17 5v14M3 9h4M3 15h4M17 9h4M17 15h4" stroke="currentColor" strokeWidth="1.5" />
    </svg>
  );
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
