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

  it("Pencil 进入编辑，保存走 PATCH 且列表显示新 statement", async () => {
    storeAuthToken("e2e-token");
    let statement = "成片节奏偏快";
    const patched = vi.fn();
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url.endsWith("/api/memories") && method === "GET") {
        return jsonResponse({ memories: [memoryPayload(statement, "")] });
      }
      if (url.includes("/api/memories/pacing") && method === "PATCH") {
        patched();
        statement = JSON.parse(String(init?.body)).statement as string;
        return jsonResponse(memoryPayload(statement, "2026-07-18T01:00:00Z"));
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
    fireEvent.change(textarea, { target: { value: "用户手动改为整体更紧凑" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(patched).toHaveBeenCalledTimes(1));
    await screen.findByText("用户手动改为整体更紧凑");
    expect(screen.getByText("手动修订")).toBeTruthy();
  });
});
