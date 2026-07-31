import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { UserExportRecord } from "../../api/client";
import { storeAuthToken } from "../../auth";
import { ExportControl } from "./ExportControl";

type FetchMock = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

describe("ExportControl 用户最终导出", () => {
  it("直接调用用户导出 API，并固定服务端 timeline_id 与画幅", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = [];
    const fetchMock: FetchMock = vi.fn(async (input, init) => {
      const url = String(input);
      requests.push({ url, init });
      if ((init?.method ?? "GET") === "GET") {
        return jsonResponse({ exports: [] });
      }
      return jsonResponse(exportFixture({ status: "pending", orientation: "portrait" }), 202);
    });
    renderControl(fetchMock);

    fireEvent.change(await screen.findByLabelText("导出画幅"), {
      target: { value: "portrait" }
    });
    fireEvent.click(screen.getByRole("button", { name: "导出 v7" }));

    await screen.findByText("v7 已排队");
    const request = requests.find((item) => item.init?.method === "POST");
    expect(request?.url).toBe("/api/drafts/draft_1/exports");
    expect(JSON.parse(String(request?.init?.body))).toEqual({
      timeline_id: "draft_1:v7",
      orientation: "portrait"
    });
  });

  it("失败只通过显式重试端点继续原版本", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = [];
    const failed = exportFixture({
      job_id: "job_failed",
      status: "failed",
      timeline_id: "draft_1:v3",
      timeline_version: 3,
      retryable: true,
      error: { error_code: "job_handler_failed", message: "渲染失败", retryable: true }
    });
    const fetchMock: FetchMock = vi.fn(async (input, init) => {
      const url = String(input);
      requests.push({ url, init });
      if ((init?.method ?? "GET") === "GET") {
        return jsonResponse({ exports: [failed] });
      }
      return jsonResponse(
        exportFixture({
          job_id: "job_retry",
          status: "pending",
          timeline_id: "draft_1:v3",
          timeline_version: 3,
          retry_of_job_id: "job_failed"
        }),
        202
      );
    });
    renderControl(fetchMock, { timelineId: "draft_1:v3", timelineVersion: 3 });

    fireEvent.click(await screen.findByRole("button", { name: "重试 v3" }));

    await waitFor(() => {
      expect(
        requests.some(
          (request) =>
            request.url === "/api/drafts/draft_1/exports/job_failed/retry" &&
            request.init?.method === "POST"
        )
      ).toBe(true);
    });
  });

  it("时间线已编辑到新版本时仍可显式重试旧版本，同时允许导出新版本", async () => {
    const requests: Array<{ url: string; init?: RequestInit }> = [];
    const failed = exportFixture({
      job_id: "job_failed_v3",
      status: "failed",
      timeline_id: "draft_1:v3",
      timeline_version: 3,
      retryable: true,
      error: { error_code: "render_failed", message: "渲染失败", retryable: true }
    });
    const fetchMock: FetchMock = vi.fn(async (input, init) => {
      const url = String(input);
      requests.push({ url, init });
      if ((init?.method ?? "GET") === "GET") {
        return jsonResponse({ exports: [failed] });
      }
      return jsonResponse(
        exportFixture({
          job_id: "job_retry_v3",
          status: "pending",
          timeline_id: "draft_1:v3",
          timeline_version: 3,
          retry_of_job_id: "job_failed_v3"
        }),
        202
      );
    });
    renderControl(fetchMock, { timelineId: "draft_1:v4", timelineVersion: 4 });

    expect(await screen.findByRole("button", { name: "重试 v3" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "导出 v4" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "重试 v3" }));

    await waitFor(() => {
      expect(
        requests.some(
          (request) =>
            request.url === "/api/drafts/draft_1/exports/job_failed_v3/retry" &&
            request.init?.method === "POST"
        )
      ).toBe(true);
    });
  });

  it("完成后提供带鉴权 token 的 MP4 下载链接", async () => {
    const succeeded = exportFixture({ status: "succeeded", export_id: "export_7", progress: 1 });
    renderControl(async () => jsonResponse({ exports: [succeeded] }));

    const link = await screen.findByRole("link", { name: "下载 v7" });
    expect(link.getAttribute("href")).toBe("/api/media/export/export_7?token=test-token");
    expect(link.getAttribute("download")).toBe("rushes-v7.mp4");
  });

  it("继续编辑到新版本后保留旧版本提示、下载入口与新版本导出动作", async () => {
    const oldExport = exportFixture({
      status: "succeeded",
      timeline_id: "draft_1:v7",
      timeline_version: 7,
      export_id: "export_7",
      progress: 1
    });
    renderControl(async () => jsonResponse({ exports: [oldExport] }), {
      timelineId: "draft_1:v8",
      timelineVersion: 8
    });

    expect(await screen.findByText("已有 v7 成片，当前 v8")).toBeTruthy();
    expect(screen.getByRole("link", { name: "下载 v7" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "导出 v8" })).toBeTruthy();
  });

  it("上层保存态或租约禁用时不允许提交", async () => {
    const fetchMock: FetchMock = vi.fn(async () => jsonResponse({ exports: [] }));
    renderControl(fetchMock, { disabled: true, disabledReason: "Agent 正在编辑" });

    const button = await screen.findByRole("button", { name: "导出 v7" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(button.getAttribute("title")).toBe("Agent 正在编辑");
    fireEvent.click(button);
    expect(vi.mocked(fetchMock).mock.calls.every(([, init]) => init?.method !== "POST")).toBe(true);
  });
});

function renderControl(
  fetchMock: FetchMock,
  overrides: Partial<{
    timelineId: string | null;
    timelineVersion: number | null;
    disabled: boolean;
    disabledReason: string;
  }> = {}
): void {
  storeAuthToken("test-token");
  vi.stubGlobal("fetch", fetchMock);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } }
  });
  render(
    <QueryClientProvider client={client}>
      <ExportControl
        draftId="draft_1"
        timelineId={overrides.timelineId ?? "draft_1:v7"}
        timelineVersion={overrides.timelineVersion ?? 7}
        disabled={overrides.disabled ?? false}
        disabledReason={overrides.disabledReason}
      />
    </QueryClientProvider>
  );
}

function exportFixture(overrides: Partial<UserExportRecord> = {}): UserExportRecord {
  return {
    job_id: "job_7",
    status: "pending",
    timeline_id: "draft_1:v7",
    timeline_version: 7,
    orientation: "auto",
    progress: 0,
    export_id: null,
    profile: null,
    retryable: false,
    retry_of_job_id: null,
    attempts: 0,
    max_retries: 0,
    created_at: "2026-07-31T00:00:00Z",
    started_at: null,
    finished_at: null,
    ...overrides
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}
