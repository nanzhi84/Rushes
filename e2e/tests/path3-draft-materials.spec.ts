import { expect, test, type APIRequestContext } from "@playwright/test";
import { execFile } from "node:child_process";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{ asset_id: string; filename: string }>;
};

type DraftResponse = {
  draft: {
    draft_id: string;
    export_current_id: string | null;
    timeline_current_version: number | null;
  };
};

type DraftMutationResponse = {
  draft: { draft_id: string };
  event_ids: number[];
};

const execFileAsync = promisify(execFile);
const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR = path.join(REPO_ROOT, ".e2e-workspace");
const FIXTURE_DIR = path.join(WORKSPACE_DIR, "fixtures");
const FIXTURE_NAME = "path3-fixture.mp4";
const FIXTURE_PATH = path.join(FIXTURE_DIR, FIXTURE_NAME);
const API_URL = "http://127.0.0.1:18000";
const TOKEN = "e2e-token";
const CLIP_ID = "e2e_clip_draft_a";
const EXPORT_ID = "e2e_export_draft_a";
const MEMORY_ID = "e2e_mem_draft_a";
// 记忆按 token 命中：草稿 B 的目标须含一个是记忆内容子串的 CJK token（靠空格断词）。
const REUSE_GOAL = "护肤口播 前三秒";

test("路径 3：草稿创建、素材导入与全局复用、memory 注入、时间线只读", async ({
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
  await expect(page.getByText("剪辑对话")).toBeVisible();
  const draftAId = idFromUrl(page.url());

  // 2. 中栏素材面板导入 fixture（原生选择框 → reference 原地索引）→ 断言文件名出现。
  await page.getByRole("button", { name: "导入素材" }).click();
  await expect(page.getByText(FIXTURE_NAME)).toBeVisible();
  const assetId = await waitForImportedAsset(request, draftAId);

  // 3. seed 草稿 A：时间线 v1 + 校验 + 导出 + user 记忆（reducer 版本链，草稿本体已由 REST 建好）。
  await runSeed("seed-draft-a", [
    "--workspace",
    WORKSPACE_DIR,
    "--draft-id",
    draftAId,
    "--asset-id",
    assetId,
    "--fixture-path",
    FIXTURE_PATH
  ]);

  const draftA = await apiGet<DraftResponse>(request, `/api/drafts/${draftAId}`);
  expect(draftA.draft.timeline_current_version).toBe(1);
  expect(draftA.draft.export_current_id).toBe(EXPORT_ID);

  // 4. 重载草稿 A 编辑器 → 时间线只读展示（版本选择器 = 1，seed 的 clip 可见）。
  await page.goto(`/drafts/${draftAId}`);
  await expect(page.getByLabel("时间线版本")).toHaveValue("1");
  await expect(
    page.locator(`[data-testid="timeline-clip"][data-clip-id="${CLIP_ID}"]`)
  ).toBeVisible();

  // 5. 建草稿 B（经 REST 带 goal——UI「开始创作」无目标输入）→ 进编辑器。
  const draftB = await apiPost<DraftMutationResponse>(request, "/api/drafts", { goal: REUSE_GOAL });
  const draftBId = draftB.draft.draft_id;
  await page.goto(`/drafts/${draftBId}`);
  await expect(page.getByText("剪辑对话")).toBeVisible();

  // 6. 草稿 B 导入同一 fixture → 全局去重复用：文件名出现，且 asset_id 与草稿 A 相同（同物理文件秒建链）。
  await page.getByRole("button", { name: "导入素材" }).click();
  await expect(page.getByText(FIXTURE_NAME)).toBeVisible();
  const reusedAssetId = await waitForImportedAsset(request, draftBId);
  expect(reusedAssetId).toBe(assetId);

  // 7. user 域记忆按 goal token 命中，注入草稿 B 的记忆上下文。
  await runSeed("verify-memory", [
    "--workspace",
    WORKSPACE_DIR,
    "--draft-id",
    draftBId,
    "--memory-id",
    MEMORY_ID
  ]);
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
