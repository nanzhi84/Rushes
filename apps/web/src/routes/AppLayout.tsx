import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useRouterState } from "@tanstack/react-router";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api } from "../api/client";
import { queryKeys } from "../app/query_client";
import { createApiEventSource } from "../auth";
import { ProjectTree, type TreeAction } from "../components/ProjectTree/ProjectTree";
import { useUiStore, type TreeSelection } from "../state/ui_store";
import { TreeMutationDialog } from "./TreeMutationDialog";

type AppLayoutProps = {
  children: ReactNode;
};

export function AppLayout({ children }: AppLayoutProps): ReactElement {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const pathname = useRouterState({ select: (state) => state.location.pathname });
  const [connectionState, setConnectionState] = useState<"connecting" | "open" | "closed">(
    "connecting"
  );
  const {
    sidebarCollapsed,
    expandedProjectIds,
    treeDialog,
    toggleSidebar,
    toggleProjectExpanded,
    setSelection,
    openTreeDialog,
    closeTreeDialog
  } = useUiStore();

  const treeQuery = useQuery({
    queryKey: queryKeys.projectTree,
    queryFn: api.projectTree
  });

  const selected = useMemo(() => selectionFromPath(pathname), [pathname]);

  useEffect(() => {
    setSelection(selected);
  }, [selected, setSelection]);

  useEffect(() => {
    const source = createApiEventSource("/api/events");
    source.onopen = () => setConnectionState("open");
    source.onerror = () => setConnectionState("closed");
    const handleWorkspaceEvent = () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.projectTree });
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects });
    };
    for (const eventName of WORKSPACE_EVENT_TYPES) {
      source.addEventListener(eventName, handleWorkspaceEvent);
    }
    return () => {
      source.close();
    };
  }, [queryClient]);

  const projects = treeQuery.data?.projects ?? [];

  function handleAction(action: TreeAction): void {
    if (action.kind === "closeCase") {
      if (action.projectId) {
        void navigate({ to: "/projects/$projectId", params: { projectId: action.projectId } });
      }
      return;
    }
    openTreeDialog({
      kind: action.kind,
      projectId: action.projectId,
      caseId: action.caseId
    });
  }

  return (
    <div className="flex min-h-screen flex-col bg-[#f6f7f9] text-[#17202a]">
      <header className="flex h-12 shrink-0 items-center justify-between border-b border-[#d9dee7] bg-white px-4">
        <div className="flex items-center gap-3">
          <span className="text-sm font-semibold">Rushes</span>
        </div>
        <div className="flex items-center gap-4 text-sm text-[#475569]">
          <span className="inline-flex items-center gap-2">
            <span className={`h-2.5 w-2.5 rounded-full ${connectionColor(connectionState)}`} />
            {connectionLabel(connectionState)}
          </span>
          <button className="rounded-md px-2 py-1 hover:bg-[#f1f5f9]" type="button">
            设置
          </button>
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {sidebarCollapsed ? (
          <aside className="flex w-12 shrink-0 justify-center border-r border-[#d9dee7] bg-white py-3">
            <button
              className="grid h-9 w-9 place-items-center rounded-md border border-[#cbd5e1] text-lg text-[#334155] hover:bg-[#f1f5f9]"
              type="button"
              aria-label="展开项目导航"
              title="展开项目导航"
              onClick={toggleSidebar}
            >
              ›
            </button>
          </aside>
        ) : (
          <aside className="flex w-[360px] shrink-0 flex-col border-r border-[#d9dee7] bg-white p-3">
            <div className="mb-3 flex items-center justify-between gap-3">
              <span className="text-sm font-semibold text-[#17202a]">项目导航</span>
              <button
                className="grid h-8 w-8 place-items-center rounded-md border border-[#cbd5e1] text-lg text-[#334155] hover:bg-[#f1f5f9]"
                type="button"
                aria-label="折叠项目导航"
                title="折叠项目导航"
                onClick={toggleSidebar}
              >
                ‹
              </button>
            </div>
            <div className="min-h-0 flex-1">
              {treeQuery.isLoading ? (
                <p className="text-sm text-[#64748b]">正在读取文件树</p>
              ) : treeQuery.error ? (
                <p className="rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">
                  文件树加载失败
                </p>
              ) : (
                <ProjectTree
                  projects={projects}
                  expandedProjectIds={expandedProjectIds}
                  selected={selected}
                  onToggleProject={toggleProjectExpanded}
                  onSelectProjectsRoot={() => void navigate({ to: "/" })}
                  onSelectProject={(projectId) =>
                    void navigate({ to: "/projects/$projectId", params: { projectId } })
                  }
                  onSelectCase={(projectId, caseId) =>
                    void navigate({
                      to: "/projects/$projectId/cases/$caseId",
                      params: { projectId, caseId }
                    })
                  }
                  onAction={handleAction}
                />
              )}
            </div>
          </aside>
        )}
        <main className="min-w-0 flex-1 overflow-y-auto">{children}</main>
      </div>

      <TreeMutationDialog dialog={treeDialog} projects={projects} onClose={closeTreeDialog} />
    </div>
  );
}

function selectionFromPath(pathname: string): TreeSelection {
  const caseMatch = pathname.match(/^\/projects\/([^/]+)\/cases\/([^/]+)/);
  if (caseMatch) {
    return {
      type: "case",
      projectId: decodeURIComponent(caseMatch[1]),
      caseId: decodeURIComponent(caseMatch[2])
    };
  }
  const projectMatch = pathname.match(/^\/projects\/([^/]+)/);
  if (projectMatch) {
    return { type: "project", projectId: decodeURIComponent(projectMatch[1]) };
  }
  return { type: "projects" };
}

function connectionColor(state: "connecting" | "open" | "closed"): string {
  if (state === "open") {
    return "bg-[#16a34a]";
  }
  if (state === "closed") {
    return "bg-[#dc2626]";
  }
  return "bg-[#f59e0b]";
}

function connectionLabel(state: "connecting" | "open" | "closed"): string {
  if (state === "open") {
    return "本地已连接";
  }
  if (state === "closed") {
    return "连接中断";
  }
  return "连接中";
}

const WORKSPACE_EVENT_TYPES = [
  "ProjectCreated",
  "ProjectRenamed",
  "ProjectTrashed",
  "ProjectCopied",
  "CaseCreated",
  "CaseRenamed",
  "CaseCopied",
  "CaseMoved",
  "CaseClosed",
  "CaseTrashed",
  "AssetLinked",
  "AssetUnlinked",
  "MemorySaved",
  "CapabilityDegraded",
  "SecurityRefusal"
];
