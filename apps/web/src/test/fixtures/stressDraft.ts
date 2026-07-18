// 压力草稿造数：300 clip（跨主视频/叠加/原声/配音/音乐/音效/字幕 7 条轨，其中 4 条为音轨）
// + 长口播波形 peaks + 500 条对话消息。作为前端渲染性能（#95 F1/F2/F6）的本地回归基准。
//
// 用法一（自动回归）：见 src/perf/stressDraft.perf.test.tsx——渲染本时间线并断言窗口化后
//   DOM 节点落在低百位（vitest，随 `make web` 常态执行）。
// 用法二（手动 profiling）：在一个临时 scratch 组件里
//     import { makeStressTimeline, makeStressMessages } from "@/test/fixtures/stressDraft";
//     <TimelineViewer timeline={makeStressTimeline()} pxPerSec={96} .../>
//   打开 DevTools Performance 录制，配合 src/perf/marks.ts 的 User Timing 区间
//   （timeline:zoom / timeline:seek / timeline:clip-drag / timeline:scroll-window）与
//   src/perf/vitals.ts 的 [web-vitals] console 输出，观察拖拽/缩放/滚动/流式的交互耗时。
//   scratch 组件用完即删，不要提交到路由。

import type { TimelineJson } from "../../components/TimelineViewer";

const FPS = 30;

export type StressMessage = {
  id: string;
  role: "user" | "assistant";
  text: string;
};

type ClipSpec = {
  trackId: string;
  count: number;
  minFrames: number;
  maxFrames: number;
  gapFrames: number;
  video?: boolean;
  audio?: boolean;
  subtitle?: boolean;
};

// 确定性伪随机，保证 fixture 跨运行稳定（回归断言可复现）。
function makeRng(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state * 1664525 + 1013904223) >>> 0;
    return state / 0xffffffff;
  };
}

// 300 = 120 + 40 + 60 + 30 + 5 + 15 + 30
const CLIP_SPECS: ClipSpec[] = [
  { trackId: "visual_base", count: 120, minFrames: 12, maxFrames: 90, gapFrames: 0, video: true },
  { trackId: "visual_overlay", count: 40, minFrames: 20, maxFrames: 70, gapFrames: 8, video: true },
  { trackId: "voiceover", count: 60, minFrames: 10, maxFrames: 45, gapFrames: 3, audio: true },
  { trackId: "original_audio", count: 30, minFrames: 30, maxFrames: 120, gapFrames: 2, audio: true },
  { trackId: "bgm", count: 5, minFrames: 300, maxFrames: 900, gapFrames: 10, audio: true },
  { trackId: "sfx", count: 15, minFrames: 8, maxFrames: 30, gapFrames: 60, audio: true },
  { trackId: "subtitles", count: 30, minFrames: 30, maxFrames: 90, gapFrames: 20, subtitle: true }
];

export type StressTimelineOptions = { clipCount?: number };

/**
 * 生成 300-clip 多轨压力时间线。clipCount 可下调用于更快的单测；默认铺满各轨规格。
 */
export function makeStressTimeline(options: StressTimelineOptions = {}): TimelineJson {
  const rng = makeRng(0x5eed);
  const cap = options.clipCount ?? Number.POSITIVE_INFINITY;
  let remaining = cap;
  let maxEndFrame = 0;

  const tracks = CLIP_SPECS.map((spec) => {
    const take = Math.max(0, Math.min(spec.count, remaining));
    remaining -= take;
    let cursor = Math.round(rng() * 30);
    const clips = Array.from({ length: take }, (_, index) => {
      const span = spec.minFrames + Math.round(rng() * (spec.maxFrames - spec.minFrames));
      const startFrame = cursor;
      const endFrame = startFrame + span;
      cursor = endFrame + spec.gapFrames;
      maxEndFrame = Math.max(maxEndFrame, endFrame);
      const id = `${spec.trackId}_${index}`;
      const clip: Record<string, unknown> = {
        timeline_clip_id: id,
        track_id: spec.trackId,
        timeline_start_frame: startFrame,
        timeline_end_frame: endFrame
      };
      if (spec.subtitle) {
        clip.text = `字幕片段 ${index}`;
      } else {
        clip.asset_id = `asset_${spec.trackId}_${index % 19}`;
        clip.asset_kind = spec.video ? "video" : "audio";
        clip.source_start_frame = 0;
        clip.source_end_frame = span;
      }
      // 配音轨零星带淡入淡出，压一压音频包络渲染路径。
      if (spec.trackId === "voiceover" && index % 7 === 0) {
        clip.fade_in_frames = 4;
        clip.fade_out_frames = 6;
      }
      // BGM 挂 beat_grid，压一压拍点标记渲染路径。
      if (spec.trackId === "bgm") {
        clip.effects = [
          {
            kind: "beat_grid",
            beat_frames: Array.from({ length: 24 }, (_, b) => startFrame + b * 15),
            strong_beat_frames: Array.from({ length: 6 }, (_, b) => startFrame + b * 60),
            downbeat_frames: Array.from({ length: 3 }, (_, b) => startFrame + b * 120)
          }
        ];
      }
      return clip;
    });
    return { track_id: spec.trackId, clips };
  });

  return {
    fps: FPS,
    duration_frames: Math.max(maxEndFrame, FPS),
    tracks: tracks as TimelineJson["tracks"]
  };
}

/**
 * 长口播波形 peaks（默认 6000 点，约 200s @30fps 的密度），值域 [0,1]。
 * 模拟语音包络：静音间隙 + 起伏音节，供手动 harness 注入或 F4 后端 peaks 对拍参考。
 */
export function makeStressWaveformPeaks(count = 6000): number[] {
  const rng = makeRng(0xba5e);
  const peaks = new Array<number>(count);
  for (let i = 0; i < count; i += 1) {
    // 每约 40 点一个音节包络，叠加词内起伏与随机噪声；间隙压到接近静音。
    const syllable = Math.abs(Math.sin(i / 40));
    const gap = i % 220 < 18 ? 0.04 : 1;
    peaks[i] = Math.min(1, syllable * gap * (0.5 + rng() * 0.5));
  }
  return peaks;
}

/**
 * 500 条对话消息：user/assistant 交替，assistant 侧含长文本（模拟流式长回复），
 * 作为聊天列表压力基准（F3 聊天虚拟化的回归数据，本 PR 不改聊天渲染）。
 */
export function makeStressMessages(count = 500): StressMessage[] {
  const rng = makeRng(0xc0ffee);
  return Array.from({ length: count }, (_, index) => {
    const role: StressMessage["role"] = index % 2 === 0 ? "user" : "assistant";
    const paragraphs = role === "assistant" ? 2 + Math.round(rng() * 4) : 1;
    const text = Array.from(
      { length: paragraphs },
      (_, p) => `第 ${index} 条消息第 ${p + 1} 段：${"口播剪辑内容示例。".repeat(3 + Math.round(rng() * 8))}`
    ).join("\n\n");
    return { id: `msg_${index}`, role, text };
  });
}
