import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createRef } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "./TimelineViewer";
import type { TimelineJson, TimelineViewerHandle } from "./TimelineViewer";

describe("TimelineViewer", () => {
  beforeEach(() => {
    // 默认桩：无 peaks（404）→ useAssetWaveforms 回退纯色块占位；不触真实网络。
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve({ ok: false, status: 404 } as Response))
    );
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
    expect(rects[0]?.getAttribute("x")).toBe("1");
    expect(rects[0]?.getAttribute("width")).toBe("58");
    expect(rects[1]?.getAttribute("x")).toBe("61");
    expect(rects[1]?.getAttribute("width")).toBe("118");
    expect(rects[1]?.getAttribute("stroke")).toBe("var(--color-focus-ring)");
    expect(rects[1]?.getAttribute("stroke-width")).toBe("2.5");
    expect(rects[2]?.getAttribute("x")).toBe("31");
    expect(rects[2]?.getAttribute("width")).toBe("58");

    fireEvent.click(rects[0] as Element);

    expect(onClipClick).toHaveBeenCalledWith("tc_a");
  });

  it("按 playheadSec 渲染播放头竖线", () => {
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} playheadSec={1.5} />);

    const playhead = screen.getByTestId("timeline-playhead");
    const line = playhead.querySelector("line");

    expect(playhead.getAttribute("visibility")).toBe("visible");
    expect(playhead.getAttribute("transform")).toBe("translate(90 0)");
    expect(line?.getAttribute("x1")).toBe("0");
    expect(line?.getAttribute("x2")).toBe("0");
  });

  it("播放时可直接推进播放头 DOM 并自动跟随长时间线", () => {
    const ref = createRef<TimelineViewerHandle>();
    render(<TimelineViewer ref={ref} timeline={timelineFixture()} pxPerSec={60} />);
    const surface = screen.getByTestId("timeline-scroll-surface");
    Object.defineProperty(surface, "clientWidth", { configurable: true, value: 300 });

    act(() => ref.current?.setPlayheadSec(2.5, true));

    expect(screen.getByTestId("timeline-playhead").getAttribute("transform")).toBe(
      "translate(150 0)"
    );
    expect(surface.scrollLeft).toBeGreaterThan(0);
  });

  it("按下空白轨道区立即换算秒数并触发 onSeek", () => {
    const onSeek = vi.fn();
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} onSeek={onSeek} />);

    const timeline = screen.getByRole("img", { name: "时间线轨道图" });
    fireEvent.pointerDown(timeline, {
      button: 0,
      pointerId: 1,
      clientX: 120
    });
    fireEvent.pointerUp(timeline, { pointerId: 1, clientX: 120 });

    expect(onSeek).toHaveBeenCalledTimes(1);
    expect(onSeek.mock.calls[0]?.[0]).toBeCloseTo(2);
  });

  it("点击空白轨道会先清除片段选择", () => {
    const onDeselect = vi.fn();
    render(
      <TimelineViewer
        timeline={timelineFixture()}
        selectedClipId="tc_a"
        onDeselect={onDeselect}
      />
    );

    fireEvent.pointerDown(screen.getByRole("img", { name: "时间线轨道图" }), {
      button: 0,
      pointerId: 3,
      clientX: 210
    });

    expect(onDeselect).toHaveBeenCalledTimes(1);
  });

  it("支持以指针位置为锚点的 Ctrl 或 Command 滚轮连续缩放", () => {
    const onZoomChange = vi.fn();
    render(
      <TimelineViewer
        timeline={timelineFixture()}
        pxPerSec={60}
        onZoomChange={onZoomChange}
      />
    );
    const surface = screen.getByTestId("timeline-scroll-surface");
    Object.defineProperty(surface, "clientWidth", { configurable: true, value: 600 });
    vi.spyOn(surface, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      left: 0,
      top: 0,
      right: 600,
      bottom: 240,
      width: 600,
      height: 240,
      toJSON: () => ({})
    });

    fireEvent.wheel(surface, { ctrlKey: true, deltaY: -120, clientX: 320 });

    expect(onZoomChange).toHaveBeenCalledTimes(1);
    expect(onZoomChange.mock.calls[0]?.[0]).toBeGreaterThan(60);
  });

  it("拖动时逐指针事件更新播放头，并在松开时提交最终时间", () => {
    const onSeek = vi.fn();
    render(<TimelineViewer timeline={timelineFixture()} pxPerSec={60} onSeek={onSeek} />);
    const timeline = screen.getByRole("img", { name: "时间线轨道图" });

    fireEvent.pointerDown(timeline, { button: 0, pointerId: 7, clientX: 30 });
    fireEvent.pointerMove(timeline, { pointerId: 7, clientX: 120 });

    expect(screen.getByTestId("timeline-playhead").getAttribute("transform")).toBe(
      "translate(120 0)"
    );

    fireEvent.pointerUp(timeline, { pointerId: 7, clientX: 180 });
    expect(onSeek).toHaveBeenLastCalledWith(3);
    expect(screen.getByTestId("timeline-playhead").getAttribute("transform")).toBe(
      "translate(180 0)"
    );
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

  it("始终保留主视频、音乐和音效三条核心轨道，隐藏其他空轨", () => {
    const timeline = timelineFixture();
    timeline.tracks.splice(
      1,
      0,
      { track_id: "visual_overlay", clips: [] },
      { track_id: "original_audio", clips: [] },
      { track_id: "voiceover", clips: [] },
      {
        track_id: "bgm",
        clips: [
          {
            timeline_clip_id: "bgm_1",
            track_id: "bgm",
            asset_id: "asset_music",
            asset_kind: "audio",
            timeline_start_frame: 0,
            timeline_end_frame: 90
          }
        ]
      },
      { track_id: "sfx", clips: [] }
    );

    render(<TimelineViewer timeline={timeline} />);

    expect(screen.getByText("主视频")).toBeTruthy();
    expect(screen.getByText("音乐")).toBeTruthy();
    expect(screen.queryByText("叠加")).toBeNull();
    expect(screen.queryByText("原声")).toBeNull();
    expect(screen.queryByText("配音")).toBeNull();
    expect(screen.getByText("音效")).toBeTruthy();
    expect(screen.getByText("V1")).toBeTruthy();
    expect(screen.getByText("A1")).toBeTruthy();
    expect(screen.getByText("A2")).toBeTruthy();
    expect(screen.getByTestId("timeline-track-stack").className).toContain("items-center");
  });

  it("从 BGM 元数据绘制普通拍点、强拍和小节强拍标记", () => {
    const timeline = timelineFixture();
    timeline.tracks.push({
      track_id: "bgm",
      clips: [
        {
          timeline_clip_id: "bgm_beat_grid",
          track_id: "bgm",
          asset_id: "music",
          timeline_start_frame: 0,
          timeline_end_frame: 90,
          effects: [
            {
              kind: "beat_grid",
              beat_frames: [15, 30, 45],
              strong_beat_frames: [30],
              downbeat_frames: [45]
            }
          ]
        }
      ]
    });

    render(<TimelineViewer timeline={timeline} pxPerSec={60} />);

    const markers = screen.getByTestId("timeline-beat-markers");
    expect(markers.querySelectorAll(":scope > g")).toHaveLength(3);
    expect(screen.getByText(/小节强拍/)).toBeTruthy();
  });

  it("后端旧数据缺少音轨节点时也会补齐音乐和音效轨", () => {
    render(<TimelineViewer timeline={timelineFixture()} />);

    expect(screen.getByText("主视频")).toBeTruthy();
    expect(screen.getByText("音乐")).toBeTruthy();
    expect(screen.getByText("音效")).toBeTruthy();
  });

  it("按音频素材从后端读取预计算 min/max 峰值并绘制波形，不在浏览器解码", async () => {
    const peaksBody = {
      version: 1,
      sample_rate_hz: 100,
      duration_sec: 3,
      peaks: Array.from({ length: 300 }, () => [-0.6, 0.6])
    };
    const fetchMock = vi.fn((_input: RequestInfo | URL) =>
      Promise.resolve({ ok: true, json: () => Promise.resolve(peaksBody) } as Response)
    );
    vi.stubGlobal("fetch", fetchMock);
    const timeline = timelineFixture();
    timeline.tracks.push({
      track_id: "bgm",
      clips: [{
        timeline_clip_id: "bgm_wave",
        track_id: "bgm",
        asset_id: "asset_music",
        asset_kind: "audio",
        timeline_start_frame: 0,
        timeline_end_frame: 90,
        source_start_frame: 0,
        source_end_frame: 90
      }]
    });
    const { container } = render(<TimelineViewer timeline={timeline} pxPerSec={72} />);

    // 读的是后端 peaks 端点（不再下载 proxy 解码）。
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain("/api/media/asset_music/peaks");
    // 峰值解析后画出半透明波形 path（fill-opacity 0.5 唯一属于波形）。
    await waitFor(() =>
      expect(container.querySelector('path[fill-opacity="0.5"]')).toBeTruthy()
    );
  });

  it("音频片段显示淡出包络与可拖拽手柄，并按整数帧提交", () => {
    const onClipFadeChange = vi.fn();
    const timeline = timelineFixture();
    timeline.tracks.push({
      track_id: "bgm",
      clips: [{
        timeline_clip_id: "bgm_fade",
        track_id: "bgm",
        asset_id: "asset_music",
        asset_kind: "audio",
        timeline_start_frame: 0,
        timeline_end_frame: 90,
        source_start_frame: 0,
        source_end_frame: 90,
        fade_out_frames: 15
      }]
    });

    render(
      <TimelineViewer
        timeline={timeline}
        pxPerSec={60}
        selectedClipId="bgm_fade"
        onClipFadeChange={onClipFadeChange}
      />
    );

    const handle = screen.getByTestId("timeline-fade-out-handle");
    expect(handle.getAttribute("aria-valuenow")).toBe("15");
    expect(handle.parentElement?.querySelector('path[fill="var(--color-panel)"]')).toBeTruthy();
    fireEvent.pointerDown(handle, { pointerId: 4, clientX: 150 });
    fireEvent.pointerUp(handle, { pointerId: 4, clientX: 130 });

    expect(onClipFadeChange).toHaveBeenCalledWith("bgm_fade", 0, 25);
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
