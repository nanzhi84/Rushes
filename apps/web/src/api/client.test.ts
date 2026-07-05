import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchCaseTimeline, postPreviewViewed } from "./client";

describe("M6 api client functions", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("fetchCaseTimeline 使用 case timeline URL 并带可选 version", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) =>
      jsonResponse({
        case_id: "case/1",
        timeline_version: 3,
        timeline: { fps: 30, duration_frames: 30, tracks: [] },
        summary: "Timeline v3",
        preview_id: "prev_1"
      })
    );
    vi.stubGlobal("fetch", fetchMock);

    await fetchCaseTimeline("project 1", "case/1", 3);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/projects/project%201/cases/case%2F1/timeline?version=3"
    );
  });

  it("postPreviewViewed 使用 viewed URL 和 POST", async () => {
    const fetchMock = vi.fn(async (..._args: unknown[]) => jsonResponse({ case: {}, event_ids: [1] }));
    vi.stubGlobal("fetch", fetchMock);

    await postPreviewViewed("project 1", "case/1", "prev/1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/projects/project%201/cases/case%2F1/previews/prev%2F1/viewed"
    );
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ method: "POST" });
  });
});

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" }
  });
}
