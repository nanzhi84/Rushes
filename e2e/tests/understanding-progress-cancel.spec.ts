import { expect, test, type APIRequestContext } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{
    asset_id: string;
    ingest_status: string;
    understanding_status: string;
  }>;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR = process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURE_PATH = path.join(WORKSPACE_DIR, "fixtures", "understanding-cancel-a.mp4");
const SECOND_FIXTURE_PATH = path.join(WORKSPACE_DIR, "fixtures", "understanding-cancel-b.mp4");
const API_URL = `http://127.0.0.1:${process.env.RUSHES_E2E_API_PORT ?? "18001"}`;
const TOKEN = "e2e-token";
const TRIGGER = "E2E_CANCEL_UNDERSTANDING";

test("异步素材理解显示真实进度，可取消 job 并停止 worker", async ({ page, request }) => {
  await page.goto(`/#t=${TOKEN}`);
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  const draftId = idFromUrl(page.url());

  const imported = await apiPost<{ asset_ids: string[] }>(
    request,
    `/api/drafts/${draftId}/materials/import-local`,
    { paths: [FIXTURE_PATH, SECOND_FIXTURE_PATH], storage_mode: "reference" }
  );
  expect(imported.asset_ids).toHaveLength(2);
  await waitForIngest(request, draftId, imported.asset_ids);
  await page.reload();
  await expect(page.getByText("understanding-cancel-a.mp4")).toBeVisible();
  await expect(page.getByText("understanding-cancel-b.mp4")).toBeVisible();

  await page.getByLabel("消息输入").fill(TRIGGER);
  await page.getByRole("button", { name: "发送" }).click();

  const cancel = page.getByRole("button", { name: "取消理解素材" });
  await expect(cancel).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(/理解素材 \d+\/\d+：understanding-cancel-.*\.mp4/)).toBeVisible();
  await cancel.click();

  await expect(page.getByText("已取消", { exact: true })).toBeVisible();
  await expect(cancel).toHaveCount(0);
  await page.getByRole("button", { name: "停止当前任务" }).click();

  await expect(page.getByLabel("消息输入")).toBeEnabled();

  const materials = await waitForSettledMaterials(request, draftId, imported.asset_ids);
  const finalStatuses = imported.asset_ids.map((id) => statusOf(materials, id)).sort();
  expect(finalStatuses.every((status) => status === "none" || status === "ready")).toBe(true);
  expect(finalStatuses).toContain("none");
  expect(materials.assets.some((asset) => asset.understanding_status === "running")).toBe(false);

  await expect(page.getByLabel("理解状态：理解中")).toHaveCount(0);
});

async function waitForSettledMaterials(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<MaterialsResponse> {
  const deadline = Date.now() + 15_000;
  let latest: MaterialsResponse | null = null;
  while (Date.now() < deadline) {
    latest = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    const selected = latest.assets.filter((asset) => assetIds.includes(asset.asset_id));
    if (selected.length === assetIds.length &&
      selected.every((asset) => asset.understanding_status !== "running")) {
      return latest;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`materials did not settle: ${JSON.stringify(latest)}`);
}

function statusOf(materials: MaterialsResponse, assetId: string): string | undefined {
  return materials.assets.find((asset) => asset.asset_id === assetId)?.understanding_status;
}

async function apiGet<T>(request: APIRequestContext, pathName: string): Promise<T> {
  const response = await request.get(`${API_URL}${pathName}`, {
    headers: { Authorization: `Bearer ${TOKEN}` }
  });
  expect(response.ok()).toBe(true);
  return (await response.json()) as T;
}

async function waitForIngest(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<void> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const materials = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    if (assetIds.every((id) => materials.assets.find((asset) => asset.asset_id === id))) {
      const selected = materials.assets.filter((asset) => assetIds.includes(asset.asset_id));
      if (selected.every((asset) => asset.ingest_status === "ready")) {
        return;
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error("assets did not finish ingest");
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

function idFromUrl(url: string): string {
  const parsed = new URL(url);
  const parts = parsed.pathname.split("/").filter(Boolean);
  const index = parts.indexOf("drafts");
  if (index === -1 || !parts[index + 1]) {
    throw new Error(`missing draft id in ${url}`);
  }
  return decodeURIComponent(parts[index + 1]);
}
