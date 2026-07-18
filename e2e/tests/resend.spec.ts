import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

type DraftMutationResponse = {
  draft: { draft_id: string };
};

type DraftResponse = {
  draft: {
    preview_current_id: string | null;
    timeline_current_version: number | null;
  };
};

type MaterialsResponse = {
  assets: Array<{ filename: string; ingest_status: string; usable: boolean }>;
};

type MessagesResponse = {
  messages: Array<{ role: string; content: string }>;
  rewound_message_count: number;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR =
  process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURE_NAME = "path3-fixture.mp4";
const FIXTURE_PATH = path.join(WORKSPACE_DIR, "fixtures", FIXTURE_NAME);
const API_URL = `http://127.0.0.1:${process.env.RUSHES_E2E_API_PORT ?? "18001"}`;
const TOKEN = "e2e-token";

test("编辑并重发把对话与时间线回退到消息之前并开启新回合（含分叉）", async ({ page, request }) => {
  const created = await apiPost<DraftMutationResponse>(request, "/api/drafts", {});
  const draftId = created.draft.draft_id;
  await apiPost(request, `/api/drafts/${draftId}/materials/import-local`, {
    paths: [FIXTURE_PATH],
    storage_mode: "reference"
  });
  await waitForMaterial(request, draftId);

  await page.goto(`/#t=${TOKEN}`);
  await page.goto(`/drafts/${draftId}`);
  await expect(page.getByRole("complementary", { name: "剪辑对话" })).toBeVisible();

  // 第一轮:首剪。第二轮:再剪,此时时间线到 v2。
  await sendMessage(page, "E2E_FULL_MAINLINE 甲");
  await waitForTimelineVersion(request, draftId, 1);
  await waitForPreview(request, draftId);
  await sendMessage(page, "E2E_FULL_MAINLINE 乙");
  const beforeResend = await waitForTimelineVersion(request, draftId, 2);

  // 编辑「乙」并重发:回退到乙发出之前(时间线回到 v1),以新内容开启新回合。
  await editAndResend(page, "E2E_FULL_MAINLINE 乙", "E2E_FULL_MAINLINE 乙改");
  await expect(
    page.getByLabel("消息列表").getByText("E2E_FULL_MAINLINE 乙改", { exact: true })
  ).toBeVisible({ timeout: 60_000 });
  // 旧「乙」及其回复从对话流消失(软遮蔽 + 领域 SSE 失效)。
  await expect(
    page.getByLabel("消息列表").getByText("E2E_FULL_MAINLINE 乙", { exact: true })
  ).toHaveCount(0);
  // 新回合完成:时间线在 v1 基础上继续推进,超过回退前的 v2。
  const afterResend = await waitForTimelineVersion(request, draftId, beforeResend + 1);
  await waitForPreview(request, draftId);

  const branched = await pollMessages(request, draftId, (messages) => {
    const contents = messages.map((message) => message.content);
    return (
      contents.includes("E2E_FULL_MAINLINE 甲") &&
      contents.includes("E2E_FULL_MAINLINE 乙改") &&
      !contents.includes("E2E_FULL_MAINLINE 乙")
    );
  });
  expect(branched.rewound_message_count).toBeGreaterThan(0);

  // 分叉场景:对更早的「甲」再编辑重发,遮蔽其后的一切(含乙改分支)。
  await editAndResend(page, "E2E_FULL_MAINLINE 甲", "E2E_FULL_MAINLINE 甲改");
  await expect(
    page.getByLabel("消息列表").getByText("E2E_FULL_MAINLINE 甲改", { exact: true })
  ).toBeVisible({ timeout: 60_000 });
  await waitForTimelineVersion(request, draftId, afterResend + 1);

  const reforked = await pollMessages(request, draftId, (messages) => {
    const contents = messages.map((message) => message.content);
    return (
      contents.includes("E2E_FULL_MAINLINE 甲改") &&
      !contents.includes("E2E_FULL_MAINLINE 甲") &&
      !contents.includes("E2E_FULL_MAINLINE 乙改")
    );
  });
  expect(reforked.messages.some((message) => message.role === "user")).toBe(true);
});

test("在途回合运行时编辑重发会先取消该回合再回退重发", async ({ page, request }) => {
  const created = await apiPost<DraftMutationResponse>(request, "/api/drafts", {});
  const draftId = created.draft.draft_id;
  await apiPost(request, `/api/drafts/${draftId}/materials/import-local`, {
    paths: [FIXTURE_PATH],
    storage_mode: "reference"
  });
  await waitForMaterial(request, draftId);

  await page.goto(`/#t=${TOKEN}`);
  await page.goto(`/drafts/${draftId}`);
  await expect(page.getByRole("complementary", { name: "剪辑对话" })).toBeVisible();

  await sendMessage(page, "E2E_FULL_MAINLINE 首轮");
  await waitForTimelineVersion(request, draftId, 1);

  // 发一条会一直阻塞直到取消的消息,制造在途回合。
  await page.getByLabel("消息输入").fill("E2E_BLOCK_UNTIL_CANCEL 卡住");
  await page.getByRole("button", { name: "发送" }).click();
  await expect(
    page.getByLabel("消息列表").getByText("E2E_BLOCK_UNTIL_CANCEL 卡住", { exact: true })
  ).toBeVisible();
  // 回合进行中:停止按钮出现。
  await expect(page.getByRole("button", { name: "停止当前任务" })).toBeVisible({ timeout: 30_000 });

  // 在途回合运行时编辑「首轮」并重发:排空屏障取消在途回合,回退重发正常完成。
  await editAndResend(page, "E2E_FULL_MAINLINE 首轮", "E2E_FULL_MAINLINE 首轮改");
  await expect(
    page.getByLabel("消息列表").getByText("E2E_FULL_MAINLINE 首轮改", { exact: true })
  ).toBeVisible({ timeout: 60_000 });
  await expect(page.getByLabel("消息输入")).toBeEnabled({ timeout: 60_000 });

  const settled = await pollMessages(request, draftId, (messages) => {
    const contents = messages.map((message) => message.content);
    return (
      contents.includes("E2E_FULL_MAINLINE 首轮改") &&
      !contents.includes("E2E_FULL_MAINLINE 首轮") &&
      !contents.includes("E2E_BLOCK_UNTIL_CANCEL 卡住")
    );
  });
  expect(settled.rewound_message_count).toBeGreaterThan(0);
});

async function sendMessage(page: Page, content: string): Promise<void> {
  await page.getByLabel("消息输入").fill(content);
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.getByLabel("消息列表").getByText(content, { exact: true })).toBeVisible();
  await expect(page.getByLabel("消息输入")).toBeEnabled({ timeout: 60_000 });
}

async function editAndResend(page: Page, original: string, next: string): Promise<void> {
  const article = page
    .getByLabel("消息列表")
    .locator("article")
    .filter({ hasText: original })
    .last();
  await article.getByRole("button", { name: "编辑并重发" }).click();
  const editor = page.getByRole("textbox", { name: "编辑消息" });
  await editor.fill(next);
  await page.getByRole("button", { name: "重发", exact: true }).click();
}

async function waitForMaterial(request: APIRequestContext, draftId: string): Promise<void> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const materials = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    const fixture = materials.assets.find((asset) => asset.filename === FIXTURE_NAME);
    if (fixture?.ingest_status === "ready" && fixture.usable) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error("resend fixture import did not become ready");
}

async function waitForTimelineVersion(
  request: APIRequestContext,
  draftId: string,
  minimum: number
): Promise<number> {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const draft = await apiGet<DraftResponse>(request, `/api/drafts/${draftId}`);
    const version = draft.draft.timeline_current_version ?? 0;
    if (version >= minimum) {
      return version;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`draft ${draftId} did not reach timeline v${minimum}`);
}

async function waitForPreview(request: APIRequestContext, draftId: string): Promise<void> {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const draft = await apiGet<DraftResponse>(request, `/api/drafts/${draftId}`);
    if (draft.draft.preview_current_id) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`draft ${draftId} did not publish its preview`);
}

async function pollMessages(
  request: APIRequestContext,
  draftId: string,
  predicate: (messages: MessagesResponse["messages"]) => boolean
): Promise<MessagesResponse> {
  const deadline = Date.now() + 30_000;
  let latest: MessagesResponse = { messages: [], rewound_message_count: 0 };
  while (Date.now() < deadline) {
    latest = await apiGet<MessagesResponse>(request, `/api/drafts/${draftId}/messages?limit=200`);
    if (predicate(latest.messages)) {
      return latest;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`messages did not reach expected state: ${JSON.stringify(latest.messages)}`);
}

async function apiGet<T>(request: APIRequestContext, pathName: string): Promise<T> {
  const response = await request.get(`${API_URL}${pathName}`, {
    headers: { Authorization: `Bearer ${TOKEN}` }
  });
  expect(response.ok()).toBe(true);
  return (await response.json()) as T;
}

async function apiPost<T = unknown>(
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
