import { expect, test, type APIRequestContext } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{ asset_id: string; filename: string }>;
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

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR = path.join(REPO_ROOT, ".e2e-workspace");
const FIXTURE_DIR = path.join(WORKSPACE_DIR, "fixtures");
const FIXTURE_NAME = "path3-fixture.mp4";
const FIXTURE_PATH = path.join(FIXTURE_DIR, FIXTURE_NAME);
const API_URL = "http://127.0.0.1:18000";
const TOKEN = "e2e-token";

test("Go 主线：导入、理解、时间线、预览、确认与最终导出", async ({
  page,
  request
}) => {
  // 原生对话框无法无头驱动：拦截 fs/pick 返回 fixture 绝对路径，其后 reference 导入链路全部真实执行。
  await page.route("**/api/fs/pick", (route) =>
    route.fulfill({ json: { available: true, paths: [FIXTURE_PATH] } })
  );

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
  await expect(page.getByRole("status", { name: /素材理解中/ })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByLabel("时间线版本")).toHaveValue("1");
  await expect(page.getByRole("region", { name: "Video Player" })).toBeVisible({
    timeout: 60_000
  });
  const afterPreview = await waitForDraft(request, draftAId, (draft) => Boolean(draft.preview_current_id));
  expect(afterPreview.preview_current_id).toBeTruthy();

  await page.getByLabel("消息输入").fill("导出成片");
  await page.getByRole("button", { name: "发送" }).click();
  const currentDecision = page.getByLabel("当前确认项");
  await expect(currentDecision.getByRole("button", { name: "确认", exact: true })).toBeVisible();
  await currentDecision.getByRole("button", { name: "确认", exact: true }).click();
  const afterExport = await waitForDraft(request, draftAId, (draft) => Boolean(draft.export_current_id));
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
    if (asset) {
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
