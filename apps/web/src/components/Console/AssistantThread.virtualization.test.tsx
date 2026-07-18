import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AssistantThread } from "./AssistantThread";
import type { ConsoleAssistantMessage, ConsoleExternalStoreRuntime } from "./runtime";
import type { StreamMessageItem, TurnStreamItem } from "./useTurnStream";

function runtime(messages: ConsoleAssistantMessage[], isRunning = false): ConsoleExternalStoreRuntime {
  return { messages, isRunning, canSubmit: !isRunning, submit: vi.fn() };
}

function userMessage(index: number): ConsoleAssistantMessage {
  return {
    id: `u${index}`,
    role: "user",
    createdAt: "2026-07-18T00:00:00Z",
    metadata: { consoleRole: "user", messageKind: "user" },
    content: [{ type: "text", text: `历史消息 ${index}` }]
  };
}

function renderThread(messages: ConsoleAssistantMessage[], streamItems: TurnStreamItem[] = [], isRunning = false) {
  return render(
    <AssistantThread
      runtime={runtime(messages, isRunning)}
      onAnswerDecision={vi.fn()}
      answerPending={false}
      streamItems={streamItems}
    />
  );
}

describe("AssistantThread 流式渲染降级与虚拟化", () => {
  afterEach(() => vi.restoreAllMocks());

  it("流式期间助手正文用纯文本渲染，不跑 Markdown（避免每 delta O(N²) 重解析）", () => {
    const streaming: StreamMessageItem = {
      type: "message",
      message_id: "m1",
      kind: "assistant",
      text: "**加粗** 与 # 井号标题"
    };
    const { container } = renderThread([], [streaming], true);

    // 原始 Markdown 记号按纯文本原样出现，且没有生成 <strong>/<h1>。
    expect(screen.getByText("**加粗** 与 # 井号标题")).toBeTruthy();
    expect(container.querySelector("strong")).toBeNull();
    expect(container.querySelector("h1")).toBeNull();
    expect(container.querySelector('[data-streaming="true"]')).toBeTruthy();
  });

  it("落库后的助手消息一次性 Markdown 化", async () => {
    const completed: ConsoleAssistantMessage = {
      id: "a1",
      role: "assistant",
      createdAt: "2026-07-18T00:00:00Z",
      metadata: { consoleRole: "assistant", messageKind: "reply" },
      content: [{ type: "text", text: "**加粗** 收尾" }]
    };
    const { container } = renderThread([completed]);

    // 历史消息（streaming=false）走 Markdown（懒加载，等按需 chunk 就绪）：加粗解析成 <strong>。
    await waitFor(() =>
      expect(container.querySelector(".md-body strong")?.textContent).toBe("加粗")
    );
    expect(screen.queryByText("**加粗** 收尾")).toBeNull();
  });

  // react-virtual 的逐行测量依赖真实布局，jsdom 无布局、其内部调度也不在 RTL act 内 flush，
  // 因此这里断言「已切到虚拟化容器、未把全部行铺进扁平列表」这一结构契约；真实窗口化（视口内
  // 少量行、滚动增删行）由真实浏览器 e2e / Playwright 验证。
  it("超过阈值的长会话切到虚拟化容器，不再把全部行铺进扁平列表", () => {
    const messages = Array.from({ length: 60 }, (_, index) => userMessage(index));
    const { container } = renderThread(messages);
    const scroller = container.querySelector('[aria-label="消息列表"]') as HTMLElement;

    // 走虚拟化路径：滚动容器内是带高度的定位占位层，而非 space-y-2.5 扁平列表。
    expect(container.querySelector(".space-y-2\\.5")).toBeNull();
    const spacer = scroller.firstElementChild as HTMLElement;
    expect(spacer.style.position).toBe("relative");
    expect(Number.parseInt(spacer.style.height, 10)).toBeGreaterThan(0);
    // 60 条历史未被全量铺进 DOM。
    expect(container.querySelectorAll("[data-user-message]").length).toBeLessThan(60);
  });

  it("阈值以内的会话不虚拟化，全部行进扁平列表 DOM（既有行为不变）", () => {
    const messages = Array.from({ length: 8 }, (_, index) => userMessage(index));
    const { container } = renderThread(messages);
    expect(container.querySelector(".space-y-2\\.5")).not.toBeNull();
    expect(container.querySelectorAll("[data-user-message]")).toHaveLength(8);
    expect(screen.getByText("历史消息 7")).toBeTruthy();
  });
});
