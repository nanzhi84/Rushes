import { expect, test, type APIRequestContext } from "@playwright/test";
import { execFile } from "node:child_process";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{ asset_id: string; filename: string; enabled: boolean }>;
};

type CaseResponse = {
  case: {
    case_id: string;
    project_id: string;
    export_current_id: string | null;
    selected_asset_ids: string[];
  };
};

type CaseMutationResponse = CaseResponse & {
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
const PROJECT_A_ID = "e2e_project_a";
const CASE_A_ID = "e2e_case_a";
const CASE_B_NAME = "护肤口播 Case B";
const EXPORT_ID = "e2e_export_case_a";

test("路径 3：Project/Case 管理、素材复用、memory 注入与移动后链接保持", async ({
  page,
  request
}) => {
  await page.goto(`/#t=${TOKEN}`);
  await expect(page.getByRole("heading", { name: "项目" })).toBeVisible();

  await page.getByRole("button", { name: "＋ 新建项目" }).click();
  await page.getByLabel("名称").fill("Project B");
  await page.getByRole("button", { name: "确认" }).click();
  await expect(page).toHaveURL(/\/projects\//);
  await expect(page.getByText("Project B")).toBeVisible();
  const projectBId = idFromUrl(page.url(), "projects");

  // 旧素材页路由应重定向到项目详情素材 tab；原生对话框无法无头驱动，
  // 拦截 fs/pick 返回 fixture 绝对路径，后续 reference 导入链路全部真实执行。
  await page.route("**/api/fs/pick", (route) =>
    route.fulfill({ json: { available: true, paths: [FIXTURE_PATH] } })
  );
  await page.goto(`/projects/${PROJECT_A_ID}/materials`);
  await expect(page.getByRole("button", { name: "重新检测失效" })).toBeVisible();
  await page.getByRole("button", { name: "＋ 导入素材" }).click();
  await expect(page.getByText(FIXTURE_NAME)).toBeVisible();

  const assetId = await waitForImportedAsset(request, PROJECT_A_ID);
  await runSeed("seed-case-a", [
    "--workspace",
    WORKSPACE_DIR,
    "--asset-id",
    assetId,
    "--fixture-path",
    FIXTURE_PATH
  ]);

  const caseA = await apiGet<CaseResponse>(
    request,
    `/api/projects/${PROJECT_A_ID}/cases/${CASE_A_ID}`
  );
  expect(caseA.case.export_current_id).toBe(EXPORT_ID);
  expect(caseA.case.selected_asset_ids).toContain(assetId);

  await page.goto(`/projects/${PROJECT_A_ID}/cases/${CASE_A_ID}`);
  await expect(page.getByText("剪辑对话")).toBeVisible();
  await expect(page.getByLabel("时间线版本")).toHaveValue("1");
  await expect(page.locator('[data-testid="timeline-clip"][data-clip-id="e2e_clip_project_a"]')).toBeVisible();

  await page.goto(`/projects/${PROJECT_A_ID}`);
  await page.getByLabel("目标文本").fill(CASE_B_NAME);
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await expect(page.getByText("剪辑对话")).toBeVisible();
  const caseBId = idFromUrl(page.url(), "cases");

  await runSeed("verify-memory", [
    "--workspace",
    WORKSPACE_DIR,
    "--case-id",
    caseBId,
    "--memory-id",
    "e2e_mem_project_a"
  ]);

  const selected = await apiPost<CaseMutationResponse>(
    request,
    `/api/projects/${PROJECT_A_ID}/cases/${caseBId}/assets/select`,
    { asset_id: assetId }
  );
  expect(selected.case.selected_asset_ids).toContain(assetId);

  // 回项目详情，经 Case 卡片悬浮菜单发起移动
  await page.goto(`/projects/${PROJECT_A_ID}`);
  const caseCard = page.getByRole("button", { name: CASE_B_NAME }).locator("..");
  await caseCard.hover();
  await caseCard.getByRole("button", { name: `剪辑任务 ${CASE_B_NAME} 更多操作` }).click();
  await caseCard.getByRole("button", { name: "移动" }).click();
  await page.getByLabel("目标项目").selectOption({ label: "Project B" });
  await page.getByLabel(/确认移动剪辑任务/).check();
  await page.getByRole("button", { name: "确认" }).click();

  await expect(page).toHaveURL(new RegExp(`/projects/${escapeRegExp(projectBId)}/cases/${escapeRegExp(caseBId)}$`));
  await expect(page.getByText("剪辑对话")).toBeVisible();

  const movedCase = await apiGet<CaseResponse>(
    request,
    `/api/projects/${projectBId}/cases/${caseBId}`
  );
  expect(movedCase.case.project_id).toBe(projectBId);
  expect(movedCase.case.selected_asset_ids).toContain(assetId);

  const projectBMaterials = await apiGet<MaterialsResponse>(
    request,
    `/api/projects/${projectBId}/materials`
  );
  expect(projectBMaterials.assets.some((asset) => asset.asset_id === assetId)).toBe(true);

  await page.goto(`/projects/${projectBId}/materials`);
  await expect(page.getByText(FIXTURE_NAME)).toBeVisible();
});

async function waitForImportedAsset(
  request: APIRequestContext,
  projectId: string
): Promise<string> {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    const materials = await apiGet<MaterialsResponse>(
      request,
      `/api/projects/${projectId}/materials`
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
  await execFileAsync("uv", ["run", "python", "e2e/fixtures/seed_case_a.py", command, ...args], {
    cwd: REPO_ROOT
  });
}

function idFromUrl(url: string, segment: "projects" | "cases"): string {
  const parsed = new URL(url);
  const parts = parsed.pathname.split("/").filter(Boolean);
  const index = parts.indexOf(segment);
  if (index === -1 || !parts[index + 1]) {
    throw new Error(`missing ${segment} id in ${url}`);
  }
  return decodeURIComponent(parts[index + 1]);
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
