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

    expect(line?.getAttribute("x1")).toBe("90");
    expect(line?.getAttribute("x2")).toBe("90");
  });

  it("点击空白轨道区换算秒数并触发 onSeek", () => {
    const onSeek = vi.fn();
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} onSeek={onSeek} />);

    fireEvent.click(screen.getByRole("img", { name: "时间线轨道图" }), {
      clientX: 120
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

  it("刀片模式按点击位置分割素材片段，并应用半秒吸附", () => {
    const onSplitClip = vi.fn();
    render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={60}
        editMode="blade"
        snapEnabled
        onSplitClip={onSplitClip}
      />
    );

    fireEvent.click(screen.getAllByTestId("timeline-clip")[0] as Element);

    expect(onSplitClip).toHaveBeenCalledWith("tc_a", 15);
  });

  it("裁剪模式在任意未锁定的选中片段上显示入点和出点手柄", () => {
    const onTrimClip = vi.fn();
    const { rerender } = render(
      <TimelineViewer
        timeline={timelineFixture()}
        editMode="trim"
        selectedClipId="tc_a"
        onTrimClip={onTrimClip}
      />
    );

    expect(screen.getByTestId("timeline-trim-start")).toBeTruthy();
    expect(screen.getByTestId("timeline-trim-end")).toBeTruthy();

    rerender(
      <TimelineViewer
        timeline={timelineFixture()}
        editMode="trim"
        selectedClipId="sub_1"
        onTrimClip={onTrimClip}
      />
    );
    expect(screen.getByTestId("timeline-trim-start")).toBeTruthy();
    expect(screen.getByTestId("timeline-trim-end")).toBeTruthy();
  });

  it("跨轨拖放会吸附片段边缘，并携带插入或覆盖模式", () => {
    const onMoveClip = vi.fn();
    render(
      <TimelineViewer
        timeline={multitrackFixture()}
        pxPerSec={60}
        snapEnabled
        dropMode="overwrite"
        onMoveClip={onMoveClip}
      />
    );
    const svg = screen.getByRole("img", { name: "时间线轨道图" });
    vi.spyOn(svg, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 240,
      bottom: 190,
      width: 240,
      height: 190,
      toJSON: () => ({})
    });
    const audioClip = screen.getByRole("button", { name: /原声片段/ });

    fireEvent.pointerDown(audioClip, { pointerId: 1, clientX: 0, clientY: 86 });
    // 28 帧的原始位移离 60 帧片段边缘只有 4px，8px 阈值内吸附到 30 帧。
    fireEvent.pointerMove(audioClip, { pointerId: 1, clientX: 56, clientY: 150 });
    fireEvent.pointerUp(audioClip, { pointerId: 1, clientX: 56, clientY: 150 });

    expect(onMoveClip).toHaveBeenCalledWith("audio_1", "voiceover", 30, "overwrite");
  });

  it("轨道头的静音、独奏、锁定和音量都会提交真实轨道状态", () => {
    const onTrackStateChange = vi.fn();
    render(
      <TimelineViewer
        timeline={multitrackFixture()}
        onTrackStateChange={onTrackStateChange}
      />
    );

    fireEvent.click(screen.getByRole("button", { name: "原声静音" }));
    fireEvent.click(screen.getByRole("button", { name: "原声独奏" }));
    fireEvent.click(screen.getByRole("button", { name: "原声锁定" }));
    const gain = screen.getByRole("slider", { name: "原声轨道音量" });
    fireEvent.change(gain, { target: { value: "-8" } });
    fireEvent.pointerUp(gain);

    expect(onTrackStateChange).toHaveBeenCalledWith("original_audio", { muted: true });
    expect(onTrackStateChange).toHaveBeenCalledWith("original_audio", { solo: true });
    expect(onTrackStateChange).toHaveBeenCalledWith("original_audio", { locked: true });
    expect(onTrackStateChange).toHaveBeenCalledWith("original_audio", { gain_db: -8 });
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

function multitrackFixture(): TimelineJson {
  return {
    fps: 30,
    duration_frames: 120,
    tracks: [
      {
        track_id: "visual_base",
        clips: [
          {
            timeline_clip_id: "video_1",
            track_id: "visual_base",
            asset_id: "asset_video",
            asset_kind: "video",
            timeline_start_frame: 0,
            timeline_end_frame: 120
          }
        ]
      },
      {
        track_id: "original_audio",
        clips: [
          {
            timeline_clip_id: "audio_1",
            track_id: "original_audio",
            asset_id: "asset_audio",
            asset_kind: "audio",
            timeline_start_frame: 0,
            timeline_end_frame: 30
          }
        ]
      },
      {
        track_id: "voiceover",
        clips: [
          {
            timeline_clip_id: "voice_1",
            track_id: "voiceover",
            asset_id: "asset_voice",
            asset_kind: "audio",
            timeline_start_frame: 60,
            timeline_end_frame: 90
          }
        ]
      }
    ]
  };
}
