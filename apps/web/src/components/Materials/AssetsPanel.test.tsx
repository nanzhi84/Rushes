import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import type { ComponentProps } from "react";
import { describe, expect, it, vi } from "vitest";
import type { MaterialAsset } from "../../api/client";
import { storeAuthToken } from "../../auth";
import { AssetsPanel } from "./AssetsPanel";
import { MaterialSummaryPanel } from "./MaterialSummaryPanel";

type FetchMock = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

// 素材瓦片的导入状态就地化 + 理解语义澄清：只断言瓦片自身的状态展示，不进对话流程。
describe("AssetsPanel 导入状态就地化", () => {
  it("理解进行中显示 N/M，取消入口可终止当前理解", async () => {
    const onCancelUnderstanding = vi.fn();
    renderPanel([], {
      understandingProgress: { completed: 2, total: 5 },
      onCancelUnderstanding
    });

    expect(await screen.findByRole("status", { name: "素材理解中 2/5" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "取消素材理解" }));
    expect(onCancelUnderstanding).toHaveBeenCalledTimes(1);
  });

  it("缩略图未就绪显示 kind 占位（脉冲），proxy/index 在跑显示旋转点", async () => {
    renderPanel([
      makeAsset({
        asset_id: "a",
        filename: "a.mp4",
        kind: "video",
        thumbnail_ready: false,
        jobs: [{ job_id: "j1", kind: "proxy", status: "running" }]
      })
    ]);

    expect(await screen.findByLabelText("视频处理中")).toBeTruthy();
    expect(screen.getByLabelText("转码与索引处理中")).toBeTruthy();
    // 占位态下没有真实缩略图
    expect(screen.queryByAltText("a.mp4 缩略图")).toBeNull();
  });

  it("缩略图就绪换真图，且后台任务收尾后不再显示旋转点", async () => {
    renderPanel([
      makeAsset({
        asset_id: "b",
        filename: "b.mp4",
        kind: "video",
        thumbnail_ready: true,
        jobs: [{ job_id: "j2", kind: "proxy", status: "succeeded" }]
      })
    ]);

    expect(await screen.findByAltText("b.mp4 缩略图")).toBeTruthy();
    expect(screen.queryByLabelText("转码与索引处理中")).toBeNull();
  });

  it("理解状态点：未理解不渲染，理解中/已理解才渲染", async () => {
    renderPanel([
      makeAsset({ asset_id: "none", filename: "none.mp4", understanding_status: "none" }),
      makeAsset({ asset_id: "run", filename: "run.mp4", understanding_status: "running" }),
      makeAsset({ asset_id: "ready", filename: "ready.mp4", understanding_status: "ready" })
    ]);

    // 三条素材里只有「理解中/已理解」两条渲染状态点，「未理解」不渲染
    await screen.findByLabelText("理解状态：已理解");
    expect(screen.getAllByLabelText(/^理解状态：/)).toHaveLength(2);
    expect(screen.getByLabelText("理解状态：理解中")).toBeTruthy();
    expect(screen.queryByLabelText("理解状态：未理解")).toBeNull();
  });
});

describe("AssetsPanel 单击试看 / 右键摘要", () => {
  it("单击瓦片触发 onPreviewAsset，且不打开摘要抽屉", async () => {
    const onPreviewAsset = vi.fn();
    renderPanel([makeAsset({ asset_id: "clip", filename: "clip.mp4" })], {
      management: true,
      onPreviewAsset
    });

    fireEvent.click(await screen.findByTitle("clip.mp4"));

    expect(onPreviewAsset).toHaveBeenCalledTimes(1);
    expect(onPreviewAsset.mock.calls[0]?.[0]?.asset_id).toBe("clip");
    // 单击不再开摘要抽屉
    expect(screen.queryByText("素材理解摘要")).toBeNull();
  });

  it("瓦片主点击面有可访问名「试看 {文件名}」，经 role+name 可达且触发试看", async () => {
    const onPreviewAsset = vi.fn();
    renderPanel([makeAsset({ asset_id: "clip", filename: "clip.mp4" })], {
      management: true,
      onPreviewAsset
    });

    // 原生 <button> 本就键盘可达（Enter/Space 由浏览器激活）；缺的是显性可访问名——
    // 用 role+name 定位即验证读屏/键盘用户能唯一命中主点击面，且区别于 ⋯ 的「更多操作」。
    const tile = await screen.findByRole("button", { name: "试看 clip.mp4" });
    expect(screen.getByRole("button", { name: "素材 clip.mp4 更多操作" })).toBeTruthy();

    fireEvent.click(tile);
    expect(onPreviewAsset).toHaveBeenCalledTimes(1);
    expect(onPreviewAsset.mock.calls[0]?.[0]?.asset_id).toBe("clip");
  });

  it("右键菜单「查看理解摘要」仍打开摘要抽屉", async () => {
    renderPanel([makeAsset({ asset_id: "clip", filename: "clip.mp4" })], { management: true });

    fireEvent.contextMenu(await screen.findByTitle("clip.mp4"));
    fireEvent.click(await screen.findByText("查看理解摘要"));

    expect(await screen.findByText("素材理解摘要")).toBeTruthy();
  });
});

describe("MaterialSummaryPanel 理解语义澄清", () => {
  it("未理解时提示理解是对话里按需调用的工具（understand.materials）", () => {
    renderSummary(makeAsset({ understanding_status: "none" }));

    expect(
      screen.getByText(
        /尚未理解。剪辑对话中，代理会按需调用理解工具（understand\.materials）生成摘要/
      )
    ).toBeTruthy();
  });

  it("理解中时提示无需手动等待", () => {
    renderSummary(makeAsset({ understanding_status: "running" }));

    expect(screen.getByText(/正在理解该素材/)).toBeTruthy();
  });
});

function renderPanel(
  assets: MaterialAsset[],
  props: Partial<ComponentProps<typeof AssetsPanel>> = {}
): void {
  storeAuthToken("test-token");
  vi.stubGlobal("fetch", materialsFetch(assets));
  render(
    <QueryClientProvider client={testQueryClient()}>
      <AssetsPanel draftId="draft_1" enableEvents={false} {...props} />
    </QueryClientProvider>
  );
}

function renderSummary(asset: MaterialAsset): void {
  render(
    <QueryClientProvider client={testQueryClient()}>
      <MaterialSummaryPanel draftId="draft_1" asset={asset} onClose={() => {}} />
    </QueryClientProvider>
  );
}

function materialsFetch(assets: MaterialAsset[]): FetchMock {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/materials")) {
      return jsonResponse({ draft_id: "draft_1", assets, invalidated_asset_ids: [] });
    }
    return jsonResponse({});
  });
}

function makeAsset(overrides: Partial<MaterialAsset> = {}): MaterialAsset {
  return {
    asset_id: "asset_1",
    storage_mode: "reference",
    kind: "video",
    source: "local_path",
    filename: "clip.mp4",
    hash: "hash_1",
    size: 1024,
    mtime: 0,
    ingest_status: "indexed",
    understanding_status: "none",
    usable: true,
    rel_dir: null,
    probe: null,
    duration_sec: null,
    proxy_object_hash: null,
    proxy_ready: false,
    thumbnail_ready: true,
    invalid: false,
    failure: null,
    jobs: [],
    ...overrides
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
