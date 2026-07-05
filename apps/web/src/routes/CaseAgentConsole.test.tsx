import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../auth";
import { CaseConsoleView } from "./CaseAgentConsole";

type Listener = (event: MessageEvent<string>) => void;

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
  it("发送消息后禁用输入框，并在 TurnEnded SSE 后恢复", async () => {
    storeAuthToken("test-token");
    MockEventSource.instances = [];
    vi.stubGlobal("EventSource", MockEventSource);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/decisions/current")) {
          return jsonResponse({ decision: null });
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
        return jsonResponse({});
      })
    );

    render(
      <QueryClientProvider client={testQueryClient()}>
        <CaseConsoleView projectId="project_1" caseId="case_1" />
      </QueryClientProvider>
    );

    const input = screen.getByLabelText("消息输入") as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "剪掉开头 3 秒" } });
    fireEvent.click(screen.getByText("发送"));

    await waitFor(() => expect(input.disabled).toBe(true));
    expect(screen.getByText("剪掉开头 3 秒")).toBeTruthy();
    expect(MockEventSource.instances[0].url).toContain("token=test-token");

    act(() => {
      MockEventSource.instances[0].emit("TurnEnded", {
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
});

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
