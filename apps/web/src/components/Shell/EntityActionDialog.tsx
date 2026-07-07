import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";
import { api, type ProjectTreeProject } from "../../api/client";
import { queryKeys } from "../../app/query_client";
import type { EntityDialogState } from "../../state/ui_store";

type EntityActionDialogProps = {
  dialog: EntityDialogState | null;
  projects: ProjectTreeProject[];
  onClose: () => void;
};

/** Project/Case 的创建、重命名、复制、删除、移动统一对话框；两态壳共用。 */
export function EntityActionDialog({
  dialog,
  projects,
  onClose
}: EntityActionDialogProps): ReactElement | null {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [goal, setGoal] = useState("");
  const [targetProjectId, setTargetProjectId] = useState("");
  const [confirmed, setConfirmed] = useState(false);

  const sourceProject = useMemo(
    () => projects.find((project) => project.project_id === dialog?.projectId) ?? null,
    [dialog?.projectId, projects]
  );
  const sourceCase = useMemo(
    () => sourceProject?.cases.find((caseNode) => caseNode.case_id === dialog?.caseId) ?? null,
    [dialog?.caseId, sourceProject]
  );

  // 用户开始编辑后不再重置：项目树查询在对话框打开期间刷新（SSE 失效重取）时，
  // 依赖数组里的 projects 引用会变化，若无守卫会把已输入的名称/确认勾选清掉。
  const dirtyRef = useRef(false);
  const lastDialogRef = useRef<EntityDialogState | null>(null);

  useEffect(() => {
    if (!dialog) {
      lastDialogRef.current = null;
      dirtyRef.current = false;
      return;
    }
    if (lastDialogRef.current !== dialog) {
      lastDialogRef.current = dialog;
      dirtyRef.current = false;
    }
    if (dirtyRef.current) {
      return;
    }
    setName(initialName(dialog.kind, sourceProject?.name, sourceCase?.name));
    setGoal("");
    setConfirmed(false);
    setTargetProjectId(
      projects.find((project) => project.project_id !== dialog.projectId)?.project_id ?? ""
    );
  }, [dialog, projects, sourceCase?.name, sourceProject?.name]);

  const markDirty = (): void => {
    dirtyRef.current = true;
  };

  const mutation = useMutation({
    mutationFn: async () => {
      if (!dialog) {
        return null;
      }
      switch (dialog.kind) {
        case "createProject":
          return { kind: dialog.kind, result: await api.createProject({ name }) };
        case "renameProject":
          return {
            kind: dialog.kind,
            result: await api.renameProject(required(dialog.projectId), { name })
          };
        case "copyProject":
          return {
            kind: dialog.kind,
            result: await api.copyProject(required(dialog.projectId), { name })
          };
        case "deleteProject":
          return {
            kind: dialog.kind,
            result: await api.deleteProject(required(dialog.projectId), confirmed)
          };
        case "createCase":
          return {
            kind: dialog.kind,
            result: await api.createCase(required(dialog.projectId), {
              name: name || "未命名剪辑任务",
              goal: goal || null,
              brief: { goal }
            })
          };
        case "renameCase":
          return {
            kind: dialog.kind,
            result: await api.renameCase(required(dialog.projectId), required(dialog.caseId), { name })
          };
        case "copyCase":
          return {
            kind: dialog.kind,
            result: await api.copyCase(required(dialog.projectId), required(dialog.caseId), { name })
          };
        case "deleteCase":
          return {
            kind: dialog.kind,
            result: await api.deleteCase(required(dialog.projectId), required(dialog.caseId), confirmed)
          };
        case "moveCase":
          return {
            kind: dialog.kind,
            result: await api.moveCase(required(dialog.projectId), required(dialog.caseId), {
              target_project_id: targetProjectId,
              confirm: confirmed
            })
          };
      }
    },
    onSuccess: async (payload) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.projectTree }),
        queryClient.invalidateQueries({ queryKey: queryKeys.projects })
      ]);
      if (!payload) {
        return;
      }
      if ("project" in payload.result) {
        const projectId = payload.result.project.project_id;
        if (payload.kind === "deleteProject") {
          await navigate({ to: "/" });
        } else {
          await navigate({ to: "/projects/$projectId", params: { projectId } });
        }
      }
      if ("case" in payload.result) {
        const caseRecord = payload.result.case;
        if (payload.kind === "deleteCase") {
          await navigate({
            to: "/projects/$projectId",
            params: { projectId: caseRecord.project_id }
          });
        } else {
          await navigate({
            to: "/projects/$projectId/cases/$caseId",
            params: { projectId: caseRecord.project_id, caseId: caseRecord.case_id }
          });
        }
      }
      onClose();
    }
  });

  if (!dialog) {
    return null;
  }

  const destructive = dialog.kind === "deleteProject" || dialog.kind === "deleteCase";
  const moving = dialog.kind === "moveCase";
  const naming = !destructive && !moving;
  const formReady =
    (destructive && confirmed) ||
    (moving && confirmed && targetProjectId.length > 0) ||
    (naming && name.trim().length > 0);

  return (
    <div className="fixed inset-0 z-20 grid place-items-center bg-black/60 px-4" role="presentation">
      <form
        className="w-full max-w-md rounded-lg border border-line bg-raised p-5"
        onSubmit={(event) => {
          event.preventDefault();
          if (!formReady || mutation.isPending) {
            return;
          }
          mutation.mutate();
        }}
      >
        <h2 className="text-lg font-semibold text-fg">{dialogTitle(dialog.kind)}</h2>

        {naming ? (
          <label className="mt-4 block text-sm font-medium text-fg-muted">
            名称
            <input
              className="mt-2 w-full rounded-md border border-line bg-ink px-3 py-2 text-fg outline-none focus:border-accent"
              value={name}
              onChange={(event) => {
                markDirty();
                setName(event.target.value);
              }}
              autoFocus
            />
          </label>
        ) : null}

        {dialog.kind === "createCase" ? (
          <label className="mt-4 block text-sm font-medium text-fg-muted">
            目标文本
            <textarea
              className="mt-2 h-24 w-full resize-none rounded-md border border-line bg-ink px-3 py-2 text-fg outline-none focus:border-accent"
              value={goal}
              onChange={(event) => {
                markDirty();
                setGoal(event.target.value);
              }}
            />
          </label>
        ) : null}

        {destructive ? (
          <label className="mt-4 flex items-start gap-3 rounded-md border border-danger/40 bg-danger/10 p-3 text-sm text-fg">
            <input
              className="mt-1 accent-[color:var(--color-danger)]"
              type="checkbox"
              checked={confirmed}
              onChange={(event) => {
                markDirty();
                setConfirmed(event.target.checked);
              }}
            />
            确认执行删除。后端会走软删除和同一条归约路径。
          </label>
        ) : null}

        {moving ? (
          <div className="mt-4 space-y-3">
            <label className="block text-sm font-medium text-fg-muted">
              目标项目
              <select
                className="mt-2 w-full rounded-md border border-line bg-ink px-3 py-2 text-fg outline-none focus:border-accent"
                value={targetProjectId}
                onChange={(event) => {
                  markDirty();
                  setTargetProjectId(event.target.value);
                }}
              >
                <option value="" disabled>
                  选择目标项目
                </option>
                {projects
                  .filter((project) => project.project_id !== dialog.projectId)
                  .map((project) => (
                    <option key={project.project_id} value={project.project_id}>
                      {project.name}
                    </option>
                  ))}
              </select>
            </label>
            <label className="flex items-start gap-3 rounded-md border border-line bg-ink p-3 text-sm text-fg">
              <input
                className="mt-1"
                type="checkbox"
                checked={confirmed}
                onChange={(event) => {
                  markDirty();
                  setConfirmed(event.target.checked);
                }}
              />
              确认移动剪辑任务，并让后端处理素材链接归属。
            </label>
          </div>
        ) : null}

        {mutation.error ? (
          <p className="mt-4 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
            操作失败，请检查后端响应。
          </p>
        ) : null}

        <div className="mt-5 flex justify-end gap-2">
          <button
            className="rounded-md border border-line px-3 py-2 text-sm text-fg-muted hover:bg-hover hover:text-fg"
            type="button"
            onClick={onClose}
          >
            取消
          </button>
          <button
            className={`rounded-md px-3 py-2 text-sm font-medium text-white disabled:opacity-40 ${
              destructive ? "bg-danger hover:bg-danger/80" : "bg-accent hover:bg-accent-strong"
            }`}
            type="submit"
            disabled={!formReady || mutation.isPending}
          >
            {mutation.isPending ? "处理中" : "确认"}
          </button>
        </div>
      </form>
    </div>
  );
}

function initialName(
  kind: EntityDialogState["kind"],
  projectName?: string,
  caseName?: string
): string {
  if (kind === "renameProject") {
    return projectName ?? "";
  }
  if (kind === "copyProject") {
    return projectName ? `${projectName} 副本` : "";
  }
  if (kind === "renameCase") {
    return caseName ?? "";
  }
  if (kind === "copyCase") {
    return caseName ? `${caseName} 副本` : "";
  }
  if (kind === "createCase") {
    return "未命名剪辑任务";
  }
  if (kind === "createProject") {
    return "未命名项目";
  }
  return "";
}

function dialogTitle(kind: EntityDialogState["kind"]): string {
  const titles: Record<EntityDialogState["kind"], string> = {
    createProject: "新建项目",
    renameProject: "重命名项目",
    copyProject: "复制项目",
    deleteProject: "删除项目",
    createCase: "新建剪辑任务",
    renameCase: "重命名剪辑任务",
    copyCase: "复制剪辑任务",
    deleteCase: "删除剪辑任务",
    moveCase: "移动剪辑任务"
  };
  return titles[kind];
}

function required(value: string | undefined): string {
  if (!value) {
    throw new Error("缺少必要参数");
  }
  return value;
}
