import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api, type ProjectTreeProject } from "../api/client";
import { queryKeys } from "../app/query_client";
import type { TreeDialogState } from "../state/ui_store";

type TreeMutationDialogProps = {
  dialog: TreeDialogState | null;
  projects: ProjectTreeProject[];
  onClose: () => void;
};

export function TreeMutationDialog({
  dialog,
  projects,
  onClose
}: TreeMutationDialogProps): ReactElement | null {
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

  useEffect(() => {
    if (!dialog) {
      return;
    }
    setName(initialName(dialog.kind, sourceProject?.name, sourceCase?.name));
    setGoal("");
    setConfirmed(false);
    setTargetProjectId(
      projects.find((project) => project.project_id !== dialog.projectId)?.project_id ?? ""
    );
  }, [dialog, projects, sourceCase?.name, sourceProject?.name]);

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
    <div className="fixed inset-0 z-20 grid place-items-center bg-black/20 px-4" role="presentation">
      <form
        className="w-full max-w-md rounded-lg border border-[#d9dee7] bg-white p-5 shadow-xl"
        onSubmit={(event) => {
          event.preventDefault();
          if (!formReady || mutation.isPending) {
            return;
          }
          mutation.mutate();
        }}
      >
        <h2 className="text-lg font-semibold text-[#17202a]">{dialogTitle(dialog.kind)}</h2>

        {naming ? (
          <label className="mt-4 block text-sm font-medium text-[#334155]">
            名称
            <input
              className="mt-2 w-full rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb]"
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoFocus
            />
          </label>
        ) : null}

        {dialog.kind === "createCase" ? (
          <label className="mt-4 block text-sm font-medium text-[#334155]">
            目标文本
            <textarea
              className="mt-2 h-24 w-full resize-none rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb]"
              value={goal}
              onChange={(event) => setGoal(event.target.value)}
            />
          </label>
        ) : null}

        {destructive ? (
          <label className="mt-4 flex items-start gap-3 rounded-md bg-[#fff7ed] p-3 text-sm text-[#7c2d12]">
            <input
              className="mt-1"
              type="checkbox"
              checked={confirmed}
              onChange={(event) => setConfirmed(event.target.checked)}
            />
            确认执行删除。后端会走软删除和同一条归约路径。
          </label>
        ) : null}

        {moving ? (
          <div className="mt-4 space-y-3">
            <label className="block text-sm font-medium text-[#334155]">
              目标项目
              <select
                className="mt-2 w-full rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb]"
                value={targetProjectId}
                onChange={(event) => setTargetProjectId(event.target.value)}
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
            <label className="flex items-start gap-3 rounded-md bg-[#eef6ff] p-3 text-sm text-[#1e3a8a]">
              <input
                className="mt-1"
                type="checkbox"
                checked={confirmed}
                onChange={(event) => setConfirmed(event.target.checked)}
              />
              确认移动剪辑任务，并让后端处理素材链接归属。
            </label>
          </div>
        ) : null}

        {mutation.error ? (
          <p className="mt-4 rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">
            操作失败，请检查后端响应。
          </p>
        ) : null}

        <div className="mt-5 flex justify-end gap-2">
          <button
            className="rounded-md border border-[#cbd5e1] px-3 py-2 text-sm text-[#334155] hover:bg-[#f1f5f9]"
            type="button"
            onClick={onClose}
          >
            取消
          </button>
          <button
            className="rounded-md bg-[#17202a] px-3 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
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

function initialName(kind: TreeDialogState["kind"], projectName?: string, caseName?: string): string {
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

function dialogTitle(kind: TreeDialogState["kind"]): string {
  const titles: Record<TreeDialogState["kind"], string> = {
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
