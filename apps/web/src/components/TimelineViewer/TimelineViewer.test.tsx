import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "./TimelineViewer";
import type { TimelineJson } from "./TimelineViewer";

const waveSurferMock = vi.hoisted(() => ({
  create: vi.fn((..._args: unknown[]) => ({
    on: vi.fn(),
    un: vi.fn(),
    exportPeaks: vi.fn(() => [[]]),
    destroy: vi.fn()
  }))
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
    expect(rects[1]?.getAttribute("stroke")).toBe("var(--color-focus-ring)");
    expect(rects[1]?.getAttribute("stroke-width")).toBe("2");
    expect(rects[2]?.getAttribute("x")).toBe("30");
    expect(rects[2]?.getAttribute("width")).toBe("60");

    fireEvent.click(rects[0] as Element);

    expect(onClipClick).toHaveBeenCalledWith("tc_a");
  });

  it("按 playheadSec 渲染播放头竖线", () => {
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} playheadSec={1.5} />);

    const playhead = screen.getByTestId("timeline-playhead");
    const line = playhead.querySelector("line");

    expect(line?.getAttribute("x1")).toBe("202");
    expect(line?.getAttribute("x2")).toBe("202");
  });

  it("点击空白轨道区换算秒数并触发 onSeek", () => {
    const onSeek = vi.fn();
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} onSeek={onSeek} />);

    fireEvent.click(screen.getByRole("img", { name: "时间线轨道图" }), {
      clientX: 112 + 120
    });

    expect(onSeek).toHaveBeenCalledTimes(1);
    expect(onSeek.mock.calls[0]?.[0]).toBeCloseTo(2);
  });

  it("点击 clip 只触发 onClipClick，不触发 onSeek", () => {
    const onClipClick = vi.fn();
    const onSeek = vi.fn();
    render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={60}
        onClipClick={onClipClick}
        onSeek={onSeek}
      />
    );

    fireEvent.click(screen.getAllByTestId("timeline-clip")[0] as Element);

    expect(onClipClick).toHaveBeenCalledWith("tc_a");
    expect(onSeek).not.toHaveBeenCalled();
  });

  it("有 waveformSrc 时用 wavesurfer 解码 peaks（内嵌波形数据源），卸载时销毁", () => {
    const { unmount } = render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={72}
        waveformSrc="/api/media/preview/prev_1"
      />
    );

    expect(waveSurferMock.create).toHaveBeenCalledTimes(1);
    expect(waveSurferMock.create.mock.calls[0]?.[0]).toMatchObject({
      url: "/api/media/preview/prev_1"
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
