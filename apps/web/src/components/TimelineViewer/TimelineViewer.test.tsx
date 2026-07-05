import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "./TimelineViewer";
import type { TimelineJson } from "./TimelineViewer";

const waveSurferMock = vi.hoisted(() => ({
  create: vi.fn((..._args: unknown[]) => ({ destroy: vi.fn() }))
}));

vi.mock("wavesurfer.js", () => ({
  default: {
    create: waveSurferMock.create
  }
}));

describe("TimelineViewer", () => {
  beforeEach(() => {
    waveSurferMock.create.mockClear();
  });

  it("按帧坐标绘制 clip 矩形并处理点击和选中态", () => {
    const onClipClick = vi.fn();
    render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={60}
        selectedClipId="tc_b"
        onClipClick={onClipClick}
      />
    );

    const rects = screen.getAllByTestId("timeline-clip");
    expect(rects).toHaveLength(3);
    expect(rects[0]?.getAttribute("x")).toBe("0");
    expect(rects[0]?.getAttribute("width")).toBe("60");
    expect(rects[1]?.getAttribute("x")).toBe("60");
    expect(rects[1]?.getAttribute("width")).toBe("120");
    expect(rects[1]?.getAttribute("stroke")).toBe("#f97316");
    expect(rects[1]?.getAttribute("stroke-width")).toBe("3");
    expect(rects[2]?.getAttribute("x")).toBe("30");
    expect(rects[2]?.getAttribute("width")).toBe("60");

    fireEvent.click(rects[0] as Element);

    expect(onClipClick).toHaveBeenCalledWith("tc_a");
  });

  it("有 waveformSrc 时创建并销毁 wavesurfer，且 minPxPerSec 与 pxPerSec 一致", () => {
    const { unmount } = render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={72}
        waveformSrc="/api/media/preview/prev_1"
      />
    );

    expect(waveSurferMock.create).toHaveBeenCalledTimes(1);
    expect(waveSurferMock.create.mock.calls[0]?.[0]).toMatchObject({
      url: "/api/media/preview/prev_1",
      minPxPerSec: 72
    });

    const instance = waveSurferMock.create.mock.results[0]?.value;
    unmount();

    expect(instance.destroy).toHaveBeenCalledTimes(1);
  });
});

function timelineFixture(): TimelineJson {
  return {
    fps: 30,
    duration_frames: 90,
    tracks: [
      {
        track_id: "visual_base",
        clips: [
          {
            timeline_clip_id: "tc_a",
            track_id: "visual_base",
            timeline_start_frame: 0,
            timeline_end_frame: 30,
            asset_id: "asset_a"
          },
          {
            timeline_clip_id: "tc_b",
            track_id: "visual_base",
            timeline_start_frame: 30,
            timeline_end_frame: 90,
            asset_id: "asset_b"
          }
        ]
      },
      {
        track_id: "subtitles",
        clips: [
          {
            timeline_clip_id: "sub_1",
            track_id: "subtitles",
            text: "字幕",
            timeline_start_frame: 15,
            timeline_end_frame: 45
          }
        ]
      }
    ]
  };
}
