import { useEffect, useMemo, useRef } from "react";
import type { CSSProperties, ReactElement } from "react";
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
  selectedClipId?: string | null;
  onClipClick?: (clipId: string) => void;
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
const HEADER_HEIGHT = 32;
const TRACK_HEIGHT = 44;
const TRACK_GAP = 8;
const CLIP_HEIGHT = 24;

export function TimelineViewer({
  timeline,
  pxPerSec = 96,
  selectedClipId = null,
  onClipClick,
  waveformSrc = null
}: TimelineViewerProps): ReactElement {
  const safeFps = timeline.fps > 0 ? timeline.fps : 30;
  const durationSec = Math.max(0, timeline.duration_frames / safeFps);
  const timelineWidth = Math.max(1, durationSec * pxPerSec);
  const tracks = useMemo(() => normalizeTracks(timeline), [timeline]);
  const svgHeight = HEADER_HEIGHT + tracks.length * (TRACK_HEIGHT + TRACK_GAP);
  const ticks = useMemo(
    () => Array.from({ length: Math.ceil(durationSec) + 1 }, (_item, second) => second),
    [durationSec]
  );
  const surfaceWidth = LABEL_WIDTH + timelineWidth;

  return (
    <div className="rounded-lg border border-[#d9dee7] bg-white">
      <div className="border-b border-[#d9dee7] px-3 py-2">
        <h3 className="text-sm font-semibold text-[#17202a]">时间线</h3>
      </div>
      <div className="overflow-x-auto">
        <div className="min-w-full" style={{ width: surfaceWidth }}>
          <svg
            role="img"
            aria-label="时间线轨道图"
            width={surfaceWidth}
            height={svgHeight}
            className="block"
          >
            <rect x={0} y={0} width={surfaceWidth} height={svgHeight} fill="#ffffff" />
            <g transform={`translate(${LABEL_WIDTH} 0)`}>
              {ticks.map((second) => (
                <g key={second} transform={`translate(${second * pxPerSec} 0)`}>
                  <line y1={0} y2={svgHeight} stroke="#e2e8f0" />
                  <text y={18} fill="#64748b" fontSize={11}>
                    {second}s
                  </text>
                </g>
              ))}
            </g>
            <g transform={`translate(0 ${HEADER_HEIGHT})`}>
              {tracks.map((track, index) => {
                const y = index * (TRACK_HEIGHT + TRACK_GAP);
                return (
                  <g key={track.track_id} transform={`translate(0 ${y})`}>
                    <text x={12} y={24} fill="#475569" fontSize={12} fontWeight={600}>
                      {track.track_id}
                    </text>
                    <rect
                      x={LABEL_WIDTH}
                      y={0}
                      width={timelineWidth}
                      height={TRACK_HEIGHT}
                      rx={4}
                      fill="#f8fafc"
                      stroke="#e2e8f0"
                    />
                    <g transform={`translate(${LABEL_WIDTH} 0)`}>
                      {track.clips.map((clip) => {
                        const x = (clip.startFrame / safeFps) * pxPerSec;
                        const width = ((clip.endFrame - clip.startFrame) / safeFps) * pxPerSec;
                        const selected = selectedClipId === clip.clipId;
                        return (
                          <rect
                            key={clip.clipId}
                            data-testid="timeline-clip"
                            data-clip-id={clip.clipId}
                            x={x}
                            y={(TRACK_HEIGHT - CLIP_HEIGHT) / 2}
                            width={width}
                            height={CLIP_HEIGHT}
                            rx={4}
                            fill={clipFill(track.track_id, clip.isSubtitle)}
                            stroke={selected ? "#f97316" : "#334155"}
                            strokeWidth={selected ? 3 : 1}
                            role="button"
                            aria-label={`${track.track_id} ${clip.clipId}`}
                            onClick={() => onClipClick?.(clip.clipId)}
                          >
                            <title>{clip.label}</title>
                          </rect>
                        );
                      })}
                    </g>
                  </g>
                );
              })}
            </g>
          </svg>
          {waveformSrc ? (
            <div
              className="flex border-t border-[#e2e8f0] bg-[#f8fafc]"
              style={{ width: surfaceWidth }}
            >
              <div className="shrink-0 px-3 py-3 text-xs font-semibold text-[#475569]" style={{ width: LABEL_WIDTH }}>
                波形
              </div>
              <TimelineWaveform
                src={waveformSrc}
                pxPerSec={pxPerSec}
                width={timelineWidth}
              />
            </div>
          ) : null}
        </div>
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
      waveColor: "#94a3b8",
      progressColor: "#2563eb"
    });
    return () => {
      wavesurfer.destroy();
    };
  }, [pxPerSec, src]);

  return <div ref={containerRef} data-testid="timeline-waveform" className="h-11" style={style} />;
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
    ? `${clip.timeline_clip_id} ${clip.text ?? ""}`.trim()
    : `${clip.timeline_clip_id} ${clip.asset_id ?? ""}`.trim();
  return {
    clipId: clip.timeline_clip_id,
    startFrame: clip.timeline_start_frame,
    endFrame: clip.timeline_end_frame,
    label,
    isSubtitle
  };
}

function clipFill(trackId: string, isSubtitle: boolean): string {
  if (isSubtitle) {
    return "#fde68a";
  }
  if (trackId === "voiceover" || trackId === "bgm" || trackId === "original_audio") {
    return "#bfdbfe";
  }
  if (trackId === "visual_overlay") {
    return "#bbf7d0";
  }
  return "#c7d2fe";
}
