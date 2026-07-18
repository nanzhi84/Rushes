import { render } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { AssistantThread } from "./AssistantThread";
import type { ConsoleAssistantMessage, ConsoleExternalStoreRuntime } from "./runtime";
import type { StreamMessageItem, TurnStreamItem } from "./useTurnStream";

// 打桩 Markdown 与 lucide 的 ChevronRight，用渲染次数观测「历史行在流式高频重渲染期间
// 是否被 memo 挡下」：Markdown 只在助手正文行出现，ChevronRight（在本用例里）只在工具组出现。
const markdownSpy = vi.hoisted(() => vi.fn());
const chevronSpy = vi.hoisted(() => vi.fn());
vi.mock("./Markdown", () => ({
  Markdown: ({ text }: { text: string }) => {
    markdownSpy(text);
    return <div data-testid="md-body-stub">{text}</div>;
  }
}));
vi.mock("lucide-react", async (importOriginal) => {
  const actual = await importOriginal<typeof import("lucide-react")>();
  return {
    ...actual,
    ChevronRight: (props: Record<string, unknown>) => {
      chevronSpy();
      return createElement(actual.ChevronRight, props);
    }
  };
});

const onAnswerDecision = vi.fn();
const submit = vi.fn();

// 关键：传入稳定的 messages 数组与对象（模拟 F3 修复后 runtime.messages 在流式增量间稳定）。
function runtimeOf(messages: ConsoleAssistantMessage[]): ConsoleExternalStoreRuntime {
  return { messages, isRunning: true, canSubmit: false, submit };
}

function view(messages: ConsoleAssistantMessage[], streamText: string) {
  const streaming: StreamMessageItem = { type: "message", message_id: "m1", kind: "assistant", text: streamText };
  const streamItems: TurnStreamItem[] = [streaming];
  return (
    <AssistantThread
      runtime={runtimeOf(messages)}
      onAnswerDecision={onAnswerDecision}
      answerPending={false}
      streamItems={streamItems}
    />
  );
}

describe("AssistantThread 行 memo 隔离流式重渲染", () => {
  it("流式 delta 连续更新时，助手正文行不再重渲染（Markdown 不重跑）", () => {
    const history: ConsoleAssistantMessage[] = [
      {
        id: "a1",
        role: "assistant",
        createdAt: "2026-07-18T00:00:00Z",
        metadata: { consoleRole: "assistant", messageKind: "reply" },
        content: [{ type: "text", text: "历史助手正文" }]
      }
    ];
    markdownSpy.mockClear();
    const { rerender } = render(view(history, "流式一"));
    expect(markdownSpy).toHaveBeenCalledTimes(1);

    markdownSpy.mockClear();
    rerender(view(history, "流式一二"));
    rerender(view(history, "流式一二三"));
    rerender(view(history, "流式一二三四"));
    expect(markdownSpy).toHaveBeenCalledTimes(0);
  });

  it("流式 delta 连续更新时，历史工具组不再重渲染（memo 未被每帧新建的 block.steps 击穿）", () => {
    // 一条持久化 tool 消息 → 历史里的折叠工具组；本用例中只有它渲染 ChevronRight。
    const history: ConsoleAssistantMessage[] = [
      {
        id: "step1",
        role: "system",
        createdAt: "2026-07-18T00:00:00Z",
        metadata: { consoleRole: "system", messageKind: "tool" },
        content: [
          {
            type: "text",
            text: JSON.stringify({
              step_id: "step1",
              tool: "render.preview",
              status: "succeeded",
              args_summary: "{\"v\":1}",
              observation: "{\"ok\":true}"
            })
          }
        ]
      }
    ];
    chevronSpy.mockClear();
    const { rerender } = render(view(history, "流式一"));
    expect(chevronSpy.mock.calls.length).toBeGreaterThan(0); // 挂载时工具组渲染过 ChevronRight

    chevronSpy.mockClear();
    // 多个 text_delta：历史工具组 props 未变（block.steps 引用稳定）→ 不重渲染。
    rerender(view(history, "流式一二"));
    rerender(view(history, "流式一二三"));
    rerender(view(history, "流式一二三四"));
    expect(chevronSpy).toHaveBeenCalledTimes(0);
  });
});
