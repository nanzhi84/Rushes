import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterContextProvider
} from "@tanstack/react-router";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
  editMode?: string;
  dropMode?: "insert" | "overwrite";
  onClipClick?: (clipId: string) => void;
  onDeselect?: () => void;
  onZoomChange?: (pxPerSec: number) => void;
  onSplitClip?: (clipId: string, splitFrame: number) => void;
  onMoveClip?: (
    clipId: string,
    targetTrackId: string,
    targetFrame: number,
    mode: "insert" | "overwrite"
  ) => void;
  onTrimClip?: (clipId: string, edge: "start" | "end", frame: number) => void;
  onTrackStateChange?: (trackId: string, patch: Record<string, unknown>) => void;
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
    DiffusionPreviewPlayer(props: MockPreviewProps) {
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
        React.Fragment,
        null,
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-timeline-seek",
            onClick: () => props.onSeek?.(2.5)
          },
          "Mock Timeline"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-timeline-split",
            onClick: () => props.onSplitClip?.("tc_a", 15)
          },
          "Mock Split"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-timeline-move",
            onClick: () =>
              props.onMoveClip?.("overlay_a", "visual_base", 30, props.dropMode ?? "insert")
          },
          "Mock Move"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-track-lock",
            onClick: () => props.onTrackStateChange?.("voiceover", { locked: true })
          },
          "Mock Track Lock"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-select-video",
            onClick: () => props.onClipClick?.("tc_a")
          },
          "Mock Select Video"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-select-audio",
            onClick: () => props.onClipClick?.("audio_a")
          },
          "Mock Select Audio"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-select-subtitle",
            onClick: () => props.onClipClick?.("subtitle_a")
          },
          "Mock Select Subtitle"
        ),
        React.createElement(
          "button",
          {
            type: "button",
            "data-testid": "mock-timeline-deselect",
            onClick: () => props.onDeselect?.()
          },
          "Mock Deselect"
        )
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

  it("时间线固定在右侧工作区底部，不跨越左侧 AI 面板", () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const timeline = screen.getByLabelText("时间线");
    const chat = screen.getByLabelText("剪辑对话");
    const workspace = screen.getByTestId("editor-workspace");
    expect(workspace.contains(timeline)).toBe(true);
    expect(workspace.contains(screen.getByTestId("materials-panel"))).toBe(true);
    expect(workspace.contains(chat)).toBe(false);
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

  it("清空对话会二次确认并调用专用端点，不操作素材和时间线", async () => {
    const fetchMock = mockFetch({ decision: null });
    const confirmMock = vi.fn(() => true);
    vi.stubGlobal("confirm", confirmMock);
    renderEditor(fetchMock);

    fireEvent.click(screen.getByRole("button", { name: "清空对话上下文" }));

    expect(confirmMock).toHaveBeenCalledWith(expect.stringContaining("素材、素材理解、时间线和预览都会保留"));
    await waitFor(() => {
      expect(
        vi.mocked(fetchMock).mock.calls.some(
          ([input, init]) =>
            String(input) === "/api/drafts/draft_1/conversation/clear" && init?.method === "POST"
        )
      ).toBe(true);
    });
    expect(
      vi.mocked(fetchMock).mock.calls.some(([input]) => String(input).includes("/materials/"))
    ).toBe(false);
  });

  it("回退面板展示检查点 diff，并从用户消息或工具批次执行恢复", async () => {
    const restored: unknown[] = [];
    const checkpointBase = {
      anchor_event_id: 1,
      clip_count: 1,
      clip_count_delta: 1,
      created_at: "2026-07-16T02:00:00Z",
      duration_frames: 90,
      duration_frames_delta: 90,
      timeline_version: 1,
      track_count: 1,
      track_count_delta: 1,
      trigger_kind: "timeline_write"
    };
    const fetchMock = mockFetch({
      decision: null,
      timeline: true,
      rewoundMessageCount: 2,
      messages: [
        {
          message_id: "user-anchor",
          role: "user",
          kind: "user",
          content: "制作第一版",
          created_at: "2026-07-16T01:59:00Z"
        },
        {
          message_id: "tool-anchor",
          role: "system",
          kind: "tool",
          content: JSON.stringify({
            step_id: "tool-anchor",
            tool: "timeline.apply_patches",
            status: "succeeded",
            args_summary: "{}",
            observation: "ok"
          }),
          created_at: "2026-07-16T02:00:00Z"
        }
      ],
      rewindCheckpoints: [
        {
          ...checkpointBase,
          checkpoint_id: "rewind-tool",
          anchor_message_id: "tool-anchor",
          anchor_turn_id: "user-anchor",
          patch_id: "patch-tool",
          summary: "工具批次 timeline.apply_patches"
        },
        {
          ...checkpointBase,
          checkpoint_id: "rewind-user",
          anchor_message_id: "user-anchor",
          anchor_turn_id: "user-anchor",
          patch_id: null,
          summary: "制作第一版",
          trigger_kind: "user_message"
        }
      ],
      onRewind: (body) => restored.push(body)
    });
    renderEditor(fetchMock);

    expect(await screen.findByText("已回退并折叠 2 条历史消息")).toBeTruthy();
    act(() => {
      emitTurnStream(turnStreamSource(), { type: "turn_started", turn_id: "turn-to-rewind" });
      emitTurnStream(turnStreamSource(), {
        type: "text_delta",
        message_id: "late-stream-message",
        delta: "即将撤销的流式回复"
      });
    });
    expect(await screen.findByText("即将撤销的流式回复")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "打开回退检查点" }));
    expect(await screen.findByRole("region", { name: "回退检查点" })).toBeTruthy();
    expect(screen.getAllByText(/较前一检查点 \+1 片段/)).toHaveLength(2);
    expect(await screen.findByRole("button", { name: "回到此消息" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "回到工具批次 批量修改时间线" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "回到此消息" }));
    fireEvent.click(screen.getByRole("button", { name: "时间线和对话" }));
    await waitFor(() => {
      expect(restored).toContainEqual(expect.objectContaining({
        checkpoint_id: "rewind-user",
        idempotency_key: expect.any(String),
        mode: "both"
      }));
    });
    await waitFor(() => expect(screen.queryByText("即将撤销的流式回复")).toBeNull());
  });

  it("顶栏成本小计渲染估算金额，且编辑器隐藏设置按钮", async () => {
    const fetchMock = mockFetch({ decision: null, costs: 1.2345 });
    renderEditor(fetchMock);

    expect(await screen.findByText("¥1.2345")).toBeTruthy();
    expect(screen.queryByText("设置")).toBeNull();
  });

  it("当前回合运行时仍可输入并把后续消息按顺序排队", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "剪掉开头 3 秒" } });
    fireEvent.click(screen.getByRole("button", { name: "发送消息" }));

    await waitFor(() => expect(screen.getByText("新消息将按发送顺序排队")).toBeTruthy());
    expect(input.disabled).toBe(false);
    expect(screen.getByText("剪掉开头 3 秒")).toBeTruthy();
    expect(screen.getByTestId("turn-activity-indicator")).toBeTruthy();
    expect(screen.getByText("正在读取上下文")).toBeTruthy();
    expect(draftEventsSource().url).toContain("token=test-token");

    fireEvent.change(input, { target: { value: "然后把结尾淡出" } });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(screen.getByText("然后把结尾淡出")).toBeTruthy());
    expect(
      vi.mocked(fetchMock).mock.calls.filter(
        ([request, init]) => String(request).endsWith("/messages") && init?.method === "POST"
      )
    ).toHaveLength(2);
    expect(input.disabled).toBe(false);

    emitTurnStream(turnStreamSource(), {
      type: "turn_ended",
      outcome: "finished",
      reason: null
    });

    await waitFor(() => expect(input.disabled).toBe(false));
    expect(screen.queryByTestId("turn-activity-indicator")).toBeNull();
    expect(screen.queryByText("新消息将按发送顺序排队")).toBeNull();
  });

  it("重连重放到活跃回合时保持输入可用并显示排队反馈", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    emitTurnStream(turnStreamSource(), { type: "turn_started", turn_id: "turn_replayed" });

    await waitFor(() => expect(screen.getByText("新消息将按发送顺序排队")).toBeTruthy());
    expect(input.disabled).toBe(false);
    expect(screen.getByTestId("turn-activity-indicator")).toBeTruthy();

    emitTurnStream(turnStreamSource(), {
      type: "turn_ended",
      outcome: "finished",
      reason: null
    });
    await waitFor(() => expect(input.disabled).toBe(false));
  });

  it("输入框使用 Enter 发送、Shift+Enter 换行，并在运行时提供停止按钮", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "先分析节奏" } });
    fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
    expect(input.disabled).toBe(false);

    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(screen.getByText("新消息将按发送顺序排队")).toBeTruthy());
    expect(input.disabled).toBe(false);
    expect(screen.getByLabelText("停止当前任务")).toBeTruthy();
    expect(screen.getByRole("button", { name: "发送消息" })).toBeTruthy();
    expect(screen.getByText("先分析节奏")).toBeTruthy();
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
    expect(new URL(stream.url).searchParams.get("turn_stream_client_id")).toBeTruthy();
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

  it("subagent_progress 实时挂在进行中的理解工具行下方，同素材新 note 覆盖旧的", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s1", tool: "understand.materials" });
    emitTurnStream(stream, {
      type: "subagent_progress",
      tool: "understand.materials",
      completed: 3,
      total: 20,
      note: "理解中 3/20"
    });
    emitTurnStream(stream, {
      type: "subagent_progress",
      asset_id: "asset_01a2",
      note: "正在查看 IMG_2031.mp4 02:10 画面"
    });
    emitTurnStream(stream, {
      type: "subagent_progress",
      asset_id: "asset_09f3",
      note: "转写音频中"
    });

    // 两条素材进度都渲染出来；带文件名的 note 只显示 note（不叠 asset_id）。
    expect(await screen.findByText("正在查看 IMG_2031.mp4 02:10 画面")).toBeTruthy();
    const progressList = screen.getByLabelText("子代理进度");
    expect(within(progressList).getByText("转写音频中")).toBeTruthy();
    // 无文件名的通用文案用 asset_id 前缀区分并发素材。
    expect(within(progressList).getByText("asset_09f3")).toBeTruthy();

    // 进度行确实挂在 understand 工具行的同一容器里（不是独立漂浮在消息流末尾）。
    const understandRow = document.querySelector('[data-tool-step-id="s1"]') as HTMLElement;
    expect(within(understandRow).getByText("理解素材")).toBeTruthy();
    expect(understandRow.parentElement?.contains(progressList)).toBe(true);
    expect(screen.queryByLabelText("素材理解中 3/20")).toBeNull();
    expect(screen.queryByRole("button", { name: "取消素材理解" })).toBeNull();

    // 同素材新 note 覆盖旧的，仍只有一条该素材的进度行。
    emitTurnStream(stream, {
      type: "subagent_progress",
      asset_id: "asset_09f3",
      note: "产出摘要中"
    });
    await waitFor(() => expect(screen.getByText("产出摘要中")).toBeTruthy());
    expect(screen.queryByText("转写音频中")).toBeNull();
    expect(screen.getAllByText("产出摘要中")).toHaveLength(1);
  });

  it("理解工具完成后其子代理进度行消失，不残留到后续工具行", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    const stream = turnStreamSource();
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s1", tool: "understand.materials" });
    emitTurnStream(stream, { type: "subagent_progress", asset_id: "asset_01a2", note: "转写音频中" });

    expect(await screen.findByText("转写音频中")).toBeTruthy();

    emitTurnStream(stream, {
      type: "tool_step_finished",
      step_id: "s1",
      tool: "understand.materials",
      status: "succeeded"
    });
    // 下一个工具进行中，但上一批的进度不应串过来。
    emitTurnStream(stream, { type: "tool_step_started", step_id: "s2", tool: "timeline.compose_initial" });

    await waitFor(() => expect(screen.queryByText("转写音频中")).toBeNull());
    expect(screen.queryByLabelText("子代理进度")).toBeNull();
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

  it.each(["stream_snapshot_truncated", "stream_gap", "error"] as const)(
    "turn-stream %s 立即刷新历史消息",
    async (signal) => {
      let refreshed = false;
      const fetchMock = mockFetch({
        decision: null,
        messages: () =>
          refreshed
            ? [
                {
                  message_id: "gap_recovery",
                  role: "assistant",
                  kind: "reply",
                  content: "缺口后的落库消息",
                  created_at: "2026-01-01T00:00:05Z"
                }
              ]
            : []
      });
      renderEditor(fetchMock);
      await waitFor(() =>
        expect(
          vi.mocked(fetchMock).mock.calls.filter(
            ([input, init]) => String(input).includes("/messages") && init?.method !== "POST"
          ).length
        ).toBeGreaterThan(0)
      );

      refreshed = true;
      const stream = turnStreamSource();
      if (signal === "error") {
        act(() => stream.emit("error", {}));
      } else {
        emitTurnStream(stream, { type: signal });
      }

      expect(await screen.findByText("缺口后的落库消息")).toBeTruthy();
    }
  );

  it("结构化 Decision 渲染五个选项并点击提交 button answer", async () => {
    const answerRequests: Array<{ url: string; body: unknown }> = [];
    const fetchMock = mockFetch({
      decision: editingStyleDecision(),
      onAnswer: (url, init) => {
        answerRequests.push({ url, body: JSON.parse(String(init?.body)) });
      }
    });
    renderEditor(fetchMock);

    await screen.findByText("这次希望采用哪种剪辑风格？");
    for (const label of ["快节奏", "舒缓", "叙事", "活力", "极简"]) {
      expect(screen.getByRole("button", { name: new RegExp(label) })).toBeTruthy();
    }

    fireEvent.click(screen.getByRole("button", { name: /叙事/ }));

    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.url).toBe("/api/decisions/dec_style/answer");
    expect(answerRequests[0]?.body).toMatchObject({
      draft_id: "draft_1",
      answer: {
        option_id: "story",
        answered_via: "button",
        payload: {}
      }
    });
  });

  it.each([
    ["critical", "这次内容主线存在冲突，请选择核心方向。"],
    ["approve_content_plan", "确认内容计划？"],
    ["approve_speech_cut", "确认口播首剪 EDL？"],
    ["approve_rough_cut", "确认卡点首剪 EDL？"]
  ] as const)("%s 决策类型渲染为可回答卡片", async (type, question) => {
    const answerRequests: Array<{ body: unknown }> = [];
    renderEditor(
      mockFetch({
        decision: editingStyleDecision({ type, question }),
        onAnswer: (_url, init) => {
          answerRequests.push({ body: JSON.parse(String(init?.body)) });
        }
      })
    );

    expect(await screen.findByText(question)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /快节奏/ }));
    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.body).toMatchObject({
      answer: { option_id: "fast", answered_via: "button" }
    });
  });

  it("当前确认项与同一条 SSE 事件只渲染一个问答面板", async () => {
    const decision = editingStyleDecision();
    const fetchMock = mockFetch({ decision });
    renderEditor(fetchMock);

    await screen.findByText("这次希望采用哪种剪辑风格？");
    expect(screen.getAllByTestId("decision-group")).toHaveLength(1);
    expect(screen.getAllByTestId("decision-question")).toHaveLength(1);

    act(() => {
      draftEventsSource().emit("DecisionCreated", {
        event_id: 12,
        event: {
          event: "DecisionCreated",
          draft_id: "draft_1",
          payload: {
            decision_id: decision.decision_id,
            scope_type: decision.scope_type,
            type: decision.type,
            question: decision.question,
            options: decision.options,
            allow_free_text: decision.allow_free_text,
            blocking: decision.blocking
          }
        }
      });
    });

    await waitFor(() => expect(screen.getAllByTestId("decision-group")).toHaveLength(1));
    expect(screen.getAllByTestId("decision-question")).toHaveLength(1);
    expect(screen.getAllByText("这次希望采用哪种剪辑风格？")).toHaveLength(1);
  });

  it("allow_free_text 提交 natural_language answer", async () => {
    const answerRequests: Array<{ body: unknown }> = [];
    const fetchMock = mockFetch({
      decision: editingStyleDecision({ options: [], allow_free_text: true }),
      onAnswer: (_url, init) => {
        answerRequests.push({ body: JSON.parse(String(init?.body)) });
      }
    });
    renderEditor(fetchMock);

    fireEvent.change(await screen.findByLabelText("自由回答"), {
      target: { value: "节奏明快，镜头切换干净" }
    });
    fireEvent.click(screen.getByRole("button", { name: "提交" }));

    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.body).toMatchObject({
      answer: {
        free_text: "节奏明快，镜头切换干净",
        answered_via: "natural_language",
        payload: {}
      }
    });
  });

  it("answered Decision 紧凑显示原问题和实际答案", () => {
    const onAnswer = vi.fn();
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "decision",
          id: "decision:dec_style",
          decision_id: "dec_style",
          decision: editingStyleDecision({
            status: "answered",
            answer: { option_id: "story", answered_via: "button", payload: {} }
          }),
          status: "answered",
          answer: { option_id: "story", answered_via: "button", payload: {} }
        }}
        onAnswerDecision={onAnswer}
      />
    );

    expect(screen.getByText("已回答 1 个问题")).toBeTruthy();
    expect(screen.getByText("这次希望采用哪种剪辑风格？")).toBeTruthy();
    expect(screen.queryByText("你的回答")).toBeNull();
    expect(screen.getByText("叙事")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /叙事/ })).toBeNull();
  });

  it("只有事件占位的 answered Decision 正确收尾，不再伪装成加载中", () => {
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "decision",
          id: "decision:dec_answered",
          decision_id: "dec_answered",
          decision: null,
          status: "answered",
          answer: { option_id: "story", answered_via: "button", payload: {} }
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    expect(screen.getByText("回答已记录")).toBeTruthy();
    expect(screen.getByText("已回答 1 个问题")).toBeTruthy();
    expect(screen.queryByText("你的回答")).toBeNull();
    expect(screen.getByText("story")).toBeTruthy();
    expect(screen.queryByText(/正在读取详情/)).toBeNull();
  });

  it("answered Decision 显示自然语言回答原文", () => {
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "decision",
          id: "decision:dec_free_text",
          decision_id: "dec_free_text",
          decision: editingStyleDecision({
            decision_id: "dec_free_text",
            status: "answered",
            answer: {
              free_text: "先只剪掉气口，之后直接配 B-roll",
              answered_via: "natural_language",
              payload: {}
            }
          }),
          status: "answered",
          answer: {
            free_text: "先只剪掉气口，之后直接配 B-roll",
            answered_via: "natural_language",
            payload: {}
          }
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    expect(screen.getByText("先只剪掉气口，之后直接配 B-roll")).toBeTruthy();
  });

  it("Decision 事件回放保留问题与选项，并把按钮答案还原为可读文本", () => {
    let items = reduceStructuredInteractionItems([], {
      event_id: 1,
      event: {
        event: "DecisionCreated",
        draft_id: "draft_1",
        payload: {
          decision_id: "dec_aroll",
          scope_type: "draft",
          type: "generic",
          question: "请确认哪个视频是需要剪辑的口播主素材？",
          options: [
            { option_id: "aroll", label: "Aroll-气口剪辑完成素材.mov" },
            { option_id: "tim", label: "Tim-Macbook Neo Talking节选.mp4" }
          ],
          allow_free_text: true,
          blocking: true
        }
      }
    });
    items = reduceStructuredInteractionItems(items, {
      event_id: 2,
      event: {
        event: "DecisionAnswered",
        draft_id: "draft_1",
        payload: {
          decision_id: "dec_aroll",
          answer: { option_id: "tim", answered_via: "button", payload: {} }
        }
      }
    });

    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({
      kind: "decision",
      status: "answered",
      decision: {
        question: "请确认哪个视频是需要剪辑的口播主素材？",
        status: "answered"
      },
      answer: { option_id: "tim" }
    });
    render(
      <StructuredInteractionRenderer
        item={items[0]!}
        onAnswerDecision={vi.fn()}
      />
    );
    expect(screen.getByText("请确认哪个视频是需要剪辑的口播主素材？")).toBeTruthy();
    expect(screen.getByText("Tim-Macbook Neo Talking节选.mp4")).toBeTruthy();
  });

  it("待补齐的 pending Decision 显示旋转加载与耗时", () => {
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "decision",
          id: "decision:dec_pending",
          decision_id: "dec_pending",
          decision: null,
          status: "pending",
          answer: null
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    expect(screen.getByText("正在同步确认项")).toBeTruthy();
    expect(screen.getByText("正在读取可选项")).toBeTruthy();
    expect(screen.getByText(/00:00/)).toBeTruthy();
  });

  it("JobProgress SSE 显示素材明细并可取消当前 job", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderEditor(fetchMock);

    act(() => {
      draftEventsSource().emit("JobProgress", jobProgressPayload(0.42));
    });

    const progress = await screen.findByRole("progressbar", { name: "理解素材 进度" });
    expect(progress.getAttribute("aria-valuenow")).toBe("42");
    expect(screen.getByText("理解素材 2/5：采访.mp4 正在调用 VLM")).toBeTruthy();

    act(() => {
      draftEventsSource().emit("JobProgress", jobProgressPayload(0.8));
    });
    await waitFor(() => expect(progress.getAttribute("aria-valuenow")).toBe("80"));

    fireEvent.click(screen.getByRole("button", { name: "取消理解素材" }));
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/jobs/job_1/cancel",
        expect.objectContaining({ method: "POST" })
      )
    );
    await screen.findByText("已取消");
    expect(screen.queryByRole("button", { name: "取消理解素材" })).toBeNull();
  });

  it.each([
    ["conflict", "任务状态已变化，无法取消；已刷新当前状态。"],
    ["network", "取消任务失败：网络不可用"]
  ] as const)("job 取消失败会显示错误：%s", async (cancelFailure, message) => {
    const fetchMock = mockFetch({ decision: null, cancelFailure });
    renderEditor(fetchMock);
    act(() => {
      draftEventsSource().emit("JobProgress", jobProgressPayload(0.42));
    });
    fireEvent.click(await screen.findByRole("button", { name: "取消理解素材" }));
    expect((await screen.findByRole("alert")).textContent).toContain(message);
    expect(screen.getByRole("button", { name: "取消理解素材" })).toBeTruthy();
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

  it("预览与时间线事件渲染为单行状态，不再使用大块卡片", () => {
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "preview",
          id: "preview:latest",
          title: "预览已生成",
          description: "可在右侧查看预览。",
          occurrences: 8
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    const row = screen.getByTestId("preview-event-row");
    expect(row.getAttribute("data-layout")).toBe("inline");
    expect(row.tagName).toBe("DIV");
    expect(screen.getByText("×8")).toBeTruthy();
  });

  it("重复预览会合并，时间线保存与校验事件不进入对话区", () => {
    let items = reduceStructuredInteractionItems([], activityEventPayload(1, "PreviewRendered"));
    items = reduceStructuredInteractionItems(items, activityEventPayload(2, "PreviewRendered"));
    items = reduceStructuredInteractionItems(items, activityEventPayload(3, "TimelineVersionCreated"));
    items = reduceStructuredInteractionItems(items, activityEventPayload(4, "TimelineValidated"));

    expect(items).toHaveLength(1);
    expect(items.find((item) => item.kind === "preview")).toMatchObject({
      id: "preview:latest",
      title: "预览已生成",
      description: "可在右侧查看预览。",
      occurrences: 2
    });
    expect(items.find((item) => item.kind === "timeline")).toBeUndefined();
  });

  it("人工与 Agent 的时间线保存事件都保持静默", () => {
    const manual = activityEventPayload(1, "TimelineVersionCreated");
    manual.event.actor = "user";
    const agent = activityEventPayload(2, "TimelineVersionCreated");
    agent.event.actor = "agent";

    expect(reduceStructuredInteractionItems([], manual)).toEqual([]);
    expect(reduceStructuredInteractionItems([], agent)).toEqual([]);
  });

  it("领域事件历史突发只合并刷新一次最新时间线", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);
    await screen.findByTestId("mock-preview");
    const timelineRequestCount = (): number =>
      vi
        .mocked(fetchMock)
        .mock.calls.filter(
          ([input, init]) =>
            String(input) === "/api/drafts/draft_1/timeline" && init?.method !== "POST"
        ).length;
    const baseline = timelineRequestCount();
    const source = draftEventsSource();

    act(() => {
      for (let index = 0; index < 50; index += 1) {
        source.emit("TimelineValidated", {
          event_id: 1000 + index,
          event: {
            event: "TimelineValidated",
            actor: "agent",
            draft_id: "draft_1",
            payload: {}
          }
        });
      }
    });

    await waitFor(() => expect(timelineRequestCount()).toBe(baseline + 1));
  });

  it("ConversationContextCleared 会清掉清空点之前的结构化交互", () => {
    const before = reduceStructuredInteractionItems(
      [],
      activityEventPayload(1, "PreviewRendered")
    );
    const after = reduceStructuredInteractionItems(before, {
      event_id: 2,
      event: { event: "ConversationContextCleared", draft_id: "draft_1" }
    });
    expect(before).toHaveLength(1);
    expect(after).toEqual([]);
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

  it("AssetUnlinked 属于已知素材生命周期事件且不进入对话区", () => {
    const payload = {
      event_id: 100,
      event: {
        event: "AssetUnlinked",
        draft_id: "draft_1",
        payload: { asset_id: "asset_1" }
      }
    };

    expect(itemFromEvent(payload)).toBeNull();
    expect(reduceStructuredInteractionItems([], payload)).toEqual([]);
  });

  it("素材 ingest job 事件不在对话区产进度行", () => {
    let items = reduceStructuredInteractionItems([], jobEventPayload("JobEnqueued", "ingest"));
    items = reduceStructuredInteractionItems(items, jobEventPayload("JobSucceeded", "ingest"));
    items = reduceStructuredInteractionItems(items, jobEventPayload("JobFailed", "ingest"));
    expect(items).toHaveLength(0);
  });

  it("白名单 job（understand）产进度行且文案按 kind 给中文名", () => {
    const items = reduceStructuredInteractionItems([], jobEventPayload("JobEnqueued", "understand"));
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: "progress", job_id: "job_1", job_kind: "理解素材" });
  });

  it("同一 job 的 queued→running→succeeded 合并进一行并流转到完成态", () => {
    // 三种事件的 job 标识都取顶层 event.job_id（这里都是 job_1），必须合并进同一行。
    let items = reduceStructuredInteractionItems([], jobEventPayload("JobEnqueued", "understand"));
    expect(items).toMatchObject([{ kind: "progress", job_id: "job_1", status: "queued" }]);

    items = reduceStructuredInteractionItems(items, jobProgressPayload(0.5));
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ job_id: "job_1", status: "running", progress: 50 });

    items = reduceStructuredInteractionItems(items, jobEventPayload("JobSucceeded", "understand"));
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ job_id: "job_1", status: "succeeded" });
  });

  it("同类并发渲染 job 独立显示并取消各自的 job_id", () => {
    let items = reduceStructuredInteractionItems(
      [],
      jobEventPayload("JobEnqueued", "render_preview", "job_preview_1")
    );
    items = reduceStructuredInteractionItems(
      items,
      jobEventPayload("JobEnqueued", "render_preview", "job_preview_2")
    );

    expect(items).toMatchObject([
      { kind: "progress", job_id: "job_preview_1", job_kind: "渲染预览", status: "queued" },
      { kind: "progress", job_id: "job_preview_2", job_kind: "渲染预览", status: "queued" }
    ]);

    const onCancelJob = vi.fn();
    render(
      <>
        {items.map((item) => (
          <StructuredInteractionRenderer
            key={item.id}
            item={item}
            onAnswerDecision={vi.fn()}
            onCancelJob={onCancelJob}
          />
        ))}
      </>
    );
    const cancelButtons = screen.getAllByRole("button", { name: "取消渲染预览" });
    fireEvent.click(cancelButtons[0]);
    fireEvent.click(cancelButtons[1]);
    expect(onCancelJob.mock.calls).toEqual([["job_preview_1"], ["job_preview_2"]]);
  });

  it("进度行终态 succeeded 显示已完成而非停在处理中", () => {
    // 即使某个 job 没发中间 JobProgress，也必须靠终态显式收尾。
    render(
      <StructuredInteractionRenderer
        item={{
          kind: "progress",
          id: "progress:job_1",
          job_id: "job_1",
          job_kind: "理解素材",
          progress: null,
          status: "succeeded"
        }}
        onAnswerDecision={vi.fn()}
      />
    );

    expect(screen.getByText("已完成")).toBeTruthy();
    expect(screen.queryByText("处理中")).toBeNull();
    expect(
      screen.getByRole("progressbar", { name: "理解素材 进度" }).getAttribute("aria-valuenow")
    ).toBe("100");
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

  it("时间线工具切换到刀片模式，并把分割操作提交到 patch API", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByRole("button", { name: "刀片 (B)" }));
    await waitFor(() => {
      expect(consoleComponentMocks.timelineProps.at(-1)?.editMode).toBe("blade");
    });

    fireEvent.click(screen.getByTestId("mock-timeline-split"));
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "split_clip", timeline_clip_id: "tc_a", split_frame: 15
      });
    });
  });

  it("时间线工具栏不再提供版本切换、撤销和重做", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    await screen.findByTestId("mock-timeline-move");
    expect(screen.queryByRole("button", { name: "撤销 (⌘Z)" })).toBeNull();
    expect(screen.queryByRole("button", { name: "重做 (⇧⌘Z)" })).toBeNull();
    expect(screen.queryByLabelText("时间线版本")).toBeNull();
  });

  it("在播放头位置新增可编辑字幕片段", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);
    await screen.findByTestId("mock-timeline-move");

    fireEvent.click(screen.getByRole("button", { name: "添加字幕" }));

    await waitFor(() => {
      const operation = manualPatchOperations(fetchMock).find(
        (candidate) => candidate.kind === "insert_subtitle"
      );
      expect(operation).toMatchObject({
        kind: "insert_subtitle",
        start_frame: 0,
        end_frame: 60,
        text: "在这里输入字幕"
      });
      expect(operation?.timeline_clip_id).toMatch(/^subtitle_manual_/);
    });
  });

  it("覆盖模式的跨轨拖放提交 move_clip，轨道锁定提交 set_track_state", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    const move = await screen.findByTestId("mock-timeline-move");
    fireEvent.click(screen.getByRole("button", { name: "覆盖" }));
    fireEvent.click(move);
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
          kind: "move_clip",
          timeline_clip_id: "overlay_a",
          target_track_id: "visual_base",
          target_frame: 30,
          mode: "overwrite"
      });
    });

    fireEvent.click(screen.getByTestId("mock-track-lock"));
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "set_track_state", track_id: "voiceover", locked: true
      });
    });
  });

  it("选中联动片段可取消音画联动，选中字幕可直接保存文字", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByTestId("mock-select-video"));
    fireEvent.click(screen.getAllByRole("button", { name: "取消联动" })[0]!);
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "set_clip_linked", timeline_clip_id: "tc_a", linked: false
      });
    });

    fireEvent.click(screen.getByTestId("mock-select-subtitle"));
    const subtitleInput = await screen.findByRole("textbox", { name: "编辑字幕" });
    fireEvent.change(subtitleInput, { target: { value: "改好的字幕" } });
    fireEvent.click(screen.getByRole("button", { name: "保存字幕" }));
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "edit_subtitle_text", timeline_clip_id: "subtitle_a", text: "改好的字幕"
      });
    });
  });

  it("选中片段后点击时间线空白处会收起详情栏", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByTestId("mock-select-video"));
    expect(await screen.findByText("已选：")).toBeTruthy();

    fireEvent.click(screen.getByTestId("mock-timeline-deselect"));

    await waitFor(() => expect(screen.queryByText("已选：")).toBeNull());
  });

  it("时间线缩放滑杆连续更新每秒像素密度", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);
    await screen.findByTestId("mock-timeline-move");

    fireEvent.change(screen.getByRole("slider", { name: "时间线缩放" }), {
      target: { value: "137" }
    });

    await waitFor(() => expect(consoleComponentMocks.timelineProps.at(-1)?.pxPerSec).toBe(137));
  });

  it("选中音频片段可分别调整片段与轨道音量", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByTestId("mock-select-audio"));
    const clipGain = await screen.findByRole("slider", { name: "片段音量" });
    fireEvent.change(clipGain, { target: { value: "-6" } });
    fireEvent.pointerUp(clipGain);
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "adjust_gain", timeline_clip_id: "audio_a", gain_db: -6
      });
    });

    const trackGain = screen.getByRole("slider", { name: "所选轨道音量" });
    fireEvent.change(trackGain, { target: { value: "-10" } });
    fireEvent.pointerUp(trackGain);
    await waitFor(() => {
      expect(manualPatchOperations(fetchMock)).toContainEqual({
        kind: "set_track_state", track_id: "original_audio", gain_db: -10
      });
    });
  });

  it("单击素材瓦片在预览区挂载试看，再击取消回占位", async () => {
    const fetchMock = mockFetch({ decision: null, materials: [videoAssetFixture()] });
    renderEditor(fetchMock);

    fireEvent.click(await screen.findByTitle("clip.mp4"));

    // 预览区挂载素材试看（原片优先）+ 顶部工具条
    expect(await screen.findByLabelText("clip.mp4 视频试看")).toBeTruthy();
    expect(screen.getByText("试看 · clip.mp4")).toBeTruthy();

    // 再次单击同一瓦片取消试看，回到「暂无时间线」占位
    fireEvent.click(await screen.findByTitle("clip.mp4"));

    await waitFor(() => expect(screen.queryByLabelText("clip.mp4 视频试看")).toBeNull());
    // 预览区回到成片占位（时间线区也有同名占位，故按预览区作用域断言）
    expect(within(screen.getByLabelText("预览区")).getByText(/暂无时间线/)).toBeTruthy();
  });

  it("流式 text_delta 高频更新期间不重渲染时间线子树（高频态已下沉到 ConsolePanel）", async () => {
    const fetchMock = mockFetch({ decision: null, timeline: true });
    renderEditor(fetchMock);

    // 等时间线首次挂载
    await screen.findByTestId("mock-timeline-seek");
    const stream = turnStreamSource();

    // 首个 delta 会把 turnActive 翻成 true，经忙碌态回调让工作区重渲染一次（导出按钮禁用态）。
    // 这是预期内的一次性切换，记录其后的渲染次数作为基线。
    emitTurnStream(stream, { type: "turn_started", turn_id: "turn_1" });
    emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "开" });
    await screen.findByText("开");
    const baseline = consoleComponentMocks.timelineProps.length;

    // 大量后续 delta：turnActive 保持 true、忙碌态不变，高频流式只重渲染左侧对话栏，
    // 时间线子树（mock TimelineViewer 的渲染次数）完全不动。
    for (let i = 0; i < 40; i += 1) {
      emitTurnStream(stream, { type: "text_delta", message_id: "a1", kind: "assistant", delta: "字" });
    }
    await screen.findByText(`开${"字".repeat(40)}`);

    expect(consoleComponentMocks.timelineProps.length).toBe(baseline);
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
  materials = [],
  onAnswer,
  costs,
  cancelFailure,
  rewindCheckpoints = [],
  rewoundMessageCount = 0,
  onRewind
}: {
  decision: Decision | null;
  timeline?: boolean;
  messages?: DraftMessageFixture[] | (() => DraftMessageFixture[]);
  materials?: Array<Record<string, unknown>>;
  onAnswer?: (url: string, init: RequestInit | undefined) => void;
  costs?: number;
  cancelFailure?: "conflict" | "network";
  rewindCheckpoints?: Array<Record<string, unknown>>;
  rewoundMessageCount?: number;
  onRewind?: (body: unknown) => void;
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
    if (url.endsWith("/rewind/checkpoints")) {
      return jsonResponse({ draft_id: "draft_1", checkpoints: rewindCheckpoints });
    }
    if (url.endsWith("/rewind") && init?.method === "POST") {
      onRewind?.(init.body ? JSON.parse(String(init.body)) : null);
      return jsonResponse({
        draft_id: "draft_1",
        checkpoint_id: "rewind_1",
        mode: "both",
        status: "restored",
        timeline_version: 2,
        rewound_message_count: 2,
        cancelled_jobs: 0,
        cancelled_decisions: 0,
        event_ids: [9]
      });
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
        messages: typeof messages === "function" ? messages() : messages,
        rewound_message_count: rewoundMessageCount
      });
    }
    if (url === "/api/decisions/dec_style/answer") {
      onAnswer?.(url, init);
      return jsonResponse({
        decision_id: "dec_style",
        status: "answered",
        event_ids: [2],
        replays_enqueued: 0
      });
    }
    if (url.endsWith("/materials")) {
      return jsonResponse({ draft_id: "draft_1", assets: materials, invalidated_asset_ids: [] });
    }
    if (url === "/api/jobs/job_1/cancel") {
      if (cancelFailure === "network") {
        throw new Error("网络不可用");
      }
      if (cancelFailure === "conflict") {
        return jsonResponse({ detail: { reason: "job_not_cancellable" } }, 409);
      }
      return jsonResponse({ job_id: "job_1", status: "cancelled", event_ids: [1] });
    }
    return jsonResponse({});
  });
}

function turnStreamSource(): MockEventSource {
  return draftEventsSource();
}

// 领域事件与 turn-stream 复用同一条草稿 SSE，避免多标签页耗尽 HTTP/1.1 连接。
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

function manualPatchOperations(fetchMock: FetchMock): Record<string, unknown>[] {
  return vi
    .mocked(fetchMock)
    .mock.calls.filter(([input]) => String(input) === "/api/drafts/draft_1/timeline/patch")
    .flatMap(([, init]) => {
      const payload = JSON.parse(String(init?.body)) as {
        op?: Record<string, unknown> & { ops?: Record<string, unknown>[] };
      };
      if (payload.op?.kind === "batch") {
        return payload.op.ops ?? [];
      }
      return payload.op ? [payload.op] : [];
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
              timeline_end_frame: 90,
              source_start_frame: 0,
              source_end_frame: 90,
              asset_id: "asset_a",
              asset_kind: "video",
              linked: true,
              parent_block_id: "block_a"
            }
          ]
        },
        {
          track_id: "visual_overlay",
          clips: [
            {
              timeline_clip_id: "overlay_a",
              track_id: "visual_overlay",
              timeline_start_frame: 20,
              timeline_end_frame: 40,
              source_start_frame: 0,
              source_end_frame: 20,
              asset_id: "asset_overlay",
              asset_kind: "video",
              linked: false
            }
          ]
        },
        {
          track_id: "original_audio",
          track_type: "audio",
          gain_db: -2,
          clips: [
            {
              timeline_clip_id: "audio_a",
              track_id: "original_audio",
              timeline_start_frame: 0,
              timeline_end_frame: 90,
              source_start_frame: 0,
              source_end_frame: 90,
              asset_id: "asset_a",
              asset_kind: "video",
              gain_db: -1,
              linked: true,
              parent_block_id: "block_a"
            }
          ]
        },
        {
          track_id: "voiceover",
          track_type: "audio",
          clips: []
        },
        {
          track_id: "subtitles",
          track_type: "text",
          clips: [
            {
              timeline_clip_id: "subtitle_a",
              track_id: "subtitles",
              timeline_start_frame: 0,
              timeline_end_frame: 30,
              text: "旧字幕"
            }
          ]
        }
      ]
    }
  };
}

function videoAssetFixture(): Record<string, unknown> {
  return {
    asset_id: "clip",
    storage_mode: "reference",
    kind: "video",
    source: "local_path",
    filename: "clip.mp4",
    hash: "h_clip",
    size: 1024,
    mtime: 0,
    ingest_status: "indexed",
    understanding_status: "none",
    usable: true,
    rel_dir: null,
    probe: null,
    duration_sec: null,
    proxy_object_hash: null,
    proxy_ready: true,
    thumbnail_ready: true,
    invalid: false,
    failure: null,
    jobs: []
  };
}

function editingStyleDecision(overrides: Partial<Decision> = {}): Decision {
  const answer = (overrides.answer ?? null) as DecisionAnswer | null;
  return {
    allow_free_text: true,
    answer,
    blocking: true,
    consumed_at: null,
    created_by_tool_call_id: "tc_1",
    decision_id: "dec_style",
    draft_id: "draft_1",
    options: [
      { option_id: "fast", label: "快节奏" },
      { option_id: "calm", label: "舒缓" },
      { option_id: "story", label: "叙事" },
      { option_id: "energetic", label: "活力" },
      { option_id: "minimal", label: "极简" }
    ],
    pending_tool_call: null,
    pending_tool_call_status: null,
    question: "这次希望采用哪种剪辑风格？",
    replayed_tool_call_id: null,
    scope_type: "draft",
    status: "pending",
    type: "generic",
    ...overrides
  };
}

function jobProgressPayload(progress: number): DomainSsePayload {
  return {
    event_id: 10,
    event: {
      event: "JobProgress",
      draft_id: "draft_1",
      payload: {
        requested_by_draft_id: "draft_1",
        job_id: "job_1",
        kind: "understand",
        progress,
        current_asset_id: "asset_2",
        done: 1,
        total: 5,
        stage: "view_frames",
        detail: "理解素材 2/5：采访.mp4 正在调用 VLM"
      }
    }
  };
}

function jobEventPayload(eventName: string, kind: string, jobId = "job_1"): DomainSsePayload {
  return {
    event_id: 11,
    event: {
      event: eventName,
      draft_id: "draft_1",
      payload: { requested_by_draft_id: "draft_1", job_id: jobId, kind }
    }
  };
}

function activityEventPayload(eventId: number, eventName: string): DomainSsePayload {
  return {
    event_id: eventId,
    event: {
      event: eventName,
      draft_id: "draft_1",
      payload: {
        artifact_id: `preview_${eventId}`,
        timeline_version: eventId
      }
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
