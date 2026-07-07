import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterContextProvider
} from "@tanstack/react-router";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Decision, DecisionAnswer } from "../api/client";
import { storeAuthToken } from "../auth";
import {
  itemFromEvent,
  reduceStructuredInteractionItems,
  StructuredInteractionRenderer
} from "../components/Console/StructuredInteractionRenderer";
import type { DomainSsePayload } from "../components/Console/StructuredInteractionRenderer";
import { DEFAULT_MATERIALS_PANEL_WIDTH, useUiStore } from "../state/ui_store";
import { DraftEditorView } from "./DraftEditor";

type MockPreviewProps = {
  seekSec?: number | null;
  onTimeUpdate?: (sec: number) => void;
};

type MockTimelineProps = {
  playheadSec?: number | null;
  pxPerSec?: number;
  onSeek?: (sec: number) => void;
};

const consoleComponentMocks = vi.hoisted(() => {
  const previewProps: MockPreviewProps[] = [];
  const timelineProps: MockTimelineProps[] = [];
  return {
    previewProps,
    timelineProps,
    reset() {
      previewProps.length = 0;
      timelineProps.length = 0;
    }
  };
});

vi.mock("../components/PreviewPlayer", async () => {
  const React = await import("react");
  return {
    PreviewPlayer(props: MockPreviewProps) {
      consoleComponentMocks.previewProps.push(props);
      return React.createElement(
        "button",
        {
          type: "button",
          "data-testid": "mock-preview",
          onClick: () => props.onTimeUpdate?.(1.25)
        },
        "Mock Preview"
      );
    }
  };
});

vi.mock("../components/TimelineViewer", async () => {
  const React = await import("react");
  return {
    TimelineViewer(props: MockTimelineProps) {
      consoleComponentMocks.timelineProps.push(props);
      return React.createElement(
        "button",
        {
          type: "button",
          "data-testid": "mock-timeline-seek",
          onClick: () => props.onSeek?.(2.5)
        },
        "Mock Timeline"
      );
    }
  };
});

type Listener = (event: MessageEvent<string>) => void;
type FetchMock = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

class MockEventSource {
  static instances: MockEventSource[] = [];
  readonly listeners = new Map<string, Listener[]>();
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readonly url: string;

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: EventListenerOrEventListenerObject): void {
    const fn = listener as Listener;
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn]);
  }

  removeEventListener(type: string, listener: EventListenerOrEventListenerObject): void {
    const fn = listener as Listener;
    this.listeners.set(
      type,
      (this.listeners.get(type) ?? []).filter((item) => item !== fn)
    );
  }

  close(): void {
    return;
  }

  emit(type: string, data: unknown): void {
    const event = new MessageEvent(type, { data: JSON.stringify(data) });
    for (const listener of this.listeners.get(type) ?? []) {
      listener(event);
    }
  }
}

describe("DraftEditorView", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
    window.localStorage.clear();
    useUiStore.setState({ materialsPanelWidth: DEFAULT_MATERIALS_PANEL_WIDTH });
    MockEventSource.instances = [];
    consoleComponentMocks.reset();
  });

  it("时间线是纵向根布局的直属子级：与三栏行同级，全宽通栏", () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const timeline = screen.getByLabelText("时间线");
    const chat = screen.getByLabelText("剪辑对话");
    // 聊天在三栏行内部；三栏行与时间线共享同一个纵向根容器 → 时间线不再被"右列"包裹。
    const threeColumnRow = chat.parentElement;
    expect(threeColumnRow?.parentElement).toBe(timeline.parentElement);
    expect(timeline.contains(chat)).toBe(false);
  });

  it("素材列可拖宽：拖宽手柄存在，宽度受 ui_store 驱动", () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    expect(screen.getByLabelText("调整素材面板宽度")).toBeTruthy();
    const panel = screen.getByTestId("materials-panel");
    expect(panel.style.width).toBe(`${DEFAULT_MATERIALS_PANEL_WIDTH}px`);

    act(() => {
      useUiStore.getState().setMaterialsPanelWidth(420);
    });
    expect(panel.style.width).toBe("420px");
  });

  it("顶栏成本小计渲染估算金额，且编辑器隐藏设置按钮", async () => {
    const fetchMock = mockFetch({ decision: null, costs: 1.2345 });
    renderEditor(fetchMock);

    expect(await screen.findByText("¥1.2345")).toBeTruthy();
    expect(screen.queryByText("设置")).toBeNull();
  });

  it("发送消息后禁用输入框，并在 TurnEnded SSE 后恢复", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "剪掉开头 3 秒" } });
    fireEvent.click(screen.getByText("发送"));

    await waitFor(() => expect(input.disabled).toBe(true));
    expect(screen.getByText("剪掉开头 3 秒")).toBeTruthy();
    expect(draftEventsSource().url).toContain("token=test-token");

    act(() => {
      draftEventsSource().emit("TurnEnded", {
        event_id: 1,
        event: {
          event: "TurnEnded",
          draft_id: "draft_1",
          turn_id: "turn_1"
        }
      });
    });

    await waitFor(() => expect(input.disabled).toBe(false));
  });

  it("回放历史消息并弱化 narration 叙述", async () => {
    const fetchMock = mockFetch({
      decision: null,
      messages: [
        {
          message_id: "m1",
          role: "user",
          kind: "user",
          content: "帮我把开头剪短",
          created_at: "2026-01-01T00:00:00Z"
        },
        {
          message_id: "m2",
          role: "assistant",
          kind: "narration",
          content: "我先看看素材再动手",
          created_at: "2026-01-01T00:00:01Z"
        },
        {
          message_id: "m3",
          role: "assistant",
          kind: "reply",
          content: "开头已经剪好了",
          created_at: "2026-01-01T00:00:02Z"
        }
      ]
    });
    renderEditor(fetchMock);

    expect(await screen.findByText("帮我把开头剪短")).toBeTruthy();
    const narration = await screen.findByText("我先看看素材再动手");
    const reply = await screen.findByText("开头已经剪好了");
    expect(narration.closest("[data-message-kind]")?.getAttribute("data-message-kind")).toBe(
      "narration"
    );
    expect(reply.closest("[data-message-kind]")?.getAttribute("data-message-kind")).toBe("reply");
  });

  it("text_delta 逐步增长流式气泡", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "正在" });

    expect(await screen.findByText("正在")).toBeTruthy();

    emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "分析素材" });

    expect(await screen.findByText("正在分析素材")).toBeTruthy();
  });

  it("message_completed 用全文整体替换流式 buffer", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "半截草稿" });

    expect(await screen.findByText("半截草稿")).toBeTruthy();

    emitTurnStream(stream, {
      type: "message_completed",
      message_id: "a1",
      kind: "reply",
      content: "这是与草稿完全不同的最终全文"
    });

    expect(await screen.findByText("这是与草稿完全不同的最终全文")).toBeTruthy();
    // 全文替换而非追加：旧的流式片段不应残留
    expect(screen.queryByText("半截草稿")).toBeNull();
  });

  it("tool_step 过程条目从进行中流转到完成/失败，未映射工具显示工具名", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s1", tool: "timeline.apply_patch" });
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s2", tool: "render.preview" });
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s3", tool: "future.mystery_tool" });

    const step1 = (await screen.findByText("修改时间线")).closest(
      "[data-tool-step-id]"
    ) as HTMLElement;
    expect(step1.getAttribute("data-tool-status")).toBe("running");
    expect(screen.getByText("渲染预览")).toBeTruthy();
    // 未映射的工具名原样展示
    expect(screen.getByText("future.mystery_tool")).toBeTruthy();

    emitTurnStream(stream, {
      type: "tool_step_finished",
      step_id: "s1",
      tool: "timeline.apply_patch",
      status: "succeeded"
    });
    emitTurnStream(stream, {
      type: "tool_step_finished",
      step_id: "s2",
      tool: "render.preview",
      status: "failed"
    });

    await waitFor(() => expect(step1.getAttribute("data-tool-status")).toBe("succeeded"));
    const step2 = screen.getByText("渲染预览").closest("[data-tool-step-id]") as HTMLElement;
    expect(step2.getAttribute("data-tool-status")).toBe("failed");
  });

  it("turn-stream turn_ended 封口流式气泡并刷新历史消息", async () => {
    let sealed = false;
    const fetchMock = mockFetch({
      decision: null,
      messages: () =>
        sealed
          ? [
              {
                message_id: "a1",
                role: "assistant",
                kind: "reply",
                content: "落库后的最终回复",
                created_at: "2026-01-01T00:00:05Z"
              }
            ]
          : []
    });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "流式临时文本" });

    expect(await screen.findByText("流式临时文本")).toBeTruthy();

    sealed = true;
    emitTurnStream(stream, {
      type: "message_completed",
      message_id: "a1",
      kind: "reply",
      content: "落库后的最终回复"
    });
    emitTurnStream(stream, { type: "turn_ended", outcome: "finished", reason: null });

    await waitFor(() => expect(screen.getByText("落库后的最终回复")).toBeTruthy());
    // 封口后不再出现临时流式片段，且历史与流式 buffer 按 message_id 去重只剩一条
    expect(screen.queryByText("流式临时文本")).toBeNull();
    expect(screen.getAllByText("落库后的最终回复")).toHaveLength(1);
  });

  it("audio_mode Decision 渲染五个选项并点击提交 button answer", async () => {
    const answerRequests: Array<{ url: string; body: unknown }> = [];
    const fetchMock = mockFetch({
      decision: audioModeDecision(),
      onAnswer: (url, init) => {
        answerRequests.push({ url, body: JSON.parse(String(init?.body)) });
      }
    });
    renderEditor(fetchMock);

    await screen.findByText("原视频里有人声，这次怎么处理声音？");
    for (const label of ["保留原声", "口播粗剪", "使用上传配音", "使用 TTS", "无旁白视频"]) {
      expect(screen.getByRole("button", { name: new RegExp(label) })).toBeTruthy();
    }

    fireEvent.click(screen.getByRole("button", { name: /口播粗剪/ }));

    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.url).toBe("/api/decisions/dec_audio/answer");
    expect(answerRequests[0]?.body).toMatchObject({
      draft_id: "draft_1",
      answer: {
        option_id: "rough_cut",
        answered_via: "button",
        payload: {}
      }
    });
  });

  it("allow_free_text 提交 natural_language answer", async () => {
    const answerRequests: Array<{ body: unknown }> = [];
    const fetchMock = mockFetch({
      decision: audioModeDecision({ options: [], allow_free_text: true }),
      onAnswer: (_url, init) => {
        answerRequests.push({ body: JSON.parse(String(init?.body)) });
      }
    });
    renderEditor(fetchMock);

    fireEvent.change(await screen.findByLabelText("自由回答"), {
      target: { value: "保留一点原声，再加轻快 BGM" }
    });
    fireEvent.click(screen.getByText("提交回答"));

    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.body).toMatchObject({
      answer: {
        free_text: "保留一点原声，再加轻快 BGM",
        answered_via: "natural_language",
        payload: {}
      }
    });
  });

  it("answered Decision 置灰并显示结果", () => {
    const onAnswer = vi.fn();
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "decision",
          id: "decision:dec_audio",
          decision_id: "dec_audio",
          decision: audioModeDecision({
            status: "answered",
            answer: { option_id: "rough_cut", answered_via: "button", payload: {} }
          }),
          status: "answered",
          answer: { option_id: "rough_cut", answered_via: "button", payload: {} }
        }}
        onAnswerDecision={onAnswer}
      />
    );

    expect(screen.getByText("已回答")).toBeTruthy();
    expect(screen.getByText("结果：口播粗剪")).toBeTruthy();
    expect((screen.getByRole("button", { name: /口播粗剪/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("JobProgress SSE 更新进度条", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    act(() => {
      draftEventsSource().emit("JobProgress", jobProgressPayload(0.42));
    });

    const progress = await screen.findByRole("progressbar", { name: "素材分析 进度" });
    expect(progress.getAttribute("aria-valuenow")).toBe("42");

    act(() => {
      draftEventsSource().emit("JobProgress", jobProgressPayload(0.8));
    });

    await waitFor(() => expect(progress.getAttribute("aria-valuenow")).toBe("80"));
  });

  it("未知 kind 渲染 JSON 兜底", () => {
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "unknown",
          id: "unknown:1",
          eventName: "FutureEvent",
          raw: { event: "FutureEvent", extra: true }
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    expect(screen.getByText("未知结构化事件：FutureEvent")).toBeTruthy();
    expect(screen.getByText(/"extra": true/)).toBeTruthy();
  });

  it("events 到渲染条目的纯函数会按 job_id 合并进度并保留未知事件", () => {
    const first = reduceStructuredInteractionItems([], jobProgressPayload(0.2));
    const second = reduceStructuredInteractionItems(first, jobProgressPayload(0.75));
    expect(second).toHaveLength(1);
    expect(second[0]).toMatchObject({
      kind: "progress",
      job_id: "job_1",
      progress: 75
    });

    const unknown = itemFromEvent({
      event_id: 99,
      event: { event: "FutureEvent", value: "kept" }
    });
    expect(unknown).toMatchObject({
      kind: "unknown",
      eventName: "FutureEvent"
    });
  });

  it("时间线 seek 会联动传给 PreviewPlayer", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByTestId("mock-timeline-seek"));

    await waitFor(() => {
      const latestPreviewProps = consoleComponentMocks.previewProps.at(-1);
      expect(latestPreviewProps?.seekSec).toBe(2.5);
    });
  });
});

function renderEditor(fetchMock: FetchMock): void {
  storeAuthToken("test-token");
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource);
  vi.stubGlobal("fetch", fetchMock);

  // 组件里有 Link/useNavigate：用 RouterContextProvider 提供上下文，不做真实路由匹配。
  const router = createRouter({
    routeTree: createRootRoute(),
    history: createMemoryHistory()
  });
  render(
    <RouterContextProvider router={router}>
      <QueryClientProvider client={testQueryClient()}>
        <DraftEditorView draftId="draft_1" />
      </QueryClientProvider>
    </RouterContextProvider>
  );
}

type DraftMessageFixture = {
  message_id: string;
  role: string;
  kind: string;
  content: string;
  created_at: string;
};

function mockFetch({
  decision,
  timeline = false,
  messages = [],
  onAnswer,
  costs
}: {
  decision: Decision | null;
  timeline?: boolean;
  messages?: DraftMessageFixture[] | (() => DraftMessageFixture[]);
  onAnswer?: (url: string, init: RequestInit | undefined) => void;
  costs?: number;
}): FetchMock {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/costs")) {
      return jsonResponse({
        costs: {
          total_cost_estimate: costs ?? 0,
          provider_call_count: 0,
          by_provider: {},
          by_capability: {}
        }
      });
    }
    if (url === "/api/drafts/draft_1") {
      return jsonResponse({
        draft: {
          draft_id: "draft_1",
          name: "7月7日",
          status: "active",
          timeline_current_version: timeline ? 1 : null
        }
      });
    }
    if (url.startsWith("/api/drafts/draft_1/timeline")) {
      return jsonResponse(timelineResponseFixture());
    }
    if (url.endsWith("/decisions/current")) {
      return jsonResponse({ decision });
    }
    if (url.includes("/messages")) {
      if (init?.method === "POST") {
        return jsonResponse(
          {
            status: "queued",
            kind: "user_message",
            draft_id: "draft_1",
            message_id: "msg_1"
          },
          202
        );
      }
      return jsonResponse({
        draft_id: "draft_1",
        messages: typeof messages === "function" ? messages() : messages
      });
    }
    if (url === "/api/decisions/dec_audio/answer") {
      onAnswer?.(url, init);
      return jsonResponse({
        decision_id: "dec_audio",
        status: "answered",
        event_ids: [2],
        replays_enqueued: 0
      });
    }
    return jsonResponse({});
  });
}

function turnStreamSource(): MockEventSource {
  const source = MockEventSource.instances.find((instance) => instance.url.includes("turn-stream"));
  if (!source) {
    throw new Error("turn-stream EventSource 未创建");
  }
  return source;
}

// 编辑器还会订阅 workspace 级 /api/events（TopBar 连接态、素材面板），按 URL 定位草稿事件源。
function draftEventsSource(): MockEventSource {
  const source = MockEventSource.instances.find((instance) =>
    instance.url.includes("/api/drafts/draft_1/events")
  );
  if (!source) {
    throw new Error("draft events EventSource 未创建");
  }
  return source;
}

function emitTurnStream(source: MockEventSource, data: Record<string, unknown>): void {
  act(() => {
    source.emit("turn_stream", data);
  });
}

function timelineResponseFixture() {
  return {
    draft_id: "draft_1",
    timeline_version: 1,
    summary: "首版粗剪",
    preview_id: "prev_1",
    timeline: {
      fps: 30,
      duration_frames: 90,
      tracks: [
        {
          track_id: "visual_base",
          clips: [
            {
              timeline_clip_id: "tc_a",
              track_id: "visual_base",
              timeline_start_frame: 0,
              timeline_end_frame: 30,
              asset_id: "asset_a"
            }
          ]
        }
      ]
    }
  };
}

function audioModeDecision(overrides: Partial<Decision> = {}): Decision {
  const answer = (overrides.answer ?? null) as DecisionAnswer | null;
  return {
    allow_free_text: true,
    answer,
    blocking: true,
    consumed_at: null,
    created_by_tool_call_id: "tc_1",
    decision_id: "dec_audio",
    draft_id: "draft_1",
    options: [
      { option_id: "keep_original", label: "保留原声" },
      { option_id: "rough_cut", label: "口播粗剪" },
      { option_id: "uploaded_voiceover", label: "使用上传配音" },
      { option_id: "tts", label: "使用 TTS" },
      { option_id: "silent", label: "无旁白视频" }
    ],
    pending_tool_call: null,
    pending_tool_call_status: null,
    question: "原视频里有人声，这次怎么处理声音？",
    replayed_tool_call_id: null,
    scope_type: "draft",
    status: "pending",
    type: "audio_mode",
    ...overrides
  };
}

function jobProgressPayload(progress: number): DomainSsePayload {
  return {
    event_id: 10,
    event: {
      event: "JobProgress",
      draft_id: "draft_1",
      requested_by_draft_id: "draft_1",
      job_id: "job_1",
      kind: "素材分析",
      progress
    }
  };
}

function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false }
    }
  });
}

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}
