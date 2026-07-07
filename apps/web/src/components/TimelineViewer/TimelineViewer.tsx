import { useCallback, useEffect, useMemo, useState } from "react";
import type { MouseEvent as ReactMouseEvent, ReactElement } from "react";
import { AudioLines, Image as ImageIcon, Type, Video } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import WaveSurfer from "wavesurfer.js";
import { api } from "../../api/client";

export type TimelineJson = {
  fps: number;
  duration_frames: number;
  tracks: TimelineTrackJson[];
};

export type TimelineTrackJson = {
  track_id: string;
  clips?: TimelineClipJson[];
};

export type TimelineClipJson = {
  timeline_clip_id?: string;
  track_id?: string;
  timeline_start_frame?: number;
  timeline_end_frame?: number;
  asset_id?: string;
  clip_id?: string | null;
  role?: string;
  text?: string;
};

export type TimelineViewerProps = {
  timeline: TimelineJson;
  pxPerSec?: number;
  playheadSec?: number | null;
  selectedClipId?: string | null;
  onClipClick?: (clipId: string) => void;
  onSeek?: (sec: number) => void;
  waveformSrc?: string | null;
};

type TrackKind = "video" | "audio" | "subtitle" | "overlay";

type DrawableClip = {
  clipId: string;
  startFrame: number;
  endFrame: number;
  label: string;
  assetId: string | null;
};

type DrawableTrack = {
  track_id: string;
  kind: TrackKind;
  clips: DrawableClip[];
};

const LABEL_WIDTH = 112;
const HEADER_HEIGHT = 28;
const TRACK_HEIGHT = 40;
const TRACK_GAP = 6;
const CLIP_HEIGHT = 30;
const CLIP_RADIUS = 6;
const FILM_TILE_WIDTH = 56; // filmstrip 单帧瓦片宽（无分镜帧端点，重复 poster 平铺）

const TRACK_ICON: Record<TrackKind, LucideIcon> = {
  video: Video,
  audio: AudioLines,
  subtitle: Type,
  overlay: ImageIcon
};

export function TimelineViewer({
  timeline,
  pxPerSec = 96,
  playheadSec = null,
  selectedClipId = null,
  onClipClick,
  onSeek,
  waveformSrc = null
}: TimelineViewerProps): ReactElement {
  const safeFps = timeline.fps > 0 ? timeline.fps : 30;
  const durationSec = Math.max(0, timeline.duration_frames / safeFps);
  const timelineWidth = Math.max(1, durationSec * pxPerSec);
  const tracks = useMemo(() => normalizeTracks(timeline), [timeline]);
  const svgHeight = HEADER_HEIGHT + tracks.length * (TRACK_HEIGHT + TRACK_GAP);
  const ticks = useMemo(() => buildTicks(durationSec, pxPerSec), [durationSec, pxPerSec]);
  const surfaceWidth = LABEL_WIDTH + timelineWidth;
  const clampedPlayheadSec =
    playheadSec === null ? null : Math.min(Math.max(playheadSec, 0), durationSec);
  const playheadX =
    clampedPlayheadSec === null ? null : LABEL_WIDTH + clampedPlayheadSec * pxPerSec;

  // 音频 clip 内嵌波形复用现 peaks 数据源：把成片预览解码成振幅数组，按 clip 时段切片渲染
  // 成 SVG path，取代旧的底部独立 wavesurfer 条。无预览时回落纯色（同缩略图未就绪逻辑）。
  const sampleCount = Math.min(6000, Math.max(512, Math.round(durationSec * 24)));
  const peaks = useTimelinePeaks(waveformSrc, sampleCount);

  const handleSeekClick = useCallback(
    (event: ReactMouseEvent<SVGSVGElement>) => {
      if (!onSeek) {
        return;
      }
      const rect = event.currentTarget.getBoundingClientRect();
      const localX = event.clientX - rect.left - LABEL_WIDTH;
      const sec = Math.min(Math.max(localX / pxPerSec, 0), durationSec);
      onSeek(sec);
    },
    [durationSec, onSeek, pxPerSec]
  );

  return (
    <div className="overflow-x-auto overflow-y-auto">
      <div className="min-w-full" style={{ width: surfaceWidth }}>
        <svg
          role="img"
          aria-label="时间线轨道图"
          width={surfaceWidth}
          height={svgHeight}
          className="block"
          onClick={handleSeekClick}
        >
          <defs>
            {/* 标签压深色渐变底：透明→ink，保证 filmstrip 上的文字可读 */}
            <linearGradient id="tl-label-scrim" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0" stopColor="var(--color-ink)" stopOpacity="0" />
              <stop offset="0.5" stopColor="var(--color-ink)" stopOpacity="0" />
              <stop offset="1" stopColor="var(--color-ink)" stopOpacity="0.82" />
            </linearGradient>
          </defs>
          <rect x={0} y={0} width={surfaceWidth} height={svgHeight} fill="var(--color-panel)" />
          {/* 刻度尺 */}
          <g transform={`translate(${LABEL_WIDTH} 0)`}>
            {ticks.map((tick) => (
              <g key={tick.sec} transform={`translate(${tick.sec * pxPerSec} 0)`}>
                <line
                  y1={tick.major ? HEADER_HEIGHT - 10 : HEADER_HEIGHT - 5}
                  y2={tick.major ? svgHeight : HEADER_HEIGHT}
                  stroke={tick.major ? "var(--color-line)" : "var(--color-line-strong)"}
                />
                {tick.major ? (
                  <text
                    y={14}
                    x={3}
                    fill="var(--color-fg-faint)"
                    className="text-2xs"
                    style={{ fontVariantNumeric: "tabular-nums" }}
                  >
                    {formatTick(tick.sec)}
                  </text>
                ) : null}
              </g>
            ))}
          </g>
          {/* 轨道 */}
          <g transform={`translate(0 ${HEADER_HEIGHT})`}>
            {tracks.map((track, index) => {
              const y = index * (TRACK_HEIGHT + TRACK_GAP);
              const tone = trackTone(track.track_id);
              const TrackIcon = TRACK_ICON[track.kind];
              return (
                <g key={track.track_id} transform={`translate(0 ${y})`}>
                  <TrackIcon
                    x={12}
                    y={(TRACK_HEIGHT - 16) / 2}
                    width={16}
                    height={16}
                    color={tone}
                    strokeWidth={1.75}
                    aria-hidden
                  />
                  <text
                    x={36}
                    y={TRACK_HEIGHT / 2 + 4}
                    fill="var(--color-fg-muted)"
                    fontSize={12}
                    fontWeight={600}
                  >
                    {trackLabel(track.track_id)}
                  </text>
                  <rect
                    x={LABEL_WIDTH}
                    y={0}
                    width={timelineWidth}
                    height={TRACK_HEIGHT}
                    rx={CLIP_RADIUS}
                    fill="var(--color-ink)"
                  />
                  <g transform={`translate(${LABEL_WIDTH} 0)`}>
                    {track.clips.map((clip) => {
                      const x = (clip.startFrame / safeFps) * pxPerSec;
                      const width = Math.max(
                        ((clip.endFrame - clip.startFrame) / safeFps) * pxPerSec,
                        2
                      );
                      const selected = selectedClipId === clip.clipId;
                      const clipY = (TRACK_HEIGHT - CLIP_HEIGHT) / 2;
                      const startSec = clip.startFrame / safeFps;
                      const endSec = clip.endFrame / safeFps;
                      const sid = sanitizeId(clip.clipId);
                      const thumbUrl =
                        (track.kind === "video" || track.kind === "overlay") && clip.assetId
                          ? api.mediaThumbnailUrl(clip.assetId)
                          : null;
                      const wavePath =
                        track.kind === "audio"
                          ? buildWavePath(peaks, durationSec, startSec, endSec, x, clipY, width)
                          : null;
                      const tileCount = thumbUrl
                        ? Math.max(1, Math.ceil(width / FILM_TILE_WIDTH))
                        : 0;
                      return (
                        <g key={clip.clipId}>
                          {/* 底：纯色回落（缩略图/波形未就绪时可见） */}
                          <rect
                            x={x}
                            y={clipY}
                            width={width}
                            height={CLIP_HEIGHT}
                            rx={CLIP_RADIUS}
                            fill={tone}
                            fillOpacity={selected ? 0.95 : 0.8}
                            pointerEvents="none"
                          />
                          {thumbUrl ? (
                            <>
                              <clipPath id={`tl-film-${sid}`}>
                                <rect
                                  x={x}
                                  y={clipY}
                                  width={width}
                                  height={CLIP_HEIGHT}
                                  rx={CLIP_RADIUS}
                                />
                              </clipPath>
                              <g clipPath={`url(#tl-film-${sid})`} pointerEvents="none">
                                {Array.from({ length: tileCount }, (_, k) => (
                                  <image
                                    key={k}
                                    href={thumbUrl}
                                    x={x + k * FILM_TILE_WIDTH}
                                    y={clipY}
                                    width={FILM_TILE_WIDTH}
                                    height={CLIP_HEIGHT}
                                    preserveAspectRatio="xMidYMid slice"
                                  />
                                ))}
                                {Array.from({ length: Math.max(0, tileCount - 1) }, (_, k) => (
                                  <line
                                    key={`sep-${k}`}
                                    x1={x + (k + 1) * FILM_TILE_WIDTH}
                                    x2={x + (k + 1) * FILM_TILE_WIDTH}
                                    y1={clipY}
                                    y2={clipY + CLIP_HEIGHT}
                                    stroke="var(--color-ink)"
                                    strokeOpacity={0.35}
                                    strokeWidth={1}
                                  />
                                ))}
                                <rect
                                  x={x}
                                  y={clipY}
                                  width={width}
                                  height={CLIP_HEIGHT}
                                  fill="url(#tl-label-scrim)"
                                />
                              </g>
                            </>
                          ) : null}
                          {wavePath ? (
                            <path
                              d={wavePath}
                              fill="var(--color-fg)"
                              fillOpacity={0.5}
                              pointerEvents="none"
                            />
                          ) : null}
                          {width > 44 ? (
                            <text
                              x={x + 7}
                              y={thumbUrl ? clipY + CLIP_HEIGHT - 7 : clipY + CLIP_HEIGHT / 2 + 4}
                              fill={thumbUrl ? "var(--color-fg)" : "var(--color-ink)"}
                              fontSize={11}
                              fontWeight={600}
                              pointerEvents="none"
                            >
                              {truncateLabel(clip.label, width - 14)}
                            </text>
                          ) : null}
                          {/* 交互 + 选中描边层（在最上，保证点击与只读命中） */}
                          <rect
                            data-testid="timeline-clip"
                            data-clip-id={clip.clipId}
                            x={x}
                            y={clipY}
                            width={width}
                            height={CLIP_HEIGHT}
                            rx={CLIP_RADIUS}
                            fill="transparent"
                            stroke={selected ? "var(--color-focus-ring)" : "var(--color-line)"}
                            strokeWidth={selected ? 2 : 1}
                            role="button"
                            aria-label={`${track.track_id} ${clip.clipId}`}
                            onClick={(event) => {
                              event.stopPropagation();
                              onClipClick?.(clip.clipId);
                            }}
                          >
                            <title>{clip.label}</title>
                          </rect>
                        </g>
                      );
                    })}
                  </g>
                </g>
              );
            })}
          </g>
          {/* 播放头：accent 细线 + 顶部把手 */}
          {playheadX !== null ? (
            <g data-testid="timeline-playhead" pointerEvents="none">
              <line
                x1={playheadX}
                x2={playheadX}
                y1={0}
                y2={svgHeight}
                stroke="var(--color-accent)"
                strokeWidth={1.5}
              />
              <rect
                x={playheadX - 5}
                y={0}
                width={10}
                height={7}
                rx={2}
                fill="var(--color-accent)"
              />
              <path
                d={`M ${playheadX - 5} 7 L ${playheadX + 5} 7 L ${playheadX} 12 Z`}
                fill="var(--color-accent)"
              />
            </g>
          ) : null}
        </svg>
      </div>
    </div>
  );
}

/** 解码成片预览为单声道振幅数组（复用现 peaks 数据源），供音频 clip 内嵌波形切片。 */
function useTimelinePeaks(src: string | null, sampleCount: number): number[] | null {
  const [peaks, setPeaks] = useState<number[] | null>(null);
  useEffect(() => {
    setPeaks(null);
    if (!src) {
      return;
    }
    const container = document.createElement("div");
    let cancelled = false;
    const wavesurfer = WaveSurfer.create({ container, url: src, height: 0, interact: false });
    const handleReady = (): void => {
      if (cancelled) {
        return;
      }
      try {
        const channels =
          typeof wavesurfer.exportPeaks === "function"
            ? wavesurfer.exportPeaks({ maxLength: sampleCount })
            : [];
        const first = channels[0];
        if (first && first.length > 0) {
          setPeaks(first.map((value) => Math.abs(Number(value))));
        }
      } catch {
        // 解码/导出失败 → 保持 null，音频 clip 回落纯色。
      }
    };
    wavesurfer.on("ready", handleReady);
    wavesurfer.on("decode", handleReady);
    return () => {
      cancelled = true;
      wavesurfer.destroy();
    };
  }, [src, sampleCount]);
  return peaks;
}

/** 把 peaks 按 clip 时段切片，构造居中镜像的填充波形 path（相对 clip 局部坐标）。 */
function buildWavePath(
  peaks: number[] | null,
  durationSec: number,
  startSec: number,
  endSec: number,
  x: number,
  clipY: number,
  width: number
): string | null {
  if (!peaks || peaks.length < 2 || durationSec <= 0 || width <= 0) {
    return null;
  }
  const total = peaks.length;
  const i0 = clamp(Math.floor((startSec / durationSec) * total), 0, total - 1);
  const i1 = clamp(Math.ceil((endSec / durationSec) * total), i0 + 1, total);
  const slice = peaks.slice(i0, i1);
  const count = slice.length;
  if (count < 2) {
    return null;
  }
  const centerY = clipY + CLIP_HEIGHT / 2;
  const half = CLIP_HEIGHT / 2 - 4;
  const top: string[] = [];
  const bottom: string[] = [];
  for (let k = 0; k < count; k += 1) {
    const px = x + (k / (count - 1)) * width;
    const amp = Math.min(1, slice[k] ?? 0);
    top.push(`${px.toFixed(2)} ${(centerY - amp * half).toFixed(2)}`);
    bottom.push(`${px.toFixed(2)} ${(centerY + amp * half).toFixed(2)}`);
  }
  bottom.reverse();
  return `M ${top.join(" L ")} L ${bottom.join(" L ")} Z`;
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

function sanitizeId(clipId: string): string {
  return clipId.replace(/[^a-zA-Z0-9_-]/g, "-");
}

type Tick = { sec: number; major: boolean };

/** 按缩放挑主刻度间隔（目标 ≥72px 一格），细分次刻度。 */
function buildTicks(durationSec: number, pxPerSec: number): Tick[] {
  const steps = [1, 2, 5, 10, 30, 60];
  const majorStep = steps.find((step) => step * pxPerSec >= 72) ?? 60;
  const minorStep = majorStep / (majorStep >= 10 ? 5 : majorStep);
  const ticks: Tick[] = [];
  for (let sec = 0; sec <= Math.ceil(durationSec); sec += minorStep) {
    const rounded = Number(sec.toFixed(4));
    ticks.push({ sec: rounded, major: rounded % majorStep === 0 });
  }
  return ticks;
}

function formatTick(sec: number): string {
  const minutes = Math.floor(sec / 60);
  const rest = Math.floor(sec % 60);
  return `${String(minutes).padStart(2, "0")}:${String(rest).padStart(2, "0")}`;
}

function truncateLabel(label: string, maxWidth: number): string {
  const maxChars = Math.max(1, Math.floor(maxWidth / 7));
  return label.length > maxChars ? `${label.slice(0, maxChars - 1)}…` : label;
}

function normalizeTracks(timeline: TimelineJson): DrawableTrack[] {
  return timeline.tracks.map((track) => ({
    track_id: track.track_id,
    kind: trackKind(track.track_id),
    clips: (track.clips ?? []).flatMap((clip) => {
      const normalized = normalizeClip(track.track_id, clip);
      return normalized ? [normalized] : [];
    })
  }));
}

function normalizeClip(trackId: string, clip: TimelineClipJson): DrawableClip | null {
  if (
    typeof clip.timeline_clip_id !== "string" ||
    typeof clip.timeline_start_frame !== "number" ||
    typeof clip.timeline_end_frame !== "number"
  ) {
    return null;
  }
  const isSubtitle = trackId === "subtitles";
  const label = isSubtitle
    ? `${clip.text ?? clip.timeline_clip_id}`.trim()
    : `${clip.asset_id ?? clip.timeline_clip_id}`.trim();
  return {
    clipId: clip.timeline_clip_id,
    startFrame: clip.timeline_start_frame,
    endFrame: clip.timeline_end_frame,
    label,
    assetId: typeof clip.asset_id === "string" ? clip.asset_id : null
  };
}

/** 轨道 → 类型（决定图标与 clip 渲染方式）。 */
function trackKind(trackId: string): TrackKind {
  if (trackId === "subtitles") {
    return "subtitle";
  }
  if (
    trackId === "voiceover" ||
    trackId === "original_audio" ||
    trackId === "bgm" ||
    trackId === "sfx"
  ) {
    return "audio";
  }
  if (trackId === "visual_overlay") {
    return "overlay";
  }
  return "video";
}

/** 轨道 → 中文标签；未知 id 原样展示。 */
function trackLabel(trackId: string): string {
  const labels: Record<string, string> = {
    visual_primary: "视频",
    visual_overlay: "叠加",
    voiceover: "配音",
    original_audio: "原声",
    bgm: "音乐",
    sfx: "音效",
    subtitles: "字幕"
  };
  return labels[trackId] ?? trackId;
}

/** 轨道 → 令牌色（CSS 变量引用，SVG 可用）。 */
function trackTone(trackId: string): string {
  if (trackId === "subtitles") {
    return "var(--color-track-subtitle)";
  }
  if (trackId === "bgm") {
    return "var(--color-track-music)";
  }
  if (trackId === "voiceover" || trackId === "original_audio" || trackId === "sfx") {
    return "var(--color-track-sfx)";
  }
  return "var(--color-track-video)";
}
