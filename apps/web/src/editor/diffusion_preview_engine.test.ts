import { describe, expect, it, vi } from "vitest";
import { frameOffsetTime, frameTime } from "./frame_time";
import { previewCoverLayout } from "./preview_layout";
import { resumePreviewClock } from "./preview_clock";
import { timelineRuntimeSignature } from "./preview_timeline_signature";

describe("DiffusionPreviewEngine frame adapter", () => {
  it("始终以整数帧字符串传给 Diffusion Core，不引入第二套持久时间基", () => {
    expect(frameTime(0)).toBe("0f");
    expect(frameTime(12.6)).toBe("13f");
    expect(frameTime(-8)).toBe("0f");
  });

  it("素材中段镜头使用有符号内部偏移，保持时间线起点不被 source range 再次平移", () => {
    const timelineStartFrame = 100;
    const sourceStartFrame = 109;

    expect(frameOffsetTime(timelineStartFrame - sourceStartFrame)).toBe("-9f");
    expect(frameOffsetTime(276 - 102)).toBe("174f");
  });

  it("横屏和竖屏素材都采用 cover 轴，避免左右留下空白", () => {
    expect(previewCoverLayout(2560, 1080, 1920, 1080)).toEqual({ height: "100%" });
    expect(previewCoverLayout(1080, 1920, 1920, 1080)).toEqual({ width: "100%" });
  });

  it("播放前等待浏览器音频时钟恢复，避免按钮进入播放态但时间停在 0", async () => {
    let state = "suspended";
    const resume = vi.fn(async () => {
      state = "running";
    });

    const clock = {
      get state() {
        return state;
      },
      resume
    };

    await resumePreviewClock(clock);
    await resumePreviewClock(clock);

    expect(state).toBe("running");
    expect(resume).toHaveBeenCalledTimes(1);
  });

  it("浏览器不放行音频时钟时快速失败，不让播放按钮永久等待", async () => {
    const clock = {
      state: "suspended",
      resume: vi.fn(() => new Promise<void>(() => undefined))
    };

    await expect(resumePreviewClock(clock, 5)).rejects.toThrow(
      "浏览器未允许启动音频预览时钟"
    );
  });

  it("相同时间线引用变化时运行签名不变，真实参数变化时签名变化", () => {
    const timeline = {
      fps: 30,
      duration_frames: 60,
      tracks: [{
        track_id: "visual_base",
        track_type: "video",
        clips: [{
          timeline_clip_id: "clip_a",
          asset_id: "asset_a",
          timeline_start_frame: 0,
          timeline_end_frame: 60,
          source_start_frame: 10,
          source_end_frame: 70
        }]
      }]
    };
    const clone = structuredClone(timeline);
    const changed = structuredClone(timeline);
    changed.tracks[0].clips[0].source_start_frame = 11;

    expect(timelineRuntimeSignature(clone)).toBe(timelineRuntimeSignature(timeline));
    expect(timelineRuntimeSignature(changed)).not.toBe(timelineRuntimeSignature(timeline));
  });
});
