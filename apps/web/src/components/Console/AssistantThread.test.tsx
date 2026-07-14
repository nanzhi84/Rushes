import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AssistantThread } from "./AssistantThread";
import type {
  ConsoleAssistantMessage,
  ConsoleExternalStoreRuntime
} from "./runtime";
import type { StreamMessageItem, TurnStreamItem } from "./useTurnStream";

describe("AssistantThread Claude Code 式消息流", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("真实事件间隙也显示动态阶段与递增耗时", () => {
    vi.useFakeTimers();
    const view = renderThread({ isRunning: true });

    const indicator = screen.getByTestId("turn-activity-indicator");
    expect(indicator.getAttribute("data-turn-activity")).toBe("正在读取上下文");
    expect(screen.getByText(/00:00/)).toBeTruthy();

    act(() => vi.advanceTimersByTime(3_200));
    expect(screen.getByText(/00:03/)).toBeTruthy();

    view.rerender(
      <AssistantThread
        runtime={runtime([], true)}
        onAnswerDecision={vi.fn()}
        answerPending={false}
        streamItems={[
          {
            type: "tool",
            step_id: "s1",
            tool: "understand.materials",
            status: "running",
            argsSummary: null,
            observation: null
          }
        ]}
      />
    );
    expect(indicator.getAttribute("data-turn-activity")).toBe("正在理解素材");
    expect(screen.getByText(/00:03/)).toBeTruthy();

    view.rerender(
      <AssistantThread
        runtime={runtime([], true)}
        onAnswerDecision={vi.fn()}
        answerPending={false}
        streamItems={[
          {
            type: "tool",
            step_id: "s2",
            tool: "media.search_shots",
            status: "running",
            argsSummary: null,
            observation: null
          }
        ]}
      />
    );
    expect(indicator.getAttribute("data-turn-activity")).toBe("正在检索镜头");
  });

  it("像 Claude Code 一样持续显示模型超时重试序号", () => {
    renderThread({
      isRunning: true,
      modelRetry: {
        attempt: 3,
        maxRetries: 5,
        reason: "模型响应超时",
        nextDelayMs: 1000
      }
    });

    const indicator = screen.getByTestId("turn-activity-indicator");
    expect(indicator.getAttribute("data-turn-activity")).toBe("模型响应超时，正在重试 3/5");
    expect(screen.getByText("模型响应超时，正在重试 3/5")).toBeTruthy();
  });

  it("把连续后台回调折叠并合并重复文案", () => {
    renderThread({
      messages: [
        message("user", "u1", "生成一条混剪", "user"),
        message("assistant", "a1", "素材已排到时间线。", "reply"),
        message("assistant", "o1", "后台任务已完成，我已读取结果并继续推进。", "reply"),
        message("assistant", "o2", "后台任务已完成，我已读取结果并继续推进。", "reply"),
        message("assistant", "o3", "后台任务已完成，我已读取结果并继续推进。", "observation")
      ]
    });

    const group = screen.getByTestId("background-activity-group") as HTMLDetailsElement;
    expect(group.open).toBe(false);
    expect(group.getAttribute("data-layout")).toBe("inline");
    expect(group.getAttribute("data-background-count")).toBe("3");
    expect(screen.getByText("3 条")).toBeTruthy();
    expect(screen.getByText("×3")).toBeTruthy();
    expect(screen.getAllByText("后台任务已完成，我已读取结果并继续推进。")).toHaveLength(1);
  });

  it("运行中的工具组自动展开，完成后折叠，结果仍可手动查看", async () => {
    const running: TurnStreamItem[] = [
      {
        type: "tool",
        step_id: "s1",
        tool: "understand.materials",
        status: "running",
        argsSummary: "{\"asset_ids\":[\"a1\"]}",
        observation: null
      },
      {
        type: "tool",
        step_id: "s2",
        tool: "timeline.compose_initial",
        status: "running",
        argsSummary: null,
        observation: null
      }
    ];
    const view = renderThread({
      streamItems: running,
      subagentProgress: [{ asset_id: "a1", note: "正在查看 demo.mp4 画面" }],
      isRunning: true
    });

    const group = screen.getByTestId("tool-activity-group") as HTMLDetailsElement;
    expect(group.open).toBe(true);
    expect(group.getAttribute("data-layout")).toBe("inline");
    expect(screen.getByText("正在使用工具")).toBeTruthy();
    expect(screen.getByText("正在查看 demo.mp4 画面")).toBeTruthy();

    view.rerender(
      <AssistantThread
        runtime={runtime([], false)}
        onAnswerDecision={vi.fn()}
        answerPending={false}
        streamItems={running.map((item) =>
          item.type === "tool"
            ? { ...item, status: "succeeded", observation: "{\"ok\":true}" }
            : item
        )}
      />
    );

    await waitFor(() => expect(group.open).toBe(false));
    expect(screen.getByText("已使用工具")).toBeTruthy();
    fireEvent.click(group.querySelector(":scope > summary")!);
    await waitFor(() => expect(group.open).toBe(true));
    expect(screen.getAllByText("结果")).toHaveLength(2);
  });

  it("刷新后把持久化 tool 消息恢复成折叠工具组", () => {
    renderThread({
      messages: [
        message(
          "system",
          "step_saved",
          JSON.stringify({
            step_id: "step_saved",
            tool: "render.preview",
            status: "succeeded",
            args_summary: "{\"timeline_version\":3}",
            observation: "{\"preview_id\":\"p3\"}"
          }),
          "tool"
        )
      ]
    });

    const group = screen.getByTestId("tool-activity-group") as HTMLDetailsElement;
    expect(group.open).toBe(false);
    expect(screen.getByText("已使用工具")).toBeTruthy();
    expect(screen.getAllByText("渲染预览")).toHaveLength(2);
  });

  it("用户消息使用源站的右侧窄灰气泡，助手正文不套卡片", () => {
    renderThread({
      messages: [
        message("user", "u1", "把节奏剪快一点", "user"),
        message("assistant", "a1", "我会先检查时间线。", "reply")
      ]
    });

    const userBubble = screen.getByText("把节奏剪快一点").closest("[data-user-message]");
    expect(userBubble).toBeTruthy();
    expect(userBubble?.className).toContain("max-w-[85%]");
    expect(userBubble?.className).toContain("bg-user-bubble");

    const assistant = screen.getByText("我会先检查时间线。").closest("article");
    expect(assistant?.className).not.toContain("border");
    expect(assistant?.className).not.toContain("bg-raised");
  });

  it("把同一结构化消息里的多个问题合并成一个紧凑问答组", () => {
    const answerOne = {
      free_text: "Tim-Macbook Neo Talking节选.mp4",
      answered_via: "natural_language" as const,
      payload: {}
    };
    const answerTwo = {
      free_text: "先剪气口，再配 B-roll",
      answered_via: "natural_language" as const,
      payload: {}
    };
    renderThread({
      messages: [
        {
          id: "structured-interactions",
          role: "assistant",
          createdAt: "2026-07-11T00:00:00Z",
          metadata: { consoleRole: "assistant" },
          content: [
            {
              type: "data",
              data: {
                kind: "decision",
                id: "decision:one",
                decision_id: "one",
                decision: {
                  decision_id: "one",
                  scope_type: "draft",
                  draft_id: "draft_1",
                  type: "generic",
                  question: "哪个视频是口播主素材？",
                  options: [],
                  allow_free_text: true,
                  blocking: true,
                  status: "answered",
                  answer: answerOne
                },
                status: "answered",
                answer: answerOne
              }
            },
            {
              type: "data",
              data: {
                kind: "decision",
                id: "decision:two",
                decision_id: "two",
                decision: {
                  decision_id: "two",
                  scope_type: "draft",
                  draft_id: "draft_1",
                  type: "generic",
                  question: "转录不可用时如何处理？",
                  options: [],
                  allow_free_text: true,
                  blocking: true,
                  status: "answered",
                  answer: answerTwo
                },
                status: "answered",
                answer: answerTwo
              }
            }
          ]
        }
      ]
    });

    expect(screen.getAllByTestId("decision-group")).toHaveLength(1);
    expect(screen.getAllByTestId("decision-question")).toHaveLength(2);
    expect(screen.getByText("已回答 2 个问题")).toBeTruthy();
    expect(screen.getByText("问题 1")).toBeTruthy();
    expect(screen.getByText("问题 2")).toBeTruthy();
    expect(screen.getAllByText("回答")).toHaveLength(2);
    expect(screen.getAllByTestId("decision-answer")).toHaveLength(2);
    expect(screen.getByText("Tim-Macbook Neo Talking节选.mp4")).toBeTruthy();
    expect(screen.getByText("先剪气口，再配 B-roll")).toBeTruthy();
    expect(screen.queryByText("你的回答")).toBeNull();
  });

  it("增量回复带流式状态，并只在 follow mode 下追随底部", async () => {
    const first: StreamMessageItem = {
      type: "message",
      message_id: "m1",
      kind: "assistant",
      text: "正在分析"
    };
    const view = renderThread({ streamItems: [first], isRunning: true });
    const scroller = screen.getByLabelText("消息列表");
    Object.defineProperties(scroller, {
      scrollHeight: { configurable: true, value: 300 },
      clientHeight: { configurable: true, value: 100 }
    });

    view.rerender(
      <AssistantThread
        runtime={runtime([], true)}
        onAnswerDecision={vi.fn()}
        answerPending={false}
        streamItems={[{ ...first, text: "正在分析素材" }]}
      />
    );
    await waitFor(() => expect(scroller.scrollTop).toBe(300));
    expect(scroller.querySelector('[data-streaming="true"]')).toBeTruthy();
    expect(screen.getByText("正在生成")).toBeTruthy();

    scroller.scrollTop = 0;
    fireEvent.scroll(scroller);
    view.rerender(
      <AssistantThread
        runtime={runtime([], true)}
        onAnswerDecision={vi.fn()}
        answerPending={false}
        streamItems={[{ ...first, text: "正在分析素材和声音" }]}
      />
    );
    await waitFor(() => expect(screen.getByText("查看最新输出")).toBeTruthy());
    expect(scroller.scrollTop).toBe(0);

    fireEvent.click(screen.getByText("查看最新输出"));
    expect(scroller.scrollTop).toBe(300);
  });
});

function renderThread({
  messages = [],
  streamItems = [],
  modelRetry = null,
  subagentProgress = [],
  isRunning = false
}: {
  messages?: ConsoleAssistantMessage[];
  streamItems?: TurnStreamItem[];
  modelRetry?: {
    attempt: number;
    maxRetries: number;
    reason: string;
    nextDelayMs: number | null;
  } | null;
  subagentProgress?: Array<{ asset_id: string; note: string }>;
  isRunning?: boolean;
}) {
  return render(
    <AssistantThread
      runtime={runtime(messages, isRunning)}
      onAnswerDecision={vi.fn()}
      answerPending={false}
      streamItems={streamItems}
      modelRetry={modelRetry}
      subagentProgress={subagentProgress}
    />
  );
}

function runtime(
  messages: ConsoleAssistantMessage[],
  isRunning: boolean
): ConsoleExternalStoreRuntime {
  return {
    messages,
    isRunning,
    canSubmit: !isRunning,
    submit: vi.fn()
  };
}

function message(
  role: "user" | "assistant" | "system",
  id: string,
  text: string,
  kind: string
): ConsoleAssistantMessage {
  return {
    id,
    role,
    createdAt: "2026-07-11T00:00:00Z",
    metadata: { consoleRole: role, messageKind: kind },
    content: [{ type: "text", text }]
  };
}
