import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
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
import { CaseConsoleView } from "./CaseAgentConsole";

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

describe("CaseConsoleView", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
    MockEventSource.instances = [];
    consoleComponentMocks.reset();
  });

  it("发送消息后禁用输入框，并在 TurnEnded SSE 后恢复", async () => {
    const fetchMock = mockFetch({ decision: null });
    renderConsole(fetchMock);

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "剪掉开头 3 秒" } });
    fireEvent.click(screen.getByText("发送"));

    await waitFor(() => expect(input.disabled).toBe(true));
    expect(screen.getByText("剪掉开头 3 秒")).toBeTruthy();
    expect(MockEventSource.instances[0]?.url).toContain("token=test-token");

    act(() => {
      MockEventSource.instances[0]?.emit("TurnEnded", {
        event_id: 1,
        event: {
          event: "TurnEnded",
          project_id: "project_1",
          case_id: "case_1",
          turn_id: "turn_1"
        }
      });
    });

    await waitFor(() => expect(input.disabled).toBe(false));
  });

  it("audio_mode Decision 渲染五个选项并点击提交 button answer", async () => {
    const answerRequests: Array<{ url: string; body: unknown }> = [];
    const fetchMock = mockFetch({
      decision: audioModeDecision(),
      onAnswer: (url, init) => {
        answerRequests.push({ url, body: JSON.parse(String(init?.body)) });
      }
    });
    renderConsole(fetchMock);

    await screen.findByText("原视频里有人声，这次怎么处理声音？");
    for (const label of ["保留原声", "口播粗剪", "使用上传配音", "使用 TTS", "无旁白视频"]) {
      expect(screen.getByRole("button", { name: new RegExp(label) })).toBeTruthy();
    }

    fireEvent.click(screen.getByRole("button", { name: /口播粗剪/ }));

    await waitFor(() => expect(answerRequests).toHaveLength(1));
    expect(answerRequests[0]?.url).toBe("/api/decisions/dec_audio/answer");
    expect(answerRequests[0]?.body).toMatchObject({
      project_id: "project_1",
      case_id: "case_1",
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
    renderConsole(fetchMock);

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
    renderConsole(fetchMock);

    act(() => {
      MockEventSource.instances[0]?.emit("JobProgress", jobProgressPayload(0.42));
    });

    const progress = await screen.findByRole("progressbar", { name: "素材分析 进度" });
    expect(progress.getAttribute("aria-valuenow")).toBe("42");

    act(() => {
      MockEventSource.instances[0]?.emit("JobProgress", jobProgressPayload(0.8));
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
    renderConsole(fetchMock);

    fireEvent.click(await screen.findByTestId("mock-timeline-seek"));

    await waitFor(() => {
      const latestPreviewProps = consoleComponentMocks.previewProps.at(-1);
      expect(latestPreviewProps?.seekSec).toBe(2.5);
    });
  });
});

function renderConsole(fetchMock: FetchMock): void {
  storeAuthToken("test-token");
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource);
  vi.stubGlobal("fetch", fetchMock);

  render(
    <QueryClientProvider client={testQueryClient()}>
      <CaseConsoleView projectId="project_1" caseId="case_1" />
    </QueryClientProvider>
  );
}

function mockFetch({
  decision,
  timeline = false,
  onAnswer
}: {
  decision: Decision | null;
  timeline?: boolean;
  onAnswer?: (url: string, init: RequestInit | undefined) => void;
}): FetchMock {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/projects/project_1/cases/case_1") {
      return jsonResponse({
        case: {
          case_id: "case_1",
          project_id: "project_1",
          name: "Case 001",
          status: "active",
          timeline_current_version: timeline ? 1 : null
        }
      });
    }
    if (url.startsWith("/api/projects/project_1/cases/case_1/timeline")) {
      return jsonResponse(timelineResponseFixture());
    }
    if (url.endsWith("/decisions/current")) {
      return jsonResponse({ decision });
    }
    if (url.endsWith("/messages")) {
      return jsonResponse(
        {
          status: "queued",
          kind: "user_message",
          project_id: "project_1",
          case_id: "case_1",
          message_id: "msg_1"
        },
        202
      );
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

function timelineResponseFixture() {
  return {
    case_id: "case_1",
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
    case_id: "case_1",
    consumed_at: null,
    created_by_tool_call_id: "tc_1",
    decision_id: "dec_audio",
    options: [
      { option_id: "keep_original", label: "保留原声" },
      { option_id: "rough_cut", label: "口播粗剪" },
      { option_id: "uploaded_voiceover", label: "使用上传配音" },
      { option_id: "tts", label: "使用 TTS" },
      { option_id: "silent", label: "无旁白视频" }
    ],
    pending_tool_call: null,
    pending_tool_call_status: null,
    project_id: "project_1",
    question: "原视频里有人声，这次怎么处理声音？",
    replayed_tool_call_id: null,
    scope_type: "case",
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
      project_id: "project_1",
      requested_by_case_id: "case_1",
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
