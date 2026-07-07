import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import type { ReactElement } from "react";
import { api, type ProjectRecord } from "../api/client";
import { queryKeys } from "../app/query_client";
import { useWorkspaceEvents } from "../app/use_workspace_events";
import { EntityActionDialog } from "../components/Shell/EntityActionDialog";
import { TopBar } from "../components/Shell/TopBar";
import { useUiStore, type EntityDialogKind } from "../state/ui_store";

export function ProjectsOverviewPage(): ReactElement {
  const navigate = useNavigate();
  const connectionState = useWorkspaceEvents();
  const { entityDialog, openEntityDialog, closeEntityDialog } = useUiStore();

  const projectsQuery = useQuery({
    queryKey: queryKeys.projects,
    queryFn: api.listProjects
  });
  const treeQuery = useQuery({
    queryKey: queryKeys.projectTree,
    queryFn: api.projectTree
  });

  const projects = projectsQuery.data?.projects ?? [];
  const treeProjects = treeQuery.data?.projects ?? [];

  return (
    <div className="flex min-h-screen flex-col bg-ink text-fg">
      <TopBar connectionState={connectionState} />

      <main className="mx-auto w-full max-w-6xl flex-1 px-6 py-8">
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div>
            <h1 className="text-xl font-semibold">项目</h1>
            <p className="mt-1 text-sm text-fg-muted">每个项目是一组素材和它的剪辑任务。</p>
          </div>
          <button
            className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-strong"
            type="button"
            onClick={() => openEntityDialog({ kind: "createProject" })}
          >
            ＋ 新建项目
          </button>
        </div>

        <div className="mt-6">
          {projectsQuery.isLoading ? (
            <p className="text-sm text-fg-muted">正在读取项目</p>
          ) : projectsQuery.error ? (
            <p className="rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
              项目列表加载失败
            </p>
          ) : projects.length === 0 ? (
            <button
              className="grid w-full place-items-center rounded-lg border border-dashed border-line-strong px-6 py-16 text-center hover:border-accent"
              type="button"
              onClick={() => openEntityDialog({ kind: "createProject" })}
            >
              <span className="text-base font-medium text-fg">还没有项目</span>
              <span className="mt-2 text-sm text-fg-muted">新建一个项目，导入素材开始剪辑。</span>
            </button>
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              {projects.map((project) => {
                const caseCount =
                  treeProjects.find((node) => node.project_id === project.project_id)?.cases
                    .length ?? 0;
                return (
                  <ProjectCard
                    key={project.project_id}
                    project={project}
                    caseCount={caseCount}
                    onOpen={() =>
                      void navigate({
                        to: "/projects/$projectId",
                        params: { projectId: project.project_id }
                      })
                    }
                    onAction={(kind) =>
                      openEntityDialog({ kind, projectId: project.project_id })
                    }
                  />
                );
              })}
            </div>
          )}
        </div>
      </main>

      <EntityActionDialog dialog={entityDialog} projects={treeProjects} onClose={closeEntityDialog} />
    </div>
  );
}

type ProjectCardProps = {
  project: ProjectRecord;
  caseCount: number;
  onOpen: () => void;
  onAction: (kind: Extract<EntityDialogKind, "renameProject" | "copyProject" | "deleteProject">) => void;
};

function ProjectCard({ project, caseCount, onOpen, onAction }: ProjectCardProps): ReactElement {
  const [menuOpen, setMenuOpen] = useState(false);
  const materialsQuery = useQuery({
    queryKey: queryKeys.materials(project.project_id),
    queryFn: () => api.listMaterials(project.project_id),
    staleTime: 30_000
  });
  const assets = materialsQuery.data?.assets ?? [];
  const thumbAssets = assets.filter((asset) => asset.thumbnail_ready).slice(0, 4);

  return (
    <div
      className="group relative rounded-lg border border-line bg-panel transition-colors hover:border-line-strong"
      onMouseLeave={() => setMenuOpen(false)}
    >
      <button className="block w-full text-left" type="button" onClick={onOpen}>
        <div className="grid aspect-video grid-cols-2 grid-rows-2 gap-px overflow-hidden rounded-t-lg bg-ink">
          {thumbAssets.length === 0 ? (
            <div className="col-span-2 row-span-2 grid place-items-center text-fg-faint">
              <FilmGlyph />
            </div>
          ) : (
            thumbAssets.map((asset, index) => (
              <img
                key={asset.asset_id}
                alt=""
                className={`h-full w-full object-cover ${collageCellClass(thumbAssets.length, index)}`}
                src={api.mediaThumbnailUrl(asset.asset_id)}
                loading="lazy"
              />
            ))
          )}
        </div>
        <div className="px-4 py-3">
          <span className="block truncate text-sm font-semibold text-fg">{project.name}</span>
          <span className="mt-1 block text-xs text-fg-muted">
            {caseCount} 个剪辑任务 · {assets.length} 个素材 · {formatDate(project.created_at)}
          </span>
        </div>
      </button>

      <button
        className="absolute right-2 top-2 hidden h-7 w-7 place-items-center rounded-md bg-black/60 text-sm text-fg hover:bg-black/80 group-hover:grid"
        type="button"
        aria-label={`项目 ${project.name} 更多操作`}
        onClick={() => setMenuOpen((open) => !open)}
      >
        ⋯
      </button>
      {menuOpen ? (
        <div className="absolute right-2 top-10 z-10 w-32 overflow-hidden rounded-md border border-line bg-raised py-1 text-sm">
          <MenuItem label="重命名" onClick={() => onAction("renameProject")} />
          <MenuItem label="复制" onClick={() => onAction("copyProject")} />
          <MenuItem label="删除" danger onClick={() => onAction("deleteProject")} />
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
