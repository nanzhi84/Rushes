import type { ReactElement } from "react";
import { AssetsPanel } from "../components/Materials/AssetsPanel";

type ProjectMaterialsViewProps = {
  projectId: string;
  enableEvents?: boolean;
};

/** 项目详情「素材」tab：素材面板的管理模式（文件夹分组 + 单一本地导入 + 管理菜单）。 */
export function ProjectMaterialsView({
  projectId,
  enableEvents = true
}: ProjectMaterialsViewProps): ReactElement {
  return (
    <section className="flex min-h-[70vh] w-full flex-col overflow-hidden rounded-lg border border-line bg-panel">
      <AssetsPanel
        projectId={projectId}
        enableEvents={enableEvents}
        management
        gridClassName="grid grid-cols-3 gap-3 md:grid-cols-4 xl:grid-cols-5"
      />
    </section>
  );
}
