import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { MaterialAsset } from "../api/client";
import { ProjectMaterialsView } from "./ProjectMaterialsPage";

describe("ProjectMaterialsView", () => {
  it("按 init、parts、complete 顺序完成分片上传", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/materials")) {
        return jsonResponse({ project_id: "project_1", invalidated_asset_ids: [], assets: [] });
      }
      if (url === "/api/uploads/init") {
        return jsonResponse(
          {
            upload_id: "upload_1",
            part_url_template: "/api/uploads/upload_1/parts/{part_number}",
            complete_url: "/api/uploads/upload_1/complete"
          },
          201
        );
      }
      if (url === "/api/uploads/upload_1/parts/1" && init?.method === "PUT") {
        return jsonResponse({ upload_id: "upload_1", part_number: 1, size: 5 });
      }
      if (url === "/api/uploads/upload_1/complete") {
        return jsonResponse({
          upload_id: "upload_1",
          project_id: "project_1",
          asset_id: "asset_upload",
          event_ids: [1]
        });
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    renderMaterials();
    const input = screen.getByLabelText("选择上传文件");
    const file = new File(["hello"], "clip.mp4", { type: "video/mp4" });
    fireEvent.change(input, { target: { files: [file] } });

    expect(await screen.findByText("上传完成")).toBeTruthy();
    const mutationCalls = fetchMock.mock.calls
      .map(([input, init]) => [String(input), init?.method ?? "GET"])
      .filter(([, method]) => method !== "GET");
    expect(mutationCalls).toEqual([
      ["/api/uploads/init", "POST"],
      ["/api/uploads/upload_1/parts/1", "PUT"],
      ["/api/uploads/upload_1/complete", "POST"]
    ]);
  });

  it("支持目录浏览下钻并选择文件导入 reference 素材", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/materials")) {
        return jsonResponse({ project_id: "project_1", invalidated_asset_ids: [], assets: [] });
      }
      if (url === "/api/fs/roots") {
        return jsonResponse({ roots: [{ name: "Movies", path: "/Movies", exists: true }] });
      }
      if (url.startsWith("/api/fs/list")) {
        const parsed = new URL(url, "http://local.test");
        const path = parsed.searchParams.get("path");
        if (path === "/Movies") {
          return jsonResponse({
            path,
            entries: [{ name: "raws", path: "/Movies/raws", type: "directory" }]
          });
        }
        return jsonResponse({
          path,
          entries: [{ name: "raw.mp4", path: "/Movies/raws/raw.mp4", type: "file", size: 1024 }]
        });
      }
      if (url.endsWith("/materials/import-local")) {
        return jsonResponse({ project_id: "project_1", asset_id: "asset_1", event_ids: [1] });
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    renderMaterials();
    fireEvent.click(screen.getByRole("button", { name: "本地路径导入" }));
    fireEvent.click(await screen.findByText("Movies"));
    fireEvent.click(await screen.findByText("raws"));
    fireEvent.click(await screen.findByText("raw.mp4"));
    fireEvent.click(screen.getByRole("button", { name: "导入此文件" }));

    await waitFor(() => {
      const importCall = fetchMock.mock.calls.find(([input]) =>
        String(input).endsWith("/materials/import-local")
      );
      expect(importCall).toBeTruthy();
      expect(JSON.parse(String(importCall?.[1]?.body))).toMatchObject({
        path: "/Movies/raws/raw.mp4",
        storage_mode: "reference"
      });
    });
  });

  it("URL 导入创建 decision 卡片后可直接确认", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/materials")) {
        return jsonResponse({ project_id: "project_1", invalidated_asset_ids: [], assets: [] });
      }
      if (url.endsWith("/materials/import-url")) {
        return jsonResponse({
          project_id: "project_1",
          asset_id: null,
          decision_id: "dec_url_import",
          event_ids: [1]
        });
      }
      if (url === "/api/decisions/dec_url_import/answer") {
        return jsonResponse({
          decision_id: "dec_url_import",
          status: "answered",
          event_ids: [2],
          replays_enqueued: 1
        });
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    renderMaterials();
    fireEvent.change(screen.getByLabelText("URL"), {
      target: { value: "https://example.test/clip.mp4" }
    });
    fireEvent.change(screen.getByLabelText("文件名"), { target: { value: "clip.mp4" } });
    fireEvent.click(screen.getByRole("button", { name: "创建确认项" }));

    expect(await screen.findByText("待确认 URL 导入")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "确认导入" }));

    await waitFor(() => {
      const answerCall = fetchMock.mock.calls.find(([input]) =>
        String(input).endsWith("/decisions/dec_url_import/answer")
      );
      expect(answerCall).toBeTruthy();
      expect(JSON.parse(String(answerCall?.[1]?.body))).toMatchObject({
        project_id: "project_1",
        answer: { option_id: "approve", payload: { approved: true } }
      });
    });
  });

  it("不再渲染素材类型选择器", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({ project_id: "project_1", invalidated_asset_ids: [], assets: [] })
      )
    );

    renderMaterials();

    await screen.findByText("上传文件");
    expect(screen.queryAllByRole("combobox")).toHaveLength(0);
    expect(screen.queryByText("类型")).toBeNull();
  });

  it("上传不支持的文件时逐文件显示拒收原因", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/materials")) {
        return jsonResponse({ project_id: "project_1", invalidated_asset_ids: [], assets: [] });
      }
      if (url === "/api/uploads/init") {
        return jsonResponse(
          {
            detail: {
              error_code: "unsupported_material_type",
              message: "不支持的素材格式：.srt。支持常见视频/音频/图片/字体格式。"
            }
          },
          400
        );
      }
      return jsonResponse({});
    });
    vi.stubGlobal("fetch", fetchMock);

    renderMaterials();
    const input = screen.getByLabelText("选择上传文件");
    const file = new File(["sub"], "caption.srt", { type: "application/x-subrip" });
    fireEvent.change(input, { target: { files: [file] } });

    expect(await screen.findByText("拒收 1 个")).toBeTruthy();
    expect(await screen.findByText(/caption\.srt：不支持的素材格式：\.srt。/)).toBeTruthy();
  });

  it("失效素材显示重新定位入口", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse({
          project_id: "project_1",
          invalidated_asset_ids: ["asset_invalid"],
          assets: [
            material({
              asset_id: "asset_invalid",
              filename: "missing.mp4",
              usable: false,
              invalid: true,
              failure: { error_code: "reference_invalidated" }
            })
          ]
        })
      )
    );

    renderMaterials();

    expect(await screen.findByText("源文件失效")).toBeTruthy();
    expect(screen.getByRole("button", { name: "重新定位" })).toBeTruthy();
  });
});

function renderMaterials(): void {
  render(
    <QueryClientProvider client={testQueryClient()}>
      <ProjectMaterialsView projectId="project_1" enableEvents={false} />
    </QueryClientProvider>
  );
}

function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false }
    }
  });
}

function material(overrides: Partial<MaterialAsset> = {}): MaterialAsset {
  return {
    asset_id: "asset_1",
    storage_mode: "reference",
    kind: "video",
    source: "local_path",
    filename: "raw.mp4",
    hash: "hash",
    size: 1024,
    mtime: 1,
    ingest_status: "imported",
    usable: true,
    enabled: true,
    probe: { duration_sec: 12.5, width: 1920, height: 1080 },
    proxy_object_hash: null,
    proxy_ready: false,
    invalid: false,
    failure: null,
    jobs: [],
    ...overrides
  };
}

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}
