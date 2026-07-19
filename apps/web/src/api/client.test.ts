import { afterEach, describe, expect, it, vi } from "vitest";
import {
  api,
  applyTimelinePatch,
  clearDraftConversation,
  fetchDraftTimeline,
  postPreviewViewed
} from "./client";

describe("draft api client functions", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("fetchDraftTimeline 始终读取草稿的当前时间线", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({
        draft_id: "draft/1",
        timeline_version: 3,
        timeline: { fps: 30, duration_frames: 30, tracks: [] },
        summary: "Timeline v3",
        preview_id: "prev_1"
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await fetchDraftTimeline("draft 1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/drafts/draft%201/timeline");
  });

  it("postPreviewViewed 使用 viewed URL 和 POST", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({ draft: {}, event_ids: [1] })
    );
    vi.stubGlobal("fetch", fetchMock);

    await postPreviewViewed("draft 1", "prev/1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/drafts/draft%201/previews/prev%2F1/viewed"
    );
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
  });

  it("clearDraftConversation 调用清空对话路由且不提交删除素材参数", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({
        status: "cleared",
        draft_id: "draft 1",
        message_id: "context_1",
        event_ids: [1],
        preserved: ["assets", "material_understanding", "timeline", "preview"]
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await clearDraftConversation("draft 1");

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/drafts/draft%201/conversation/clear"
    );
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init).toMatchObject({ method: "POST" });
    expect(init?.body).toBeUndefined();
  });

  it("applyTimelinePatch 使用当前时间线 patch 路由", async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
      jsonResponse({
        draft_id: "draft 1",
        timeline_version: 2,
        timeline: { fps: 30, duration_frames: 30, tracks: [] },
        summary: "Timeline v2",
        preview_id: null
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await applyTimelinePatch("draft 1", {
      op: { kind: "split_clip", timeline_clip_id: "clip 1", split_frame: 15 }
    });

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/drafts/draft%201/timeline/patch");
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
  });

  it("trashDrafts 通过集合 DELETE 一次提交所选草稿", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({
        deleted_count: 2,
        deleted_draft_ids: ["draft_1", "draft_2"],
        event_ids: [11, 12]
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.trashDrafts(["draft_1", "draft_2"]);

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/drafts");
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init).toMatchObject({ method: "DELETE" });
    expect(init?.body).toBe(JSON.stringify({ draft_ids: ["draft_1", "draft_2"], confirm: true }));
  });

  it("cancelJob 调用 job 取消端点并提交原因", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({ event_ids: [9], job_id: "job/1", status: "cancelled" })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.cancelJob("job/1", "user_cancelled");

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/jobs/job%2F1/cancel");
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init).toMatchObject({ method: "POST" });
    expect(init?.body).toBe(JSON.stringify({ reason: "user_cancelled" }));
  });

  it("旧记忆回执撤回携带当前值前置条件", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({ deleted_count: 1, deleted_memory_keys: ["pacing"] })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.deleteMemory("pacing", {
      memory_key: "pacing",
      kind: "preference",
      statement: "成片节奏偏快",
      source_draft_id: "draft_1",
      created_at: "2026-07-19T00:00:00.000000000Z",
      last_confirmed_at: "2026-07-19T00:01:00.000000000Z",
      manually_revised_at: ""
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    const headers = init?.headers as Headers;
    expect(init?.method).toBe("DELETE");
    expect(
      JSON.parse(decodeURIComponent(headers.get("X-Rushes-Memory-If-Match") ?? ""))
    ).toEqual({
      kind: "preference",
      statement: "成片节奏偏快",
      source_draft_id: "draft_1",
      created_at: "2026-07-19T00:00:00.000000000Z",
      last_confirmed_at: "2026-07-19T00:01:00.000000000Z",
      manually_revised_at: ""
    });
  });

  it("编辑并重发使用消息专用路由", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({
        draft_id: "draft 1",
        message_id: "msg_new",
        status: "resent",
        restored_timeline_version: 3,
        rewound_message_count: 2
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await api.resendMessage("draft 1", "msg/1", {
      content: "改写内容",
      idempotency_key: "resend-request-1"
    });

    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/drafts/draft%201/messages/msg%2F1/resend"
    );
    const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init).toMatchObject({ method: "POST" });
    expect(init?.body).toBe(JSON.stringify({
      content: "改写内容",
      idempotency_key: "resend-request-1"
    }));
  });

});

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
}
