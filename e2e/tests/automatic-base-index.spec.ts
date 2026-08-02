import { expect, test, type APIRequestContext } from "@playwright/test";
import path from "node:path";
import { fileURLToPath } from "node:url";

type MaterialsResponse = {
  assets: Array<{
    asset_id: string;
    filename: string;
    ingest_status: string;
    understanding_status: string;
    jobs?: Array<{ kind: string; status: string }>;
  }>;
};

type TimelineResponse = {
  edit_lease: { active: boolean };
};

type MessagesResponse = {
  messages: Array<{ message_id: string; role: string }>;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR = process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURES = [
  path.join(WORKSPACE_DIR, "fixtures", "auto-index-a.mp4"),
  path.join(WORKSPACE_DIR, "fixtures", "auto-index-b.mp4")
];
const API_URL = `http://127.0.0.1:${process.env.RUSHES_E2E_API_PORT ?? "18001"}`;
const TOKEN = "e2e-token";

test("视频导入后自动建立基础镜头索引，不占 timeline lease 且不续跑 Agent", async ({
  page,
  request
}) => {
  await page.goto(`/#t=${TOKEN}`);
  await page.getByRole("button", { name: "开始创作", exact: true }).click();
  await expect(page).toHaveURL(/\/drafts\//);
  const draftId = idFromUrl(page.url());
  const assistantRepliesBefore = assistantReplyIds(
    await apiGet<MessagesResponse>(request, `/api/drafts/${draftId}/messages?limit=200`)
  );

  const imported = await apiPost<{ asset_ids: string[] }>(
    request,
    `/api/drafts/${draftId}/materials/import-local`,
    { paths: FIXTURES, storage_mode: "reference" }
  );
  expect(imported.asset_ids).toHaveLength(2);
  await waitForIngest(request, draftId, imported.asset_ids);

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

  const started = await waitForUnderstandingStarted(request, draftId, imported.asset_ids);
  expect(started.assets.filter((asset) => imported.asset_ids.includes(asset.asset_id)))
    .toHaveLength(2);
  expect(
    (await apiGet<TimelineResponse>(request, `/api/drafts/${draftId}/timeline`)).edit_lease.active
  ).toBe(false);

  const ready = await waitForUnderstandingReady(request, draftId, imported.asset_ids);
  const indexed = ready.assets.filter((asset) => imported.asset_ids.includes(asset.asset_id));
  expect(indexed.every((asset) => asset.understanding_status === "ready")).toBe(true);
  expect(
    indexed.every((asset) => asset.jobs?.some((job) => job.kind === "understand" && job.status === "succeeded"))
  ).toBe(true);

  await page.reload();
  await expect(page.getByText("auto-index-a.mp4")).toBeVisible();
  await expect(page.getByText("auto-index-b.mp4")).toBeVisible();
  await expect(page.getByLabel("理解状态：理解中")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "停止当前任务" })).toHaveCount(0);
  expect(
    assistantReplyIds(await apiGet<MessagesResponse>(request, `/api/drafts/${draftId}/messages?limit=200`))
  ).toEqual(assistantRepliesBefore);

  await page.getByLabel("消息输入").fill("E2E_SHOT_SEARCH");
  await page.getByRole("button", { name: "发送" }).click();
  const searchReply = page
    .locator('[data-message-kind="reply"]')
    .filter({ hasText: "E2E_SHOT_SEARCH_OK" });
  await expect(searchReply).toContainText("total=2 returned=2 frozen=2");
  await expect(searchReply).toContainText("snapshot=shot_search_");

  await page.reload();
  await expect(
    page.locator('[data-message-kind="reply"]').filter({ hasText: "E2E_SHOT_SEARCH_OK" })
  ).toContainText("total=2 returned=2 frozen=2");
});

async function waitForUnderstandingStarted(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<MaterialsResponse> {
  return waitForMaterials(request, draftId, (selected) =>
    selected.every((asset) => asset.understanding_status !== "none"), assetIds);
}

async function waitForUnderstandingReady(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<MaterialsResponse> {
  return waitForMaterials(request, draftId, (selected) =>
    selected.every((asset) => asset.understanding_status === "ready"), assetIds);
}

async function waitForMaterials(
  request: APIRequestContext,
  draftId: string,
  predicate: (selected: MaterialsResponse["assets"]) => boolean,
  assetIds: string[]
): Promise<MaterialsResponse> {
  const deadline = Date.now() + 30_000;
  let latest: MaterialsResponse | null = null;
  while (Date.now() < deadline) {
    latest = await apiGet<MaterialsResponse>(request, `/api/drafts/${draftId}/materials`);
    const selected = latest.assets.filter((asset) => assetIds.includes(asset.asset_id));
    const failed = selected.find((asset) => asset.understanding_status === "failed");
    if (failed) {
      throw new Error(`understanding failed: ${JSON.stringify(failed)}`);
    }
    if (selected.length === assetIds.length && predicate(selected)) {
      return latest;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`materials did not reach expected state: ${JSON.stringify(latest)}`);
}

async function waitForIngest(
  request: APIRequestContext,
  draftId: string,
  assetIds: string[]
): Promise<void> {
  await waitForMaterials(request, draftId, (selected) =>
    selected.every((asset) => asset.ingest_status === "ready"), assetIds);
}

function assistantReplyIds(messages: MessagesResponse): string[] {
  return messages.messages
    .filter((message) => message.role === "assistant")
    .map((message) => message.message_id)
    .sort();
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

function idFromUrl(url: string): string {
  const parsed = new URL(url);
  const match = `${parsed.pathname}${parsed.hash}`.match(/\/drafts\/([^/?#]+)/);
  if (!match) {
    throw new Error(`draft id missing from ${url}`);
  }
  return decodeURIComponent(match[1]);
}
