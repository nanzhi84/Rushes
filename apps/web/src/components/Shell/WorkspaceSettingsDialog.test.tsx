import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { storeAuthToken } from "../../auth";
import { WorkspaceSettingsDialog } from "./WorkspaceSettingsDialog";

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
}

function memoryPayload(statement: string, manuallyRevisedAt: string) {
  return {
    memory_key: "pacing",
    kind: "preference",
    statement,
    source_draft_id: "draft_1",
    created_at: "2026-07-18T00:00:00Z",
    last_confirmed_at: "2026-07-18T00:00:00Z",
    manually_revised_at: manuallyRevisedAt
  };
}

describe("WorkspaceSettingsDialog 长期记忆就地编辑", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("就地编辑保存期间「正在保存」可见，settled 后显示新 statement 与「手动修订」", async () => {
    storeAuthToken("e2e-token");
    let statement = "成片节奏偏快";
    let revisedAt = "";
    let releasePatch: () => void = () => {};
    const patchGate = new Promise<void>((resolve) => {
      releasePatch = resolve;
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url.endsWith("/api/memories") && method === "GET") {
        return jsonResponse({ memories: [memoryPayload(statement, revisedAt)] });
      }
      if (url.includes("/api/memories/pacing") && method === "PATCH") {
        statement = JSON.parse(String(init?.body)).statement as string;
        revisedAt = "2026-07-18T01:00:00Z";
        await patchGate; // 暂缓 settled，让「正在保存」可被观察
        return jsonResponse(memoryPayload(statement, revisedAt));
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <WorkspaceSettingsDialog open onClose={vi.fn()} />
      </QueryClientProvider>
    );

    await screen.findByText("成片节奏偏快");
    fireEvent.click(screen.getByRole("button", { name: "编辑长期记忆 pacing" }));
    fireEvent.change(screen.getByRole("textbox", { name: "编辑长期记忆 pacing" }), {
      target: { value: "用户手动改为整体更紧凑" }
    });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    // 保存 pending 期间保持编辑态且「正在保存」可见。
    expect(await screen.findByRole("button", { name: "正在保存" })).toBeTruthy();

    releasePatch();

    // settled 后退出编辑态，列表显示新 statement 与「手动修订」标。
    await screen.findByText("用户手动改为整体更紧凑");
    expect(await screen.findByText("手动修订")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("button", { name: "正在保存" })).toBeNull());
  });

  it("保存失败保留编辑态与草稿文本，并显示错误提示", async () => {
    storeAuthToken("e2e-token");
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url.endsWith("/api/memories") && method === "GET") {
        return jsonResponse({ memories: [memoryPayload("成片节奏偏快", "")] });
      }
      if (url.includes("/api/memories/pacing") && method === "PATCH") {
        return new Response(JSON.stringify({ detail: "boom" }), { status: 500 });
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <WorkspaceSettingsDialog open onClose={vi.fn()} />
      </QueryClientProvider>
    );

    await screen.findByText("成片节奏偏快");
    fireEvent.click(screen.getByRole("button", { name: "编辑长期记忆 pacing" }));
    const textarea = screen.getByRole("textbox", { name: "编辑长期记忆 pacing" });
    fireEvent.change(textarea, { target: { value: "改了一半的草稿" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await screen.findByText(/保存失败/);
    // 仍在编辑态，草稿文本保留。
    expect(
      (screen.getByRole("textbox", { name: "编辑长期记忆 pacing" }) as HTMLTextAreaElement).value
    ).toBe("改了一半的草稿");
  });
});
