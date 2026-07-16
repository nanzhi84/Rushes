import { expect, test, type APIRequestContext } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{
    asset_id: string;
    filename: string;
    ingest_status: string;
    usable: boolean;
    jobs: Array<{ kind: string; status: string }>;
  }>;
};

type DraftResponse = {
  draft: {
    draft_id: string;
    export_current_id: string | null;
    timeline_current_version: number | null;
    preview_current_id: string | null;
  };
};

type DraftMutationResponse = {
  draft: { draft_id: string };
  event_ids: number[];
};

type TimelineResponse = {
  timeline_version: number;
  timeline: {
    fps: number;
    duration_frames: number;
    tracks: Array<{
      clips?: Array<{
        timeline_clip_id?: string;
        asset_id?: string;
        source_start_frame?: number;
        source_end_frame?: number;
      }>;
    }>;
  };
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR =
  process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURE_DIR = path.join(WORKSPACE_DIR, "fixtures");
const FIXTURE_NAME = "path3-fixture.mp4";
const FIXTURE_PATH = path.join(FIXTURE_DIR, FIXTURE_NAME);
const API_URL = `http://127.0.0.1:${process.env.RUSHES_E2E_API_PORT ?? "18001"}`;
const TOKEN = "e2e-token";

test("Go 主线：导入、理解、时间线、预览、确认与最终导出", async ({
  page,
  request
}) => {
  // 原生对话框无法无头驱动：拦截 fs/pick 返回 fixture 绝对路径，其后 reference 导入链路全部真实执行。
  await page.route("**/api/fs/pick", async (route) => {
    expect(route.request().postDataJSON()).toEqual({ mode: "mixed" });
    await route.fulfill({ json: { available: true, paths: [FIXTURE_PATH] } });
  });

  // 1. 首页草稿墙 →「开始创作」= POST /drafts → 直接进编辑器（无表单）。
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "草稿" })).toBeVisible();
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  await expect(page.getByRole("complementary", { name: "剪辑对话" })).toBeVisible();
  const draftAId = idFromUrl(page.url());

  // 2. 中栏素材面板导入 fixture（原生选择框 → reference 原地索引）→ 断言文件名出现。
  await page.getByRole("button", { name: "导入素材" }).click();
  await expect(page.getByText(FIXTURE_NAME)).toBeVisible();
  const assetId = await waitForImportedAsset(request, draftAId);

  await page.getByLabel("消息输入").fill("E2E_FULL_MAINLINE");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.getByRole("status", { name: /素材理解中/ })).toHaveCount(0);
  await expect(page.getByRole("img", { name: "时间线轨道图" })).toBeVisible({
    timeout: 60_000
  });
  await expect(page.getByLabel("时间线版本")).toHaveCount(0);
  const initialPreview = page
    .getByLabel("Diffusion Studio 代理预览")
    .or(page.getByRole("region", { name: "Video Player" }));
  await expect(initialPreview).toBeVisible({ timeout: 60_000 });
  // 先确认初版时间线的后台预览已落库，再提交手工裁剪。否则渲染 v1
  // 与创建 v2 并发时，Reducer 会正确拒绝把旧预览设为当前预览，导致
  // 用例依赖机器快慢而随机失败。
  const renderedInitial = await waitForDraft(request, draftAId, (draft) =>
    Boolean(draft.preview_current_id)
  );
  expect(renderedInitial.preview_current_id).toBeTruthy();

  // 回归“素材中段取片”：Diffusion Core 的 delay 必须扣掉 source_start_frame。
  // 旧实现同时把 source start 算入 delay/range，在时间线 0 帧会没有活跃 clip，画布稳定黑屏。
  const timelineBeforeTrim = await apiGet<TimelineResponse>(
    request,
    `/api/drafts/${draftAId}/timeline`
  );
  const visualClip = timelineBeforeTrim.timeline.tracks
    .flatMap((track) => track.clips ?? [])
    .find((clip) => clip.asset_id === assetId);
  expect(visualClip?.timeline_clip_id).toBeTruthy();
  expect(visualClip?.source_end_frame).toBeGreaterThanOrEqual(30);
  const trimmedTimeline = await apiPost<TimelineResponse>(
    request,
    `/api/drafts/${draftAId}/timeline/patch`,
    {
      op: {
        kind: "trim_clip",
        timeline_clip_id: visualClip?.timeline_clip_id,
        source_start_frame: 15,
        source_end_frame: 30
      }
    }
  );
  expect(trimmedTimeline.timeline_version).toBeGreaterThan(
    timelineBeforeTrim.timeline_version
  );
  expect(
    trimmedTimeline.timeline.tracks
      .flatMap((track) => track.clips ?? [])
      .find((clip) => clip.timeline_clip_id === visualClip?.timeline_clip_id)?.source_start_frame
  ).toBe(15);

  // 页面重载后必须从最新服务端时间线重建本地预览。将 WebGL canvas
  // 缩放到小型 2D canvas 取样，验收的是真实画面而不是“播放头有走”。
  await page.reload();
  await expect(page.getByLabel("Diffusion Studio 代理预览")).toBeVisible({
    timeout: 60_000
  });
  const previewProgress = page.getByRole("slider", { name: "预览进度" });
  await previewProgress.press("Home");
  await expect
    .poll(async () => Number(await previewProgress.inputValue()))
    .toBeLessThan(0.05);
  const previewCanvas = page.locator('canvas[data-diffusion-preview="true"]');
  await expect(previewCanvas).toBeVisible();
  await expect
    .poll(async () => canvasNonBlackRatio(previewCanvas), { timeout: 15_000 })
    .toBeGreaterThan(0.2);
  const previewStart = Number(await previewProgress.inputValue());
  await page.getByRole("button", { name: "播放", exact: true }).click();
  await expect
    .poll(async () => Number(await previewProgress.inputValue()), { timeout: 5_000 })
    .toBeGreaterThan(previewStart + 0.1);
  await page.getByRole("button", { name: "暂停", exact: true }).click();
  await page.getByLabel("消息输入").fill("导出成片");
  await page.getByRole("button", { name: "发送" }).click();
  const currentDecision = page.getByRole("region", { name: /个问题待回答/ });
  await expect(currentDecision.getByRole("button", { name: "确认", exact: true })).toBeVisible();
  await currentDecision.getByRole("button", { name: "确认", exact: true }).click();
  const afterExport = await waitForDraft(request, draftAId, (draft) =>
    Boolean(draft.export_current_id)
  );
  expect(afterExport.export_current_id).toBeTruthy();

  // 同路径导入另一个草稿只建链接，不再生成一份素材。
  const draftB = await apiPost<DraftMutationResponse>(request, "/api/drafts", {});
  await apiPost<unknown>(
    request,
    `/api/drafts/${draftB.draft.draft_id}/materials/import-local`,
    { paths: [FIXTURE_PATH], storage_mode: "reference" }
  );
  const reusedAssetId = await waitForImportedAsset(request, draftB.draft.draft_id);
  expect(reusedAssetId).toBe(assetId);
});

async function waitForImportedAsset(
  request: APIRequestContext,
  draftId: string
): Promise<string> {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    const materials = await apiGet<MaterialsResponse>(
      request,
      `/api/drafts/${draftId}/materials`
    );
    const asset = materials.assets.find((item) => item.filename === FIXTURE_NAME);
    const ingestSucceeded = asset?.jobs.some(
      (job) => job.kind === "ingest" && job.status === "succeeded"
    );
    if (asset?.ingest_status === "ready" && asset.usable && ingestSucceeded) {
      return asset.asset_id;
    }
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  throw new Error(`asset not imported: ${FIXTURE_NAME}`);
}

async function apiGet<T>(request: APIRequestContext, pathName: string): Promise<T> {
  const response = await request.get(`${API_URL}${pathName}`, {
    headers: { Authorization: `Bearer ${TOKEN}` }
  });
  expect(response.ok()).toBe(true);
  return (await response.json()) as T;
}

async function apiPost<T>(
  request: APIRequestContext,
  pathName: string,
  body: unknown
): Promise<T> {
  const response = await request.post(`${API_URL}${pathName}`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
    data: body
  });
  expect(response.ok()).toBe(true);
  return (await response.json()) as T;
}

async function waitForDraft(
  request: APIRequestContext,
  draftId: string,
  predicate: (draft: DraftResponse["draft"]) => boolean
): Promise<DraftResponse["draft"]> {
  const deadline = Date.now() + 60_000;
  let latest: DraftResponse["draft"] | null = null;
  while (Date.now() < deadline) {
    latest = (await apiGet<DraftResponse>(request, `/api/drafts/${draftId}`)).draft;
    if (predicate(latest)) {
      return latest;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`draft did not reach expected state: ${JSON.stringify(latest)}`);
}

function idFromUrl(url: string): string {
  const parsed = new URL(url);
  const parts = parsed.pathname.split("/").filter(Boolean);
  const index = parts.indexOf("drafts");
  if (index === -1 || !parts[index + 1]) {
    throw new Error(`missing draft id in ${url}`);
  }
  return decodeURIComponent(parts[index + 1]);
}

async function canvasNonBlackRatio(
  canvas: import("@playwright/test").Locator
): Promise<number> {
  return canvas.evaluate((source) => {
    const sample = document.createElement("canvas");
    sample.width = 32;
    sample.height = 32;
    const context = sample.getContext("2d", { willReadFrequently: true });
    if (!context) {
      return 0;
    }
    context.drawImage(source, 0, 0, sample.width, sample.height);
    const pixels = context.getImageData(0, 0, sample.width, sample.height).data;
    let nonBlack = 0;
    for (let index = 0; index < pixels.length; index += 4) {
      if (
        Math.max(pixels[index] ?? 0, pixels[index + 1] ?? 0, pixels[index + 2] ?? 0) > 20
      ) {
        nonBlack += 1;
      }
    }
    return nonBlack / (pixels.length / 4);
  });
}
