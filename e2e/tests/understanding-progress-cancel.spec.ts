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

type TimelineResponse = {
  edit_lease: { active: boolean };
};

type MessagesResponse = {
  messages: Array<{
    message_id: string;
    role: string;
    kind: string;
    content: string;
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

test("同 turn 素材理解可全局停止等待，底层结果继续复用且不会续跑 Agent", async ({
  page,
  request
}) => {
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

  // 为 lease 状态提供可查询的 canonical timeline。纯素材理解不应提前占用
  // timeline lease；全局停止后也不能留下阻塞用户编辑的租约。
  const seededTimeline = await apiPost<TimelineResponse>(
    request,
    `/api/drafts/${draftId}/timeline/patch`,
    {
      op: {
        kind: "insert_clip",
        track_id: "visual_base",
        asset_id: imported.asset_ids[0],
        role: "a_roll",
        source_start_frame: 0,
        source_end_frame: 30
      }
    }
  );
  expect(seededTimeline.edit_lease.active).toBe(false);

  await page.reload();
  await expect(page.getByText("understanding-cancel-a.mp4")).toBeVisible();
  await expect(page.getByText("understanding-cancel-b.mp4")).toBeVisible();

  const assistantRepliesBefore = assistantReplyIds(
    await apiGet<MessagesResponse>(request, `/api/drafts/${draftId}/messages?limit=200`)
  );

  await page.getByLabel("消息输入").fill(TRIGGER);
  await page.getByRole("button", { name: "发送" }).click();

  const stopTurn = page.getByRole("button", { name: "停止当前任务" });
  await expect(stopTurn).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText("正在检测镜头", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "取消理解素材" })).toHaveCount(0);
  const understandingAssetId = await waitForUnderstandingStarted(
    request,
    draftId,
    imported.asset_ids
  );

  const timelineWhileAnalyzing = await apiGet<TimelineResponse>(
    request,
    `/api/drafts/${draftId}/timeline`
  );
  expect(timelineWhileAnalyzing.edit_lease.active).toBe(false);

  await stopTurn.click();
  await expect(stopTurn).toHaveCount(0, { timeout: 10_000 });
  await expect(page.getByText("新消息将按发送顺序排队")).toHaveCount(0);

  const input = page.getByLabel("消息输入");
  await expect(input).toBeEnabled();
  await expect(input).toHaveAttribute("placeholder", "描述你想怎样剪辑…");
  await input.fill("停止后仍可继续输入");
  await expect(page.getByRole("button", { name: "发送消息" })).toBeEnabled();
  await input.fill("");

  await expect
    .poll(
      async () =>
        (await apiGet<TimelineResponse>(request, `/api/drafts/${draftId}/timeline`))
          .edit_lease.active,
      { timeout: 10_000 }
    )
    .toBe(false);

  const materials = await waitForUnderstandingReady(request, draftId, understandingAssetId);
  expect(
    materials.assets.find((asset) => asset.asset_id === understandingAssetId)
      ?.understanding_status
  ).toBe("ready");
  expect(materials.assets.some((asset) => asset.understanding_status === "running")).toBe(false);

  await expect(page.getByLabel("理解状态：理解中")).toHaveCount(0);
  await expect
    .poll(
      async () =>
        (
          await apiGet<MessagesResponse>(
            request,
            `/api/drafts/${draftId}/messages?limit=200`
          )
        ).messages.some((message) => message.kind === "turn_cancelled"),
      { timeout: 10_000 }
    )
    .toBe(true);

  // understand 完成只能更新素材事实，不能在已取消 turn 之后再合成 assistant
  // 回复或启动一个新的 Agent turn。
  await assertNoNewAssistantReplies(
    request,
    draftId,
    assistantRepliesBefore,
    3_000
  );
  await expect(stopTurn).toHaveCount(0);
});

async function waitForUnderstandingStarted(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<string> {
  const deadline = Date.now() + 30_000;
  let latest: MaterialsResponse | null = null;
  while (Date.now() < deadline) {
    latest = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    const started = latest.assets.find(
      (asset) =>
        assetIds.includes(asset.asset_id) && asset.understanding_status !== "none"
    );
    if (started) {
      return started.asset_id;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`understanding did not start: ${JSON.stringify(latest)}`);
}

async function waitForUnderstandingReady(
  request: APIRequestContext,
  draftId: string,
  assetId: string
): Promise<MaterialsResponse> {
  const deadline = Date.now() + 30_000;
  let latest: MaterialsResponse | null = null;
  while (Date.now() < deadline) {
    latest = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    const selected = latest.assets.find((asset) => asset.asset_id === assetId);
    if (selected?.understanding_status === "ready") {
      return latest;
    }
    if (selected?.understanding_status === "failed") {
      throw new Error(`understanding failed: ${JSON.stringify(selected)}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`understanding did not finish: ${JSON.stringify(latest)}`);
}

function assistantReplyIds(messages: MessagesResponse): string[] {
  return messages.messages
    .filter((message) => message.role === "assistant")
    .map((message) => message.message_id)
    .sort();
}

async function assertNoNewAssistantReplies(
  request: APIRequestContext,
  draftId: string,
  expectedIds: string[],
  durationMs: number
): Promise<void> {
  const deadline = Date.now() + durationMs;
  while (Date.now() < deadline) {
    const messages = await apiGet<MessagesResponse>(
      request,
      `/api/drafts/${draftId}/messages?limit=200`
    );
    expect(assistantReplyIds(messages)).toEqual(expectedIds);
    expect(
      messages.messages.some((message) => message.content.includes("素材理解已完成。"))
    ).toBe(false);
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
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
