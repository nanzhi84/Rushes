import type { ReactElement } from "react";
import type { ProjectTreeProject } from "../../api/client";
import type { TreeDialogKind, TreeSelection } from "../../state/ui_store";

export type TreeAction = {
  kind: TreeDialogKind | "closeCase";
  projectId?: string;
  caseId?: string;
};

type ProjectTreeProps = {
  projects: ProjectTreeProject[];
  expandedProjectIds: Record<string, boolean>;
  selected: TreeSelection;
  onToggleProject: (projectId: string) => void;
  onSelectProjectsRoot: () => void;
  onSelectProject: (projectId: string) => void;
  onSelectCase: (projectId: string, caseId: string) => void;
  onAction: (action: TreeAction) => void;
};

export function ProjectTree({
  projects,
  expandedProjectIds,
  selected,
  onToggleProject,
  onSelectProjectsRoot,
  onSelectProject,
  onSelectCase,
  onAction
}: ProjectTreeProps): ReactElement {
  return (
    <nav aria-label="项目文件树" className="flex h-full flex-col gap-3">
      <div className="flex items-center justify-between">
        <button
          className={rootButtonClass(selected.type === "projects")}
          type="button"
          onClick={onSelectProjectsRoot}
        >
          项目
        </button>
        <TreeActionButton label="新建" onClick={() => onAction({ kind: "createProject" })} />
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto pr-1">
        {projects.length === 0 ? (
          <p className="rounded-md border border-dashed border-[#cbd5e1] px-3 py-4 text-sm text-[#64748b]">
            还没有项目。
          </p>
        ) : (
          <ul className="space-y-1">
            {projects.map((project) => {
              const expanded = expandedProjectIds[project.project_id] ?? true;
              const projectSelected =
                selected.type === "project" && selected.projectId === project.project_id;
              return (
                <li key={project.project_id}>
                  <div className={rowClass(projectSelected)}>
                    <button
                      aria-label={expanded ? "折叠项目" : "展开项目"}
                      className="grid h-7 w-7 place-items-center rounded text-[#64748b] hover:bg-[#e8edf3]"
                      type="button"
                      onClick={() => onToggleProject(project.project_id)}
                    >
                      {expanded ? "▾" : "▸"}
                    </button>
                    <button
                      className="min-w-0 flex-1 truncate text-left"
                      type="button"
                      onClick={() => onSelectProject(project.project_id)}
                      title={project.name}
                    >
                      {project.name}
                      {project.status !== "active" ? (
                        <span className="ml-2 text-xs text-[#9a3412]">{project.status}</span>
                      ) : null}
                    </button>
                    <ProjectActions projectId={project.project_id} onAction={onAction} />
                  </div>

                  {expanded ? (
                    <ul className="ml-7 mt-1 space-y-1 border-l border-[#d9dee7] pl-2">
                      {project.cases.map((caseNode) => {
                        const caseSelected =
                          selected.type === "case" &&
                          selected.projectId === project.project_id &&
                          selected.caseId === caseNode.case_id;
                        return (
                          <li key={caseNode.case_id}>
                            <div className={rowClass(caseSelected)}>
                              <button
                                className="min-w-0 flex-1 truncate text-left"
                                type="button"
                                onClick={() => onSelectCase(project.project_id, caseNode.case_id)}
                                title={caseNode.name}
                              >
                                {caseNode.name}
                                {caseNode.status !== "active" ? (
                                  <span className="ml-2 text-xs text-[#9a3412]">
                                    {caseNode.status}
                                  </span>
                                ) : null}
                              </button>
                              <CaseActions
                                projectId={project.project_id}
                                caseId={caseNode.case_id}
                                onAction={onAction}
                              />
                            </div>
                          </li>
                        );
                      })}
                    </ul>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </nav>
  );
}

function ProjectActions({
  projectId,
  onAction
}: {
  projectId: string;
  onAction: (action: TreeAction) => void;
}): ReactElement {
  return (
    <div className="flex shrink-0 items-center gap-1">
      <TreeActionButton label="新建任务" onClick={() => onAction({ kind: "createCase", projectId })} />
      <TreeActionButton label="重命名" onClick={() => onAction({ kind: "renameProject", projectId })} />
      <TreeActionButton label="复制" onClick={() => onAction({ kind: "copyProject", projectId })} />
      <TreeActionButton label="删除" danger onClick={() => onAction({ kind: "deleteProject", projectId })} />
    </div>
  );
}

function CaseActions({
  projectId,
  caseId,
  onAction
}: {
  projectId: string;
  caseId: string;
  onAction: (action: TreeAction) => void;
}): ReactElement {
  return (
    <div className="flex shrink-0 items-center gap-1">
      <TreeActionButton label="新建" onClick={() => onAction({ kind: "createCase", projectId })} />
      <TreeActionButton
        label="重命名"
        onClick={() => onAction({ kind: "renameCase", projectId, caseId })}
      />
      <TreeActionButton label="复制" onClick={() => onAction({ kind: "copyCase", projectId, caseId })} />
      <TreeActionButton label="移动" onClick={() => onAction({ kind: "moveCase", projectId, caseId })} />
      <TreeActionButton label="关闭" onClick={() => onAction({ kind: "closeCase", projectId, caseId })} />
      <TreeActionButton
        label="删除"
        danger
        onClick={() => onAction({ kind: "deleteCase", projectId, caseId })}
      />
    </div>
  );
}

function TreeActionButton({
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
      className={`rounded px-1.5 py-1 text-[11px] ${
        danger
          ? "text-[#b42318] hover:bg-[#fee4e2]"
          : "text-[#475569] hover:bg-[#e8edf3] hover:text-[#17202a]"
      }`}
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        onClick();
      }}
    >
      {label}
    </button>
  );
}

function rootButtonClass(active: boolean): string {
  return `rounded-md px-2 py-1.5 text-sm font-semibold ${
    active ? "bg-[#17202a] text-white" : "text-[#17202a] hover:bg-[#e8edf3]"
  }`;
}

function rowClass(active: boolean): string {
  return `group flex min-h-9 items-center gap-1 rounded-md px-1 text-sm ${
    active ? "bg-[#dfe7f1] text-[#17202a]" : "text-[#334155] hover:bg-[#edf1f5]"
  }`;
}
