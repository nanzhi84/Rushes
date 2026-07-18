import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { AssistantThread } from "./AssistantThread";
import type { ConsoleAssistantMessage, ConsoleExternalStoreRuntime } from "./runtime";
import type { StreamMessageItem, TurnStreamItem } from "./useTurnStream";

// 打桩 Markdown，用渲染次数观测「历史消息行在流式高频重渲染期间是否被 memo 挡下」。
const markdownSpy = vi.hoisted(() => vi.fn());
vi.mock("./Markdown", () => ({
  Markdown: ({ text }: { text: string }) => {
    markdownSpy(text);
    return <div data-testid="md-body-stub">{text}</div>;
  }
}));

const onAnswerDecision = vi.fn();
const submit = vi.fn();
const history: ConsoleAssistantMessage[] = [
  {
    id: "a1",
    role: "assistant",
    createdAt: "2026-07-18T00:00:00Z",
    metadata: { consoleRole: "assistant", messageKind: "reply" },
    content: [{ type: "text", text: "历史助手正文" }]
  }
];

function runtime(): ConsoleExternalStoreRuntime {
  return { messages: history, isRunning: true, canSubmit: false, submit };
}

function view(streamText: string) {
  const streaming: StreamMessageItem = {
    type: "message",
    message_id: "m1",
    kind: "assistant",
    text: streamText
  };
  const streamItems: TurnStreamItem[] = [streaming];
  return (
    <AssistantThread
      runtime={runtime()}
      onAnswerDecision={onAnswerDecision}
      answerPending={false}
      streamItems={streamItems}
    />
  );
}

describe("AssistantThread 行 memo 隔离流式重渲染", () => {
  it("流式 delta 连续更新时，props 未变的历史消息行不再重渲染（Markdown 不重跑）", () => {
    const { rerender } = render(view("流式一"));
    // 历史助手消息首次渲染跑一次 Markdown。
    expect(markdownSpy).toHaveBeenCalledTimes(1);

    markdownSpy.mockClear();
    // 模拟多个 text_delta：仅流式消息在增长（纯文本渲染，不经 Markdown），
    // 历史行 props 不变 → memo 挡下重渲染 → Markdown 零重跑。
    rerender(view("流式一二"));
    rerender(view("流式一二三"));
    rerender(view("流式一二三四"));

    expect(markdownSpy).toHaveBeenCalledTimes(0);
  });
});
