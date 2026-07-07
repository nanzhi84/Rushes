import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type ProjectTreeCase } from "../api/client";
import { queryKeys } from "../app/query_client";
import { useWorkspaceEvents } from "../app/use_workspace_events";
import { EntityActionDialog } from "../components/Shell/EntityActionDialog";
import { TopBar } from "../components/Shell/TopBar";
import { useUiStore, type EntityDialogKind } from "../state/ui_store";
import { ProjectMaterialsView } from "./ProjectMaterialsPage";

export type ProjectDetailTab = "cases" | "materials" | "settings";

export function normalizeProjectDetailTab(value: unknown): ProjectDetailTab {
  return value === "materials" || value === "settings" ? value : "cases";
}

export function ProjectDetailPage(): ReactElement {
  const params = useParams({ strict: false }) as { projectId: string };
  const search = useSearch({ strict: false }) as { tab?: string };
  const { projectId } = params;
  const tab = normalizeProjectDetailTab(search.tab);
  const navigate = useNavigate();
  const connectionState = useWorkspaceEvents();
  const { entityDialog, openEntityDialog, closeEntityDialog } = useUiStore();

  const treeQuery = useQuery({
    queryKey: queryKeys.projectTree,
    queryFn: api.projectTree
  });
  const treeProjects = treeQuery.data?.projects ?? [];
  const project = useMemo(
    () => treeProjects.find((item) => item.project_id === projectId) ?? null,
    [projectId, treeProjects]
  );

  const setTab = (nextTab: ProjectDetailTab): void => {
    void navigate({
      to: "/projects/$projectId",
      params: { projectId },
      search: nextTab === "cases" ? {} : { tab: nextTab },
      replace: true
    });
  };

  return (
    <div className="flex min-h-screen flex-col bg-ink text-fg">
      <TopBar
        connectionState={connectionState}
        leading={
          <>
            <Link
              aria-label="返回项目列表"
              className="grid h-8 w-8 place-items-center rounded-md text-fg-muted hover:bg-hover hover:text-fg"
              to="/"
            >
              <HomeGlyph />
            </Link>
            <span className="text-fg-faint">/</span>
            <span className="truncate text-sm font-semibold">{project?.name ?? projectId}</span>
          </>
        }
      />

      <div className="border-b border-line bg-panel px-6">
        <nav className="mx-auto flex w-full max-w-6xl gap-1" aria-label="项目页面切换">
          <TabButton active={tab === "cases"} label="剪辑任务" onClick={() => setTab("cases")} />
          <TabButton active={tab === "materials"} label="素材" onClick={() => setTab("materials")} />
          <TabButton active={tab === "settings"} label="设置" onClick={() => setTab("settings")} />
        </nav>
      </div>

      <main className="mx-auto w-full max-w-6xl flex-1 px-6 py-6">
        {tab === "cases" ? (
          <CasesTab
            projectId={projectId}
            cases={project?.cases ?? []}
            loading={treeQuery.isLoading}
            onCaseAction={(kind, caseId) => openEntityDialog({ kind, projectId, caseId })}
          />
        ) : null}
        {tab === "materials" ? <ProjectMaterialsView projectId={projectId} /> : null}
        {tab === "settings" ? (
          <SettingsTab
            onProjectAction={(kind) => openEntityDialog({ kind, projectId })}
          />
        ) : null}
      </main>

      <EntityActionDialog dialog={entityDialog} projects={treeProjects} onClose={closeEntityDialog} />
    </div>
  );
}

type CaseActionKind = Extract<EntityDialogKind, "renameCase" | "copyCase" | "deleteCase" | "moveCase">;

function CasesTab({
  projectId,
  cases,
  loading,
  onCaseAction
}: {
  projectId: string;
  cases: ProjectTreeCase[];
  loading: boolean;
  onCaseAction: (kind: CaseActionKind, caseId: string) => void;
}): ReactElement {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [goal, setGoal] = useState("");

  const createCase = useMutation({
    mutationFn: () =>
      api.createCase(projectId, {
        name: caseNameFromGoal(goal),
        goal,
        brief: { goal }
      }),
    onSuccess: async (payload) => {
      await queryClient.invalidateQueries({ queryKey: queryKeys.projectTree });
      await navigate({
        to: "/projects/$projectId/cases/$caseId",
        params: { projectId, caseId: payload.case.case_id }
      });
    }
  });

  return (
    <div className="flex flex-col gap-6">
      <form
        className="rounded-lg border border-line bg-panel p-5"
        onSubmit={(event) => {
          event.preventDefault();
          createCase.mutate();
        }}
      >
        <h2 className="text-base font-semibold">新建剪辑任务</h2>
        <label className="mt-3 block text-sm text-fg-muted">
          目标文本
          <textarea
            className="mt-2 h-24 w-full resize-none rounded-md border border-line bg-ink px-3 py-2 text-fg outline-none placeholder:text-fg-faint focus:border-accent"
            value={goal}
            onChange={(event) => setGoal(event.target.value)}
            placeholder="例如：剪一条 30 秒种草视频"
          />
        </label>
        <div className="mt-3 flex justify-end">
          <button
            className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-strong disabled:opacity-40"
            type="submit"
            disabled={goal.trim().length === 0 || createCase.isPending}
          >
            创建并进入工作台
          </button>
        </div>
      </form>

      {loading ? (
        <p className="text-sm text-fg-muted">正在读取剪辑任务</p>
      ) : cases.length === 0 ? (
        <p className="rounded-lg border border-dashed border-line-strong px-4 py-10 text-center text-sm text-fg-muted">
          还没有剪辑任务。写下目标，创建第一个。
        </p>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2">
          {cases.map((caseNode) => (
            <CaseCard
              key={caseNode.case_id}
              caseNode={caseNode}
              onOpen={() =>
                void navigate({
                  to: "/projects/$projectId/cases/$caseId",
                  params: { projectId, caseId: caseNode.case_id }
                })
              }
              onAction={(kind) => onCaseAction(kind, caseNode.case_id)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function CaseCard({
  caseNode,
  onOpen,
  onAction
}: {
  caseNode: ProjectTreeCase;
  onOpen: () => void;
  onAction: (kind: CaseActionKind) => void;
}): ReactElement {
  const [menuOpen, setMenuOpen] = useState(false);
  return (
    <div
      className="group relative rounded-lg border border-line bg-panel transition-colors hover:border-line-strong"
      onMouseLeave={() => setMenuOpen(false)}
    >
      <button className="block w-full px-4 py-3 text-left" type="button" onClick={onOpen}>
        <span className="block truncate text-sm font-semibold text-fg">{caseNode.name}</span>
        <span className="mt-1 block text-xs text-fg-muted">
          {caseStatusLabel(caseNode.status)}
        </span>
      </button>
      <button
        className="absolute right-2 top-2 hidden h-7 w-7 place-items-center rounded-md bg-black/60 text-sm text-fg hover:bg-black/80 group-hover:grid"
        type="button"
        aria-label={`剪辑任务 ${caseNode.name} 更多操作`}
        onClick={() => setMenuOpen((open) => !open)}
      >
        ⋯
      </button>
      {menuOpen ? (
        <div className="absolute right-2 top-10 z-10 w-32 overflow-hidden rounded-md border border-line bg-raised py-1 text-sm">
          <CardMenuItem label="重命名" onClick={() => onAction("renameCase")} />
          <CardMenuItem label="复制" onClick={() => onAction("copyCase")} />
          <CardMenuItem label="移动" onClick={() => onAction("moveCase")} />
          <CardMenuItem label="删除" danger onClick={() => onAction("deleteCase")} />
        </div>
      ) : null}
    </div>
  );
}

function CardMenuItem({
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

function SettingsTab({
  onProjectAction
}: {
  onProjectAction: (
    kind: Extract<EntityDialogKind, "renameProject" | "copyProject" | "deleteProject">
  ) => void;
}): ReactElement {
  return (
    <div className="flex flex-col gap-4">
      <section className="rounded-lg border border-line bg-panel p-5">
        <h2 className="text-base font-semibold">成本汇总</h2>
        <p className="mt-2 text-sm text-fg-muted">成本汇总与默认策略后续接入。</p>
      </section>
      <section className="rounded-lg border border-line bg-panel p-5">
        <h2 className="text-base font-semibold">项目操作</h2>
        <div className="mt-3 flex flex-wrap gap-2">
          <button
            className="rounded-md border border-line px-3 py-2 text-sm text-fg hover:bg-hover"
            type="button"
            onClick={() => onProjectAction("renameProject")}
          >
            重命名项目
          </button>
          <button
            className="rounded-md border border-line px-3 py-2 text-sm text-fg hover:bg-hover"
            type="button"
            onClick={() => onProjectAction("copyProject")}
          >
            复制项目
          </button>
          <button
            className="rounded-md border border-danger/40 px-3 py-2 text-sm text-danger hover:bg-danger/10"
            type="button"
            onClick={() => onProjectAction("deleteProject")}
          >
            删除项目
          </button>
        </div>
      </section>
    </div>
  );
}

function TabButton({
  active,
  label,
  onClick
}: {
  active: boolean;
  label: string;
  onClick: () => void;
}): ReactElement {
  return (
    <button
      className={`border-b-2 px-3 py-2.5 text-sm transition-colors ${
        active
          ? "border-accent font-medium text-fg"
          : "border-transparent text-fg-muted hover:text-fg"
      }`}
      type="button"
      onClick={onClick}
    >
      {label}
    </button>
  );
}

function HomeGlyph(): ReactElement {
  return (
    <svg aria-hidden width="16" height="16" viewBox="0 0 24 24" fill="none">
      <path
        d="M4 11.5 12 4l8 7.5M6 10v9h12v-9"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function caseStatusLabel(status: string): string {
  const labels: Record<string, string> = {
    active: "进行中",
    closed: "已关闭",
    trashed: "已删除"
  };
  return labels[status] ?? status;
}

function caseNameFromGoal(goal: string): string {
  const trimmed = goal.trim();
  if (!trimmed) {
    return "未命名剪辑任务";
  }
  return trimmed.length > 28 ? `${trimmed.slice(0, 28)}...` : trimmed;
}
