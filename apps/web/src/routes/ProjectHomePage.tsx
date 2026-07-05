import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import type { ReactElement } from "react";
import { api } from "../api/client";
import { queryKeys } from "../app/query_client";

export function ProjectHomePage(): ReactElement {
  const params = useParams({ strict: false }) as { projectId: string };
  const { projectId } = params;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [goal, setGoal] = useState("");
  const treeQuery = useQuery({
    queryKey: queryKeys.projectTree,
    queryFn: api.projectTree
  });

  const project = useMemo(
    () => treeQuery.data?.projects.find((item) => item.project_id === projectId) ?? null,
    [projectId, treeQuery.data?.projects]
  );

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
    <section className="mx-auto flex w-full max-w-5xl flex-col gap-6 p-6">
      <div>
        <p className="text-sm font-medium text-[#64748b]">项目首页</p>
        <h1 className="mt-2 text-2xl font-semibold">{project?.name ?? projectId}</h1>
      </div>

      <form
        className="rounded-lg border border-[#d9dee7] bg-white p-5"
        onSubmit={(event) => {
          event.preventDefault();
          createCase.mutate();
        }}
      >
        <h2 className="text-lg font-semibold">新建剪辑任务</h2>
        <label className="mt-4 block text-sm font-medium text-[#334155]">
          目标文本
          <textarea
            className="mt-2 h-28 w-full resize-none rounded-md border border-[#cbd5e1] px-3 py-2 outline-none focus:border-[#2563eb]"
            value={goal}
            onChange={(event) => setGoal(event.target.value)}
            placeholder="例如：剪一条 30 秒种草视频"
          />
        </label>
        <div className="mt-4 flex justify-end">
          <button
            className="rounded-md bg-[#17202a] px-4 py-2 text-sm font-medium text-white disabled:bg-[#94a3b8]"
            type="submit"
            disabled={goal.trim().length === 0 || createCase.isPending}
          >
            创建并进入控制台
          </button>
        </div>
      </form>

      <div className="grid gap-4 md:grid-cols-2">
        <Link
          className="rounded-lg border border-[#d9dee7] bg-white p-5 text-[#17202a] hover:border-[#94a3b8]"
          to="/projects/$projectId/materials"
          params={{ projectId }}
        >
          <span className="text-lg font-semibold">素材管理</span>
          <span className="mt-2 block text-sm text-[#64748b]">查看素材池、导入素材和标注状态。</span>
        </Link>
        <section className="rounded-lg border border-[#d9dee7] bg-white p-5">
          <h2 className="text-lg font-semibold">项目设置</h2>
          <p className="mt-2 text-sm text-[#64748b]">成本汇总与默认策略后续接入。</p>
        </section>
      </div>
    </section>
  );
}

function caseNameFromGoal(goal: string): string {
  const trimmed = goal.trim();
  if (!trimmed) {
    return "未命名剪辑任务";
  }
  return trimmed.length > 28 ? `${trimmed.slice(0, 28)}...` : trimmed;
}
