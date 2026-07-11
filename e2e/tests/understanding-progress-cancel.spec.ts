import { expect, test, type APIRequestContext } from "@playwright/test";
import { execFile } from "node:child_process";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{
    asset_id: string;
    understanding_status: string;
  }>;
};

const execFileAsync = promisify(execFile);
const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR = path.join(REPO_ROOT, ".e2e-workspace");
const FIXTURE_PATH = path.join(WORKSPACE_DIR, "fixtures", "path3-fixture.mp4");
const API_URL = "http://127.0.0.1:18000";
const TOKEN = "e2e-token";
const TRIGGER = "E2E_CANCEL_UNDERSTANDING";
const READY_ASSET_ID = "e2e_cancel_ready";
const SLOW_ASSET_ID = "e2e_cancel_slow";

test("真实理解链路显示 N/M，可取消并保留已完成摘要", async ({ page, request }) => {
  await page.goto(`/#t=${TOKEN}`);
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  const draftId = idFromUrl(page.url());

  // 只用 E2E fixture 脚本准备两条真实素材记录；其后的消息、工具执行、SSE、取消、
  // reducer 落库和 UI 刷新均走完整产品链路，不拦截 EventSource 或 API。
  await runSeed("seed-cancel-assets", [
    "--workspace",
    WORKSPACE_DIR,
    "--draft-id",
    draftId,
    "--fixture-path",
    FIXTURE_PATH
  ]);
  await page.reload();
  await expect(page.getByText("e2e_cancel_ready.mp4")).toBeVisible();
  await expect(page.getByText("e2e_cancel_slow.mp4")).toBeVisible();

  await page.getByLabel("消息输入").fill(TRIGGER);
  await page.getByRole("button", { name: "发送" }).click();

  await expect(page.getByRole("status", { name: "素材理解中 1/2" })).toBeVisible({
    timeout: 30_000
  });
  await page.getByRole("button", { name: "取消素材理解" }).click();

  await expect(page.getByRole("status", { name: /素材理解中/ })).toHaveCount(0, {
    timeout: 30_000
  });
  await expect(page.getByLabel("消息输入")).toBeEnabled();

  const materials = await waitForSettledMaterials(request, draftId);
  expect(statusOf(materials, READY_ASSET_ID)).toBe("ready");
  expect(statusOf(materials, SLOW_ASSET_ID)).toBe("none");
  expect(materials.assets.some((asset) => asset.understanding_status === "running")).toBe(false);

  await expect(page.getByLabel("理解状态：已理解")).toBeVisible();
  await expect(page.getByLabel("理解状态：理解中")).toHaveCount(0);
});

async function waitForSettledMaterials(
  request: APIRequestContext,
  draftId: string
): Promise<MaterialsResponse> {
  const deadline = Date.now() + 15_000;
  let latest: MaterialsResponse | null = null;
  while (Date.now() < deadline) {
    latest = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    if (
      statusOf(latest, READY_ASSET_ID) === "ready" &&
      !latest.assets.some((asset) => asset.understanding_status === "running")
    ) {
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

async function runSeed(command: string, args: string[]): Promise<void> {
  await execFileAsync("uv", ["run", "python", "e2e/fixtures/seed_draft.py", command, ...args], {
    cwd: REPO_ROOT
  });
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
