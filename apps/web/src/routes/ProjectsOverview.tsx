import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import type { ReactElement } from "react";
import { api } from "../api/client";
import { queryKeys } from "../app/query_client";

export function ProjectsOverviewPage(): ReactElement {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [name, setName] = useState("未命名项目");
  const projectsQuery = useQuery({
    queryKey: queryKeys.projects,
    queryFn: api.listProjects
  });

  const createProject = useMutation({
    mutationFn: () => api.createProject({ name }),
    onSuccess: async (payload) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.projects }),
        queryClient.invalidateQueries({ queryKey: queryKeys.projectTree })
      ]);
      await navigate({
        to: "/projects/$projectId",
        params: { projectId: payload.project.project_id }
      });
    }
  });

  return (
    <section className="mx-auto flex w-full max-w-6xl flex-col gap-6 p-6">
      <div>
        <h1 className="text-2xl font-semibold">项目总览</h1>
        <p className="mt-2 text-sm text-[#64748b]">选择一个项目，或从这里创建新的剪辑工作区。</p>
      </div>

      <form
        className="flex flex-wrap items-end gap-3 rounded-lg border border-[#d9dee7] bg-white p-4"
        onSubmit={(event) => {
          event.preventDefault();
          createProject.mutate();
        }}
      >
        <label className="min-w-64 flex-1 text-sm font-medium text-[#334155]">
          项目名称
          <input
            className="mt-2 w-full rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb]"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </label>
        <button
          className="rounded-md bg-[#17202a] px-4 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
          type="submit"
          disabled={name.trim().length === 0 || createProject.isPending}
        >
          新建项目
        </button>
      </form>

      {projectsQuery.isLoading ? (
        <p className="text-sm text-[#64748b]">正在读取项目</p>
      ) : projectsQuery.error ? (
        <p className="rounded-md bg-[#fee4e2] px-3 py-2 text-sm text-[#b42318]">
          项目列表加载失败
        </p>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {(projectsQuery.data?.projects ?? []).map((project) => (
            <button
              key={project.project_id}
              className="min-h-32 rounded-lg border border-[#d9dee7] bg-white p-4 text-left shadow-sm hover:border-[#94a3b8]"
              type="button"
              onClick={() =>
                void navigate({
                  to: "/projects/$projectId",
                  params: { projectId: project.project_id }
                })
              }
            >
              <span className="block truncate text-lg font-semibold">{project.name}</span>
              <span className="mt-3 inline-block rounded bg-[#eef2f7] px-2 py-1 text-xs text-[#475569]">
                {project.status}
              </span>
            </button>
          ))}
        </div>
      )}
    </section>
  );
}
