import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { MaterialAsset } from "../api/client";
import { storeAuthToken } from "../auth";
import { ProjectMaterialsView } from "./ProjectMaterialsPage";

describe("ProjectMaterialsView", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.sessionStorage.clear();
  });

  it("按 rel_dir 文件夹分组展示素材，可下钻并经面包屑返回", async () => {
    stubFetch({
      assets: [
        material({ asset_id: "a1", filename: "root.mp4", rel_dir: null }),
        material({ asset_id: "a2", filename: "clip1.mp4", rel_dir: "素材A/视频" }),
        material({ asset_id: "a3", filename: "cover.png", kind: "image", rel_dir: "素材A" })
      ]
    });
    renderMaterials();

    // 根层：直接导入的文件 + 「素材A」文件夹瓦片
    expect(await screen.findByText("root.mp4")).toBeTruthy();
    expect(screen.getByText("素材A")).toBeTruthy();
    expect(screen.queryByText("clip1.mp4")).toBeNull();

    fireEvent.click(screen.getByText("素材A"));

    expect(await screen.findByText("cover.png")).toBeTruthy();
    expect(screen.getByText("视频")).toBeTruthy();
    expect(screen.queryByText("root.mp4")).toBeNull();

    fireEvent.click(screen.getByText("视频"));

    expect(await screen.findByText("clip1.mp4")).toBeTruthy();

    fireEvent.click(screen.getByText("全部素材"));

    expect(await screen.findByText("root.mp4")).toBeTruthy();
  });

  it("本地导入支持勾选文件夹并批量提交 paths", async () => {
    const fetchMock = stubFetch({ assets: [] });
    renderMaterials();

    fireEvent.click(await screen.findByText("＋ 本地导入"));
    fireEvent.click(await screen.findByText("Movies"));
    fireEvent.click(await screen.findByLabelText("选择文件夹 raws"));
    fireEvent.click(screen.getByText("导入所选"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([input]) =>
        String(input).endsWith("/materials/import-local")
      );
      expect(call).toBeTruthy();
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({
        paths: ["/Movies/raws"],
        storage_mode: "reference"
      });
    });
  });

  it("导入响应含 skipped 时展示跳过提示", async () => {
    stubFetch({ assets: [], importResponse: { skipped: ["notes.txt"], asset_ids: ["a9"] } });
    renderMaterials();

    fireEvent.click(await screen.findByText("＋ 本地导入"));
    fireEvent.click(await screen.findByText("Movies"));
    fireEvent.click(await screen.findByLabelText("选择文件夹 raws"));
    fireEvent.click(screen.getByText("导入所选"));

    expect(await screen.findByText(/已跳过 1 个不支持的文件/)).toBeTruthy();
  });

  it("瓦片菜单可禁用素材", async () => {
    const fetchMock = stubFetch({
      assets: [material({ asset_id: "a1", filename: "root.mp4" })]
    });
    renderMaterials();

    await screen.findByText("root.mp4");
    fireEvent.click(screen.getByLabelText("素材 root.mp4 更多操作"));
    fireEvent.click(await screen.findByText("禁用"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([input, init]) => String(input).endsWith("/materials/a1") && init?.method === "PATCH"
      );
      expect(call).toBeTruthy();
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ enabled: false });
    });
  });

  it("瓦片菜单删除引用需 confirm 确认", async () => {
    const fetchMock = stubFetch({
      assets: [material({ asset_id: "a1", filename: "root.mp4" })]
    });
    vi.stubGlobal("confirm", vi.fn(() => true));
    renderMaterials();

    await screen.findByText("root.mp4");
    fireEvent.click(screen.getByLabelText("素材 root.mp4 更多操作"));
    fireEvent.click(await screen.findByText("删除引用"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([input]) =>
        String(input).endsWith("/materials/unlink")
      );
      expect(call).toBeTruthy();
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ asset_id: "a1" });
    });
  });

  it("点击素材瓦片打开理解摘要面板", async () => {
    stubFetch({
      assets: [material({ asset_id: "a1", filename: "root.mp4", understanding_status: "ready" })],
      summary: {
        asset_id: "a1",
        summary: {
          asset_id: "a1",
          version: 1,
          overall: "一段海边火舞视频",
          segments: [],
          created_at: "2026-07-01T00:00:00Z"
        }
      }
    });
    renderMaterials();

    await screen.findByText("root.mp4");
    fireEvent.click(screen.getByTitle("root.mp4"));

    expect(await screen.findByText(/一段海边火舞视频/)).toBeTruthy();
  });

  it("失效素材可经菜单重新定位到新路径", async () => {
    const fetchMock = stubFetch({
      assets: [
        material({ asset_id: "a1", filename: "root.mp4", invalid: true, usable: false })
      ]
    });
    renderMaterials();

    await screen.findByText("root.mp4");
    fireEvent.click(screen.getByLabelText("素材 root.mp4 更多操作"));
    fireEvent.click(await screen.findByText("重新定位"));
    fireEvent.click(await screen.findByText("Movies"));
    fireEvent.click(await screen.findByText("raws"));
    fireEvent.click(await screen.findByText("raw.mp4"));
    fireEvent.click(screen.getByText("使用此路径"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        ([input, init]) => String(input).endsWith("/materials/a1") && init?.method === "PATCH"
      );
      expect(call).toBeTruthy();
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({
        reference_path: "/Movies/raws/raw.mp4"
      });
    });
  });

  it("重新检测失效按钮触发 revalidate", async () => {
    const fetchMock = stubFetch({ assets: [] });
    renderMaterials();

    fireEvent.click(await screen.findByText("重新检测失效"));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([input]) =>
        String(input).endsWith("/materials/revalidate")
      );
      expect(call).toBeTruthy();
    });
  });
});

type StubOptions = {
  assets: MaterialAsset[];
  importResponse?: Record<string, unknown>;
  summary?: Record<string, unknown>;
};

function stubFetch(options: StubOptions): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/materials/import-local")) {
      return jsonResponse({
        project_id: "project_1",
        asset_id: "asset_new",
        asset_ids: ["asset_new"],
        skipped: [],
        event_ids: [1],
        ...(options.importResponse ?? {})
      });
    }
    if (url.endsWith("/materials/unlink") || url.endsWith("/materials/revalidate")) {
      return jsonResponse({
        project_id: "project_1",
        assets: [],
        invalidated_asset_ids: [],
        event_ids: []
      });
    }
    if (/\/materials\/[^/]+$/.test(url) && init?.method === "PATCH") {
      return jsonResponse({ project_id: "project_1", asset_id: "a1", event_ids: [] });
    }
    if (url.includes("/summary")) {
      return jsonResponse(options.summary ?? {});
    }
    if (url.endsWith("/materials")) {
      return jsonResponse({
        project_id: "project_1",
        invalidated_asset_ids: [],
        assets: options.assets
      });
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
    return jsonResponse({});
  });
  storeAuthToken("test-token");
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

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
    ingest_status: "ready",
    understanding_status: "none",
    usable: true,
    enabled: true,
    rel_dir: null,
    probe: { duration_sec: 12 },
    duration_sec: 12,
    proxy_object_hash: null,
    proxy_ready: false,
    thumbnail_ready: false,
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
