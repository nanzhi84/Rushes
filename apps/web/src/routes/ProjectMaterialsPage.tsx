import type { ReactElement } from "react";
import { Link, useParams } from "@tanstack/react-router";

export function ProjectMaterialsPage(): ReactElement {
  const params = useParams({ strict: false }) as { projectId: string };
  const { projectId } = params;

  return (
    <section className="mx-auto flex w-full max-w-5xl flex-col gap-5 p-6">
      <div>
        <p className="text-sm font-medium text-[#64748b]">素材管理</p>
        <h1 className="mt-2 text-2xl font-semibold">项目级素材页</h1>
      </div>
      <div className="rounded-lg border border-dashed border-[#cbd5e1] bg-white p-8">
        <p className="text-[#475569]">素材池、导入、本地目录浏览和标注状态在下一阶段交付。</p>
        <Link
          className="mt-5 inline-flex rounded-md border border-[#cbd5e1] px-3 py-2 text-sm hover:bg-[#f1f5f9]"
          to="/projects/$projectId"
          params={{ projectId }}
        >
          返回项目首页
        </Link>
      </div>
    </section>
  );
}
