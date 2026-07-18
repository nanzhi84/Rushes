import { fireEvent, render } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "../components/TimelineViewer";
import {
  makeStressMessages,
  makeStressTimeline,
  makeStressWaveformPeaks
} from "../test/fixtures/stressDraft";

beforeEach(() => {
  // 音频轨会请求后端 peaks；桩成 404，回退纯色块占位，不触真实网络（与波形节点数无关）。
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve({ ok: false, status: 404 } as Response))
  );
});

function stubSyncRaf(): void {
  vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
    cb(0);
    return 1;
  });
  vi.stubGlobal("cancelAnimationFrame", () => {});
}

function scrollViewportTo(surface: HTMLElement, scrollLeft: number, clientWidth = 1200): void {
  Object.defineProperty(surface, "clientWidth", { configurable: true, value: clientWidth });
  Object.defineProperty(surface, "scrollLeft", { configurable: true, writable: true, value: scrollLeft });
  fireEvent.scroll(surface);
}

describe("压力草稿 fixture 作为前端渲染性能回归基准", () => {
  it("makeStressTimeline 生成 300 clip 的多轨草稿（含 4 条音轨）", () => {
    const timeline = makeStressTimeline();
    const totalClips = timeline.tracks.reduce((sum, track) => sum + (track.clips?.length ?? 0), 0);
    expect(totalClips).toBe(300);

    const trackIds = timeline.tracks.map((track) => track.track_id);
    for (const audio of ["voiceover", "original_audio", "bgm", "sfx"]) {
      expect(trackIds).toContain(audio);
    }
    expect(trackIds).toContain("subtitles");
    expect(timeline.duration_frames).toBeGreaterThan(0);
  });

  it("clipCount 可下调用于更快单测", () => {
    const timeline = makeStressTimeline({ clipCount: 50 });
    const totalClips = timeline.tracks.reduce((sum, track) => sum + (track.clips?.length ?? 0), 0);
    expect(totalClips).toBe(50);
  });

  it("渲染压力草稿：窗口化后 DOM 节点从数千降到低百位", () => {
    stubSyncRaf();
    const { container } = render(<TimelineViewer timeline={makeStressTimeline()} pxPerSec={96} />);
    const surface = container.querySelector<HTMLElement>('[data-testid="timeline-scroll-surface"]');
    expect(surface).not.toBeNull();

    const fullNodeCount = container.querySelectorAll("svg *").length;
    expect(container.querySelectorAll("[data-clip-group]")).toHaveLength(300);
    expect(fullNodeCount).toBeGreaterThan(2000);

    scrollViewportTo(surface as HTMLElement, 4000);

    const windowedNodeCount = container.querySelectorAll("svg *").length;
    // 富轨草稿（7 轨 + bgm 拍点网格）落在数百级，较全量数千下降 >3x；单轨纯净场景更低
    // （见 TimelineViewer.windowing.test.tsx 的 <500 断言）。
    expect(container.querySelectorAll("[data-clip-group]").length).toBeLessThan(120);
    expect(windowedNodeCount).toBeLessThan(800);
    expect(windowedNodeCount).toBeLessThan(fullNodeCount / 3);
  });

  it("makeStressMessages 生成 500 条 user/assistant 交替消息", () => {
    const messages = makeStressMessages();
    expect(messages).toHaveLength(500);
    expect(messages[0]?.role).toBe("user");
    expect(messages[1]?.role).toBe("assistant");
    // assistant 侧含多段长文本，长于 user 侧。
    expect((messages[1]?.text.length ?? 0)).toBeGreaterThan(messages[0]?.text.length ?? 0);
  });

  it("makeStressWaveformPeaks 生成归一化长口播波形", () => {
    const peaks = makeStressWaveformPeaks();
    expect(peaks).toHaveLength(6000);
    expect(Math.min(...peaks)).toBeGreaterThanOrEqual(0);
    expect(Math.max(...peaks)).toBeLessThanOrEqual(1);
  });
});
