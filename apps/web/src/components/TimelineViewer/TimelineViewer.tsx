import { useCallback, useEffect, useMemo, useRef } from "react";
import type { CSSProperties, MouseEvent as ReactMouseEvent, ReactElement } from "react";
import WaveSurfer from "wavesurfer.js";

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

type DrawableClip = {
  clipId: string;
  startFrame: number;
  endFrame: number;
  label: string;
  isSubtitle: boolean;
};

type DrawableTrack = {
  track_id: string;
  clips: DrawableClip[];
};

const LABEL_WIDTH = 112;
const HEADER_HEIGHT = 28;
const TRACK_HEIGHT = 40;
const TRACK_GAP = 6;
const CLIP_HEIGHT = 30;

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
                    fontSize={10}
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
              return (
                <g key={track.track_id} transform={`translate(0 ${y})`}>
                  <rect
                    x={8}
                    y={(TRACK_HEIGHT - 10) / 2}
                    width={4}
                    height={10}
                    rx={2}
                    fill={tone}
                  />
                  <text
                    x={20}
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
                    rx={4}
                    fill="var(--color-ink)"
                  />
                  <g transform={`translate(${LABEL_WIDTH} 0)`}>
                    {track.clips.map((clip) => {
                      const x = (clip.startFrame / safeFps) * pxPerSec;
                      const width = ((clip.endFrame - clip.startFrame) / safeFps) * pxPerSec;
                      const selected = selectedClipId === clip.clipId;
                      const clipY = (TRACK_HEIGHT - CLIP_HEIGHT) / 2;
                      return (
                        <g key={clip.clipId}>
                          <rect
                            data-testid="timeline-clip"
                            data-clip-id={clip.clipId}
                            x={x}
                            y={clipY}
                            width={Math.max(width, 2)}
                            height={CLIP_HEIGHT}
                            rx={4}
                            fill={tone}
                            fillOpacity={selected ? 1 : 0.75}
                            stroke={selected ? "var(--color-accent)" : "var(--color-ink)"}
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
                          {width > 48 ? (
                            <text
                              x={x + 6}
                              y={clipY + CLIP_HEIGHT / 2 + 4}
                              fill="rgba(0,0,0,0.75)"
                              fontSize={10.5}
                              fontWeight={600}
                              pointerEvents="none"
                            >
                              {truncateLabel(clip.label, width - 12)}
                            </text>
                          ) : null}
                        </g>
                      );
                    })}
                  </g>
                </g>
              );
            })}
          </g>
          {/* 播放头 */}
          {playheadX !== null ? (
            <g data-testid="timeline-playhead" pointerEvents="none">
              <line
                x1={playheadX}
                x2={playheadX}
                y1={0}
                y2={svgHeight}
                stroke="var(--color-accent)"
                strokeWidth={2}
              />
              <path
                d={`M ${playheadX - 5} 0 L ${playheadX + 5} 0 L ${playheadX} 8 Z`}
                fill="var(--color-accent)"
              />
            </g>
          ) : null}
        </svg>
        {waveformSrc ? (
          <div className="flex border-t border-line bg-panel" style={{ width: surfaceWidth }}>
            <div
              className="shrink-0 px-3 py-3 text-xs font-semibold text-fg-muted"
              style={{ width: LABEL_WIDTH }}
            >
              波形
            </div>
            <TimelineWaveform src={waveformSrc} pxPerSec={pxPerSec} width={timelineWidth} />
          </div>
        ) : null}
      </div>
    </div>
  );
}

function TimelineWaveform({
  src,
  pxPerSec,
  width
}: {
  src: string;
  pxPerSec: number;
  width: number;
}): ReactElement {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const style: CSSProperties = { width };

  useEffect(() => {
    const container = containerRef.current;
    if (!container) {
      return;
    }
    const wavesurfer = WaveSurfer.create({
      container,
      url: src,
      minPxPerSec: pxPerSec,
      height: 44,
      interact: false,
      cursorWidth: 0,
      // canvas 不认 CSS 变量，取值对齐令牌：line-strong / accent。
      waveColor: "#3d3d47",
      progressColor: "#ff5c38"
    });
    return () => {
      wavesurfer.destroy();
    };
  }, [pxPerSec, src]);

  return <div ref={containerRef} data-testid="timeline-waveform" className="h-11" style={style} />;
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
    isSubtitle
  };
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
