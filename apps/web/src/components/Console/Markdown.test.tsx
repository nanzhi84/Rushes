import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

// 模拟 chunk 加载失败（网络抖动 / 重新构建后旧 hash 404）：动态 import 会 reject。
// Suspense 只接管加载中、接不住 rejection，lazy 还会缓存失败——Markdown 的 loader
// 必须自行 catch 并退回纯文本渲染，否则错误冒泡到裸 createRoot 根导致整应用白屏。
vi.mock("react-markdown", () => {
  throw new Error("chunk load failed");
});

import { Markdown } from "./Markdown";

describe("Markdown 懒加载失败兜底", () => {
  it("react-markdown chunk 加载失败时退回纯文本渲染而非抛错白屏", async () => {
    render(<Markdown text="**加粗** 原文" />);
    await waitFor(() => {
      expect(screen.getByText("**加粗** 原文")).toBeTruthy();
    });
    expect(document.querySelector(".md-body p.whitespace-pre-wrap")).not.toBeNull();
    expect(document.querySelector("strong")).toBeNull();
  });
});
