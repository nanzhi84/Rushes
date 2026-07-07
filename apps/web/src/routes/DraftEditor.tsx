import { useParams } from "@tanstack/react-router";
import type { ReactElement } from "react";

// 地基占位：草稿编辑器（单 draftId）。后续页面代理由 CaseAgentConsole 改造为四区布局
//（左对话 / 中素材 / 右播放器 / 底部只读时间线）。此处仅渲染加载骨架，保证路由与类型可编译。
export function DraftEditorPage(): ReactElement {
  const { draftId } = useParams({ from: "/drafts/$draftId" });
  return (
    <main aria-busy="true" data-draft-id={draftId} style={{ padding: 24 }}>
      加载中…
    </main>
  );
}
