import type { ReactElement } from "react";

// 地基占位：首页草稿墙。后续页面代理由 ProjectsOverview 改造为草稿列表 + 封面卡片 +
//「开始创作」按钮（createDraft → 跳编辑器）。此处仅渲染加载骨架，保证路由与类型可编译。
export function DraftsHomePage(): ReactElement {
  return (
    <main aria-busy="true" style={{ padding: 24 }}>
      加载中…
    </main>
  );
}
