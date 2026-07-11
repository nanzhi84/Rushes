import { afterEach, describe, expect, it, vi } from "vitest";
import {
  applyTimelinePatch,
  fetchDraftTimeline,
  postPreviewViewed,
  restoreTimelineVersion
} from "./client";

describe("draft api client functions", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("fetchDraftTimeline 使用 draft timeline URL 并带可选 version", async () => {
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

    await fetchDraftTimeline("draft 1", 3);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/drafts/draft%201/timeline?version=3");
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

  it("applyTimelinePatch 使用版本化时间线 patch 路由", async () => {
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

  it("restoreTimelineVersion 使用 restore 路由并提交目标版本", async () => {
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) =>
      jsonResponse({})
    );
    vi.stubGlobal("fetch", fetchMock);

    await restoreTimelineVersion("draft 1", 2);

    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/drafts/draft%201/timeline/restore");
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
    expect(JSON.parse(String(fetchMock.mock.calls[0]?.[1]?.body))).toEqual({ version: 2 });
  });
});

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
}
