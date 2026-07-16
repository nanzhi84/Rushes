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

type TimelineResponse = {
  timeline_version: number;
  timeline: {
    tracks: Array<{
      clips?: Array<{
        timeline_clip_id?: string;
        source_start_frame?: number;
        source_end_frame?: number;
      }>;
    }>;
  };
};

type RewindCheckpoint = {
  checkpoint_id: string;
  trigger_kind: "user_message" | "timeline_write" | "restore";
  anchor_message_id: string | null;
  timeline_version: number | null;
  summary: string;
};

type RewindCheckpointsResponse = {
  checkpoints: RewindCheckpoint[];
};

type RewindRestoreResponse = {
  checkpoint_id: string;
  mode: "timeline" | "conversation" | "both";
  timeline_version: number | null;
  rewound_message_count: number;
};

type MessagesResponse = {
  messages: Array<{ content: string }>;
};

const E2E_DIR = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(E2E_DIR, "../..");
const WORKSPACE_DIR =
  process.env.RUSHES_E2E_WORKSPACE ?? path.join(REPO_ROOT, ".playwright-workspace");
const FIXTURE_NAME = "path3-fixture.mp4";
const FIXTURE_PATH = path.join(WORKSPACE_DIR, "fixtures", FIXTURE_NAME);
const API_URL = `http://127.0.0.1:${process.env.RUSHES_E2E_API_PORT ?? "18001"}`;
const TOKEN = "e2e-token";

test("Rewind 面板依次执行仅时间线、仅对话和两者恢复", async ({ page, request }) => {
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
  await sendMessage(page, "E2E_FULL_MAINLINE");
  const timelineV1 = await waitForTimelineVersion(request, draftId, 1);
  await waitForPreview(request, draftId);
  const preview = page
    .getByLabel("Diffusion Studio 代理预览")
    .or(page.getByRole("region", { name: "Video Player" }));
  await expect(preview).toBeVisible({ timeout: 60_000 });

  await sendMessage(page, "第二轮方向");
  const clip = timelineV1.timeline.tracks.flatMap((track) => track.clips ?? [])[0];
  expect(clip?.timeline_clip_id).toBeTruthy();
  const sourceStart = clip?.source_start_frame ?? 0;
  const sourceEnd = clip?.source_end_frame ?? 0;
  expect(sourceEnd - sourceStart).toBeGreaterThan(1);
  const trimmedStart = sourceStart + 1;
  const timelineV2 = await apiPost<TimelineResponse>(
    request,
    `/api/drafts/${draftId}/timeline/patch`,
    {
      op: {
        kind: "trim_clip",
        timeline_clip_id: clip?.timeline_clip_id,
        source_start_frame: trimmedStart,
        source_end_frame: sourceEnd
      }
    }
  );
  expect(timelineV2.timeline_version).toBeGreaterThan(timelineV1.timeline_version);

  const initialCheckpoints = await waitForCheckpoints(request, draftId, (rows) => {
    const timelineOne = rows.find(
      (row) => row.trigger_kind === "timeline_write" && row.timeline_version === timelineV1.timeline_version
    );
    const timelineTwo = rows.find(
      (row) => row.trigger_kind === "timeline_write" && row.timeline_version === timelineV2.timeline_version
    );
    const firstMessage = rows.find(
      (row) => row.trigger_kind === "user_message" && row.summary === "E2E_FULL_MAINLINE"
    );
    return Boolean(timelineOne && timelineTwo && firstMessage && timelineOne.summary.startsWith("工具批次"));
  });
  const timelineOneCheckpoint = requireCheckpoint(
    initialCheckpoints,
    (row) => row.trigger_kind === "timeline_write" && row.timeline_version === timelineV1.timeline_version
  );
  const timelineTwoCheckpoint = requireCheckpoint(
    initialCheckpoints,
    (row) => row.trigger_kind === "timeline_write" && row.timeline_version === timelineV2.timeline_version
  );
  const firstMessageCheckpoint = requireCheckpoint(
    initialCheckpoints,
    (row) => row.trigger_kind === "user_message" && row.summary === "E2E_FULL_MAINLINE"
  );

  await openRewindPanel(page);
  await selectCheckpoint(page, timelineOneCheckpoint);
  const timelineOnly = await restoreFromPanel(page, "仅时间线");
  expect(timelineOnly.mode).toBe("timeline");
  expect(timelineOnly.timeline_version).toBeGreaterThan(timelineV2.timeline_version);
  expect(timelineOnly.rewound_message_count).toBe(0);
  const messageList = page.getByLabel("消息列表");
  await expect(messageList.getByText("第二轮方向", { exact: true })).toBeVisible();

  await selectCheckpoint(page, firstMessageCheckpoint);
  const conversationOnly = await restoreFromPanel(page, "仅对话");
  expect(conversationOnly.mode).toBe("conversation");
  expect(conversationOnly.timeline_version).toBe(timelineOnly.timeline_version);
  expect(conversationOnly.rewound_message_count).toBeGreaterThan(0);
  await expect(page.getByText(/已回退并折叠 \d+ 条历史消息/)).toBeVisible();
  await expect(messageList.getByText("第二轮方向", { exact: true })).toHaveCount(0);

  await sendMessage(page, "新分支方向");
  const branchedCheckpoints = await waitForCheckpoints(request, draftId, (rows) =>
    rows.some((row) => row.trigger_kind === "user_message" && row.summary === "新分支方向")
  );
  const newBranchCheckpoint = requireCheckpoint(
    branchedCheckpoints,
    (row) => row.trigger_kind === "user_message" && row.summary === "新分支方向"
  );
  await apiPost<RewindRestoreResponse>(request, `/api/drafts/${draftId}/rewind`, {
    checkpoint_id: newBranchCheckpoint.checkpoint_id,
    idempotency_key: `e2e-new-branch-${draftId}`,
    mode: "conversation"
  });
  const branchedMessages = await apiGet<MessagesResponse>(request, `/api/drafts/${draftId}/messages?limit=200`);
  expect(branchedMessages.messages.map((message) => message.content)).toContain("E2E_FULL_MAINLINE");
  expect(branchedMessages.messages.map((message) => message.content)).toContain("新分支方向");
  expect(branchedMessages.messages.map((message) => message.content)).not.toContain("第二轮方向");

  await selectCheckpoint(page, timelineTwoCheckpoint);
  const both = await restoreFromPanel(page, "时间线和对话");
  expect(both.mode).toBe("both");
  expect(both.timeline_version).toBeGreaterThan(timelineOnly.timeline_version ?? 0);
  await expect(messageList.getByText("第二轮方向", { exact: true })).toBeVisible();
  await expect(preview).toBeVisible();

  const restoredTimeline = await apiGet<TimelineResponse>(request, `/api/drafts/${draftId}/timeline`);
  const restoredClip = restoredTimeline.timeline.tracks
    .flatMap((track) => track.clips ?? [])
    .find((item) => item.timeline_clip_id === clip?.timeline_clip_id);
  expect(restoredTimeline.timeline_version).toBe(both.timeline_version);
  expect(restoredClip?.source_start_frame).toBe(trimmedStart);
});

async function sendMessage(page: Page, content: string): Promise<void> {
  await page.getByLabel("消息输入").fill(content);
  await page.getByRole("button", { name: "发送" }).click();
  await expect(page.getByLabel("消息列表").getByText(content, { exact: true })).toBeVisible();
  await expect(page.getByLabel("消息输入")).toBeEnabled({ timeout: 60_000 });
}

async function openRewindPanel(page: Page): Promise<void> {
  await page.getByRole("button", { name: "打开回退检查点" }).click();
  await expect(page.getByRole("region", { name: "回退检查点" })).toBeVisible();
}

async function selectCheckpoint(page: Page, checkpoint: RewindCheckpoint): Promise<void> {
  const panel = page.getByRole("region", { name: "回退检查点" });
  const button = panel
    .getByRole("button", { name: `选择检查点 ${checkpoint.summary}`, exact: true })
    .first();
  await expect(button).toBeVisible();
  await button.click();
  await expect(button).toHaveAttribute("aria-pressed", "true");
}

async function restoreFromPanel(
  page: Page,
  label: "仅时间线" | "仅对话" | "时间线和对话"
): Promise<RewindRestoreResponse> {
  const responsePromise = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" && /\/api\/drafts\/[^/]+\/rewind$/.test(response.url())
  );
  await page
    .getByRole("region", { name: "回退检查点" })
    .getByRole("button", { name: label, exact: true })
    .click();
  const response = await responsePromise;
  expect(response.ok()).toBe(true);
  return (await response.json()) as RewindRestoreResponse;
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
  throw new Error("rewind fixture import did not become ready");
}

async function waitForTimelineVersion(
  request: APIRequestContext,
  draftId: string,
  minimum: number
): Promise<TimelineResponse> {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    const draft = await apiGet<DraftResponse>(request, `/api/drafts/${draftId}`);
    if ((draft.draft.timeline_current_version ?? 0) >= minimum) {
      return apiGet<TimelineResponse>(request, `/api/drafts/${draftId}/timeline`);
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
  throw new Error(`draft ${draftId} did not publish its initial preview`);
}

async function waitForCheckpoints(
  request: APIRequestContext,
  draftId: string,
  predicate: (rows: RewindCheckpoint[]) => boolean
): Promise<RewindCheckpoint[]> {
  const deadline = Date.now() + 15_000;
  let latest: RewindCheckpoint[] = [];
  while (Date.now() < deadline) {
    latest = (
      await apiGet<RewindCheckpointsResponse>(
        request,
        `/api/drafts/${draftId}/rewind/checkpoints`
      )
    ).checkpoints;
    if (predicate(latest)) {
      return latest;
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`rewind checkpoints did not reach expected state: ${JSON.stringify(latest)}`);
}

function requireCheckpoint(
  rows: RewindCheckpoint[],
  predicate: (row: RewindCheckpoint) => boolean
): RewindCheckpoint {
  const checkpoint = rows.find(predicate);
  if (!checkpoint) {
    throw new Error(`missing rewind checkpoint: ${JSON.stringify(rows)}`);
  }
  return checkpoint;
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
