import { fireEvent, render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { TimelineViewer } from "./TimelineViewer";
import type { TimelineJson } from "./TimelineViewer";

// 让窗口化测量同步生效：measureViewport 挂在 requestAnimationFrame 上，桩成同步执行，
// 使 fireEvent.scroll 后立即完成一次可视区量测与重渲染，断言无需等待真实帧。
function stubSyncRaf(): void {
  vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
    cb(0);
    return 1;
  });
  vi.stubGlobal("cancelAnimationFrame", () => {});
}

// 在滚动面上桩接布局尺寸并滚到指定位置，触发一次窗口化量测。
function scrollViewportTo(surface: HTMLElement, scrollLeft: number, clientWidth = 1000): void {
  Object.defineProperty(surface, "clientWidth", { configurable: true, value: clientWidth });
  Object.defineProperty(surface, "scrollLeft", { configurable: true, writable: true, value: scrollLeft });
  fireEvent.scroll(surface);
}

// 300 clip 压力草稿：等宽 clip 背靠背铺在主视频轨，便于精确断言窗口化命中集合。
function stressTimeline(clipCount = 300, clipFrames = 20): TimelineJson {
  const clips = Array.from({ length: clipCount }, (_, index) => ({
    timeline_clip_id: `clip_${index}`,
    track_id: "visual_base",
    asset_id: `asset_${index}`,
    asset_kind: "video",
    timeline_start_frame: index * clipFrames,
    timeline_end_frame: (index + 1) * clipFrames
  }));
  return {
    fps: 30,
    duration_frames: clipCount * clipFrames,
    tracks: [{ track_id: "visual_base", clips }]
  };
}

describe("TimelineViewer 视口窗口化与瓦片降级", () => {
  it("未测量到视口尺寸时全量渲染，保持既有行为", () => {
    const { container } = render(<TimelineViewer timeline={stressTimeline()} pxPerSec={60} />);
    // jsdom 下 clientWidth 为 0 → 窗口化退化为全量，300 个 clip 全部在 DOM 中。
    expect(container.querySelectorAll("[data-clip-group]")).toHaveLength(300);
  });

  it("量测到视口后只渲染可视区 ±overscan 命中的 clip，DOM 节点从数千降到低百位", () => {
    stubSyncRaf();
    const { container } = render(<TimelineViewer timeline={stressTimeline()} pxPerSec={60} />);
    const surface = container.querySelector<HTMLElement>('[data-testid="timeline-scroll-surface"]');
    expect(surface).not.toBeNull();

    // 量测前（width 0）全量渲染：300 clip + 全长刻度 → 数千个 SVG 节点。
    const fullNodeCount = container.querySelectorAll("svg *").length;
    expect(container.querySelectorAll("[data-clip-group]")).toHaveLength(300);
    expect(fullNodeCount).toBeGreaterThan(2000);

    // clip 宽 = (20/30)*60 = 40px；视口 1000-184=816px，overscan 512，命中 [-512,1328] 内的 clip。
    scrollViewportTo(surface as HTMLElement, 0);

    const groups = container.querySelectorAll("[data-clip-group]");
    const windowedNodeCount = container.querySelectorAll("svg *").length;
    // 窗口化后 clip 数落在低百位（数十个），整体 SVG 节点从数千降到低百位。
    expect(groups.length).toBeGreaterThan(0);
    expect(groups.length).toBeLessThan(120);
    expect(windowedNodeCount).toBeLessThan(500);
    expect(windowedNodeCount).toBeLessThan(fullNodeCount / 4);

    // 视口起点的 clip 在 DOM 内，远端 clip 被摘除。
    expect(container.querySelector('[data-clip-id="clip_0"]')).not.toBeNull();
    expect(container.querySelector('[data-clip-id="clip_299"]')).toBeNull();
  });

  it("滚动到中段只渲染该区域的 clip", () => {
    stubSyncRaf();
    const { container } = render(<TimelineViewer timeline={stressTimeline()} pxPerSec={60} />);
    const surface = container.querySelector<HTMLElement>('[data-testid="timeline-scroll-surface"]');

    scrollViewportTo(surface as HTMLElement, 6000);

    // scrollLeft 6000 → 命中 clip index ≈134-180；起点 clip_0 应已被摘除。
    expect(container.querySelector('[data-clip-id="clip_0"]')).toBeNull();
    expect(container.querySelector('[data-clip-id="clip_150"]')).not.toBeNull();
  });

  it("选中的 clip 即使滚出视口也始终保留，选中态不丢", () => {
    stubSyncRaf();
    const { container } = render(
      <TimelineViewer timeline={stressTimeline()} pxPerSec={60} selectedClipId="clip_0" />
    );
    const surface = container.querySelector<HTMLElement>('[data-testid="timeline-scroll-surface"]');

    scrollViewportTo(surface as HTMLElement, 6000);

    // clip_0 已在视口外，但因选中而保留，选中描边仍在。
    const selected = container.querySelector('[data-testid="timeline-clip"][data-clip-id="clip_0"]');
    expect(selected).not.toBeNull();
    expect(selected?.getAttribute("stroke")).toBe("var(--color-focus-ring)");
  });

  it("窗口化下点击可视 clip 仍正常触发选择", () => {
    stubSyncRaf();
    const onClipClick = vi.fn();
    const { container } = render(
      <TimelineViewer timeline={stressTimeline()} pxPerSec={60} onClipClick={onClipClick} />
    );
    const surface = container.querySelector<HTMLElement>('[data-testid="timeline-scroll-surface"]');
    scrollViewportTo(surface as HTMLElement, 0);

    const clip = container.querySelector('[data-testid="timeline-clip"][data-clip-id="clip_3"]');
    expect(clip).not.toBeNull();
    fireEvent.click(clip as Element);
    expect(onClipClick).toHaveBeenCalledWith("clip_3");
  });

  it("胶片瓦片降级为单 image + pattern 平铺，长 clip 不再铺数百个 image 节点", () => {
    // 60s 视频 clip @最大缩放：原实现 ceil(width/56)≈342 个 <image>，降级后应为 1 个。
    const timeline: TimelineJson = {
      fps: 30,
      duration_frames: 1800,
      tracks: [
        {
          track_id: "visual_base",
          clips: [
            {
              timeline_clip_id: "wide",
              track_id: "visual_base",
              asset_id: "asset_wide",
              asset_kind: "video",
              timeline_start_frame: 0,
              timeline_end_frame: 1800
            }
          ]
        }
      ]
    };
    const { container } = render(<TimelineViewer timeline={timeline} pxPerSec={320} />);

    const group = container.querySelector('[data-clip-group][data-clip-id="wide"]');
    expect(group).not.toBeNull();
    // 整条 clip 只有 pattern 里的一个 image，且不再使用 clipPath。
    expect(container.querySelectorAll("image")).toHaveLength(1);
    expect(container.querySelector("clipPath")).toBeNull();
    const pattern = container.querySelector('pattern[id^="tl-film-"]');
    expect(pattern).not.toBeNull();
    const film = container.querySelector("[data-clip-film]");
    expect(film?.getAttribute("fill")).toBe(`url(#${pattern?.getAttribute("id")})`);
  });
});
