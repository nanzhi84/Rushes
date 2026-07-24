import { describe, expect, it } from "vitest";
import { ApiError } from "../../auth";
import {
  timelinePatchErrorMessage,
  timelinePatchPartialFailure
} from "./error_messages";

describe("timeline patch error details", () => {
  it("解析原子批次的已提交前缀和最新服务端快照", () => {
    const error = new ApiError(400, "API 请求失败：400", {
      detail: {
        reason: "timeline_patch_validation_failed",
        applied_count: 1,
        failed_index: 1,
        latest: {
          draft_id: "draft_1",
          timeline_version: 2,
          timeline: { fps: 30, duration_frames: 60, tracks: [] },
          summary: "v2",
          preview_id: null
        }
      }
    });

    expect(timelinePatchPartialFailure(error)).toMatchObject({
      appliedCount: 1,
      failedIndex: 1,
      latest: { timeline_version: 2 }
    });
    expect(timelinePatchErrorMessage(error)).toBe("timeline_patch_validation_failed");
  });

  it("缺少完整最新快照时不把普通错误误判为部分成功", () => {
    const error = new ApiError(400, "API 请求失败：400", {
      detail: { reason: "timeline_patch_invalid", applied_count: 0, failed_index: 0 }
    });

    expect(timelinePatchPartialFailure(error)).toBeNull();
  });
});
