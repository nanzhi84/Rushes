import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type {
  MouseEvent as ReactMouseEvent,
  PointerEvent as ReactPointerEvent,
  ReactElement
} from "react";
import {
  AudioLines,
  Image as ImageIcon,
  Link2,
  Lock,
  Type,
  Unlock,
  Video,
  Volume2,
  VolumeX
} from "lucide-react";
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
  track_type?: string;
  clips?: TimelineClipJson[];
  muted?: boolean;
  solo?: boolean;
  locked?: boolean;
  gain_db?: number;
};

export type TimelineClipJson = {
  timeline_clip_id?: string;
  track_id?: string;
  timeline_start_frame?: number;
  timeline_end_frame?: number;
  asset_id?: string;
  asset_kind?: string;
  clip_id?: string | null;
  role?: string;
  text?: string;
  source_start_frame?: number;
  source_end_frame?: number;
  playback_rate?: number;
  gain_db?: number;
  lock_policy?: string;
  parent_block_id?: string;
  linked?: boolean;
};

export type TimelineEditMode = "select" | "trim" | "blade";
export type TimelineDropMode = "insert" | "overwrite";
export type TimelineTrackStatePatch = {
  muted?: boolean;
  solo?: boolean;
  locked?: boolean;
  gain_db?: number;
};

export type TimelineViewerProps = {
  timeline: TimelineJson;
  pxPerSec?: number;
  playheadSec?: number | null;
  selectedClipId?: string | null;
  onClipClick?: (clipId: string) => void;
  onSeek?: (sec: number) => void;
  waveformSrc?: string | null;
  editMode?: TimelineEditMode;
  dropMode?: TimelineDropMode;
  snapEnabled?: boolean;
  editing?: boolean;
  onSplitClip?: (clipId: string, splitFrame: number) => void;
  onMoveClip?: (
    clipId: string,
    targetTrackId: string,
    targetFrame: number,
    mode: TimelineDropMode
  ) => void;
  onTrimClip?: (clipId: string, edge: "start" | "end", frame: number) => void;
  onTrackStateChange?: (trackId: string, patch: TimelineTrackStatePatch) => void;
};

type TrackKind = "video" | "audio" | "subtitle" | "overlay";

type DrawableClip = {
  clipId: string;
  trackId: string;
  startFrame: number;
  endFrame: number;
  label: string;
  assetId: string | null;
  assetKind: string | null;
  sourceStartFrame: number | null;
  sourceEndFrame: number | null;
  playbackRate: number;
  gainDb: number;
  linked: boolean;
  parentBlockId: string | null;
};

type DrawableTrack = {
  track_id: string;
  track_type: string | null;
  kind: TrackKind;
  clips: DrawableClip[];
  muted: boolean;
  solo: boolean;
  locked: boolean;
  gainDb: number;
};

type ClipDragState = {
  clip: DrawableClip;
  pointerId: number;
  startClientX: number;
  sourceTrackIndex: number;
  targetTrackIndex: number;
  targetFrame: number;
  valid: boolean;
  moved: boolean;
  element: SVGGElement;
};

type TrimDragState = {
  clip: DrawableClip;
  edge: "start" | "end";
  pointerId: number;
  startClientX: number;
  trackIndex: number;
  frame: number;
  element: SVGRectElement;
};

type SnapCandidate = {
  frame: number;
  label: string;
  priority: number;
  clipId?: string;
  parentBlockId?: string | null;
};

type SnapResult = {
  frame: number;
  candidate: SnapCandidate | null;
};

const LABEL_WIDTH = 184;
const HEADER_HEIGHT = 28;
const TRACK_HEIGHT = 50;
const TRACK_GAP = 4;
const CLIP_HEIGHT = 32;
const FILM_TILE_WIDTH = 56;
const TRIM_HANDLE_WIDTH = 8;
const SNAP_THRESHOLD_PX = 8;

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
  waveformSrc = null,
  editMode = "select",
  dropMode = "insert",
  snapEnabled = true,
  editing = false,
  onSplitClip,
  onMoveClip,
  onTrimClip,
  onTrackStateChange
}: TimelineViewerProps): ReactElement {
  const safeFps = timeline.fps > 0 ? timeline.fps : 30;
  const durationSec = Math.max(0, timeline.duration_frames / safeFps);
  const timelineWidth = Math.max(1, durationSec * pxPerSec);
  const tracks = useMemo(() => normalizeTracks(timeline), [timeline]);
  const svgHeight = HEADER_HEIGHT + tracks.length * (TRACK_HEIGHT + TRACK_GAP);
  const ticks = useMemo(() => buildTicks(durationSec, pxPerSec), [durationSec, pxPerSec]);
  const clampedPlayheadSec =
    playheadSec === null ? null : clamp(playheadSec, 0, durationSec);
  const playheadX = clampedPlayheadSec === null ? null : clampedPlayheadSec * pxPerSec;
  const svgRef = useRef<SVGSVGElement | null>(null);
  const snapGuideRef = useRef<SVGGElement | null>(null);
  const snapGuideTextRef = useRef<SVGTextElement | null>(null);
  const dropHighlightRef = useRef<SVGRectElement | null>(null);
  const trimPreviewRef = useRef<SVGRectElement | null>(null);
  const clipDragRef = useRef<ClipDragState | null>(null);
  const trimDragRef = useRef<TrimDragState | null>(null);
  const suppressClipClickRef = useRef(false);

  const sampleCount = Math.min(6000, Math.max(512, Math.round(durationSec * 24)));
  const peaks = useTimelinePeaks(waveformSrc, sampleCount);
  const snapCandidates = useMemo(
    () => buildSnapCandidates(tracks, ticks, clampedPlayheadSec, safeFps, timeline.duration_frames),
    [clampedPlayheadSec, safeFps, ticks, timeline.duration_frames, tracks]
  );

  const hideGuides = useCallback(() => {
    snapGuideRef.current?.setAttribute("visibility", "hidden");
    dropHighlightRef.current?.setAttribute("visibility", "hidden");
  }, []);

  const showSnapGuide = useCallback(
    (candidate: SnapCandidate | null) => {
      const guide = snapGuideRef.current;
      if (!guide || !candidate) {
        guide?.setAttribute("visibility", "hidden");
        return;
      }
      guide.setAttribute("visibility", "visible");
      guide.setAttribute("transform", `translate(${(candidate.frame / safeFps) * pxPerSec} 0)`);
      if (snapGuideTextRef.current) {
        snapGuideTextRef.current.textContent = `${candidate.label} · ${formatFrameTime(candidate.frame, safeFps)}`;
      }
    },
    [pxPerSec, safeFps]
  );

  const showDropHighlight = useCallback(
    (trackIndex: number, valid: boolean) => {
      const highlight = dropHighlightRef.current;
      if (!highlight) {
        return;
      }
      highlight.setAttribute("visibility", "visible");
      highlight.setAttribute("y", String(HEADER_HEIGHT + trackIndex * (TRACK_HEIGHT + TRACK_GAP)));
      highlight.setAttribute("width", String(timelineWidth));
      highlight.setAttribute("stroke", valid ? "var(--color-accent)" : "var(--color-danger)");
      highlight.setAttribute("fill", valid ? "var(--color-accent)" : "var(--color-danger)");
    },
    [timelineWidth]
  );

  const handleSeekClick = useCallback(
    (event: ReactMouseEvent<SVGSVGElement>) => {
      if (!onSeek) {
        return;
      }
      const rect = event.currentTarget.getBoundingClientRect();
      const localX = event.clientX - rect.left;
      onSeek(clamp(localX / pxPerSec, 0, durationSec));
    },
    [durationSec, onSeek, pxPerSec]
  );

  const calculateDragPreview = useCallback(
    (event: ReactPointerEvent<SVGRectElement>, drag: ClipDragState) => {
      const duration = drag.clip.endFrame - drag.clip.startFrame;
      const deltaFrames = Math.round(((event.clientX - drag.startClientX) / pxPerSec) * safeFps);
      const rawStart = clamp(
        drag.clip.startFrame + deltaFrames,
        0,
        Math.max(0, timeline.duration_frames - duration)
      );
      const snapped = snapEnabled
        ? snapMovingClip(
            rawStart,
            duration,
            snapCandidates,
            drag.clip,
            safeFps,
            pxPerSec,
            timeline.duration_frames
          )
        : { frame: rawStart, candidate: null };
      const svg = svgRef.current;
      const svgTop = svg?.getBoundingClientRect().top ?? 0;
      const rawTrackIndex = Math.floor(
        (event.clientY - svgTop - HEADER_HEIGHT) / (TRACK_HEIGHT + TRACK_GAP)
      );
      const targetTrackIndex = clamp(rawTrackIndex, 0, Math.max(0, tracks.length - 1));
      const targetTrack = tracks[targetTrackIndex];
      const sourceTrack = tracks[drag.sourceTrackIndex];
      const valid = Boolean(
        targetTrack &&
          sourceTrack &&
          !targetTrack.locked &&
          isTrackDropCompatible(drag.clip, sourceTrack, targetTrack)
      );
      return {
        targetFrame: snapped.frame,
        candidate: snapped.candidate,
        targetTrackIndex,
        valid
      };
    },
    [pxPerSec, safeFps, snapCandidates, snapEnabled, timeline.duration_frames, tracks]
  );

  const beginClipDrag = useCallback(
    (
      event: ReactPointerEvent<SVGRectElement>,
      clip: DrawableClip,
      track: DrawableTrack,
      trackIndex: number
    ) => {
      if (editing || editMode !== "select" || !onMoveClip || track.locked) {
        return;
      }
      const element = event.currentTarget.closest("[data-clip-group]") as SVGGElement | null;
      if (!element) {
        return;
      }
      clipDragRef.current = {
        clip,
        pointerId: event.pointerId,
        startClientX: event.clientX,
        sourceTrackIndex: trackIndex,
        targetTrackIndex: trackIndex,
        targetFrame: clip.startFrame,
        valid: true,
        moved: false,
        element
      };
      event.currentTarget.setPointerCapture?.(event.pointerId);
    },
    [editMode, editing, onMoveClip]
  );

  const updateClipDrag = useCallback(
    (event: ReactPointerEvent<SVGRectElement>) => {
      const drag = clipDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      const preview = calculateDragPreview(event, drag);
      const deltaX = ((preview.targetFrame - drag.clip.startFrame) / safeFps) * pxPerSec;
      const deltaY =
        (preview.targetTrackIndex - drag.sourceTrackIndex) * (TRACK_HEIGHT + TRACK_GAP);
      drag.element.setAttribute("transform", `translate(${deltaX} ${deltaY})`);
      drag.element.style.opacity = preview.valid ? "0.9" : "0.45";
      drag.targetFrame = preview.targetFrame;
      drag.targetTrackIndex = preview.targetTrackIndex;
      drag.valid = preview.valid;
      drag.moved = drag.moved || Math.abs(event.clientX - drag.startClientX) >= 4 || deltaY !== 0;
      showSnapGuide(preview.candidate);
      showDropHighlight(preview.targetTrackIndex, preview.valid);
    },
    [calculateDragPreview, pxPerSec, safeFps, showDropHighlight, showSnapGuide]
  );

  const finishClipDrag = useCallback(
    (event: ReactPointerEvent<SVGRectElement>) => {
      const drag = clipDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      const preview = calculateDragPreview(event, drag);
      drag.element.removeAttribute("transform");
      drag.element.style.opacity = "";
      clipDragRef.current = null;
      hideGuides();
      if (drag.moved) {
        suppressClipClickRef.current = true;
        const targetTrack = tracks[preview.targetTrackIndex];
        if (preview.valid && targetTrack) {
          onMoveClip?.(drag.clip.clipId, targetTrack.track_id, preview.targetFrame, dropMode);
        }
      }
    },
    [calculateDragPreview, dropMode, hideGuides, onMoveClip, tracks]
  );

  const cancelClipDrag = useCallback(() => {
    const drag = clipDragRef.current;
    if (drag) {
      drag.element.removeAttribute("transform");
      drag.element.style.opacity = "";
    }
    clipDragRef.current = null;
    hideGuides();
  }, [hideGuides]);

  const calculateTrimFrame = useCallback(
    (event: ReactPointerEvent<SVGRectElement>, drag: TrimDragState): SnapResult => {
      const deltaFrames = Math.round(((event.clientX - drag.startClientX) / pxPerSec) * safeFps);
      const raw =
        drag.edge === "start"
          ? clamp(drag.clip.startFrame + Math.max(0, deltaFrames), drag.clip.startFrame, drag.clip.endFrame - 1)
          : clamp(drag.clip.endFrame + Math.min(0, deltaFrames), drag.clip.startFrame + 1, drag.clip.endFrame);
      if (!snapEnabled) {
        return { frame: raw, candidate: null };
      }
      const snapped = snapPoint(raw, snapCandidates, drag.clip, safeFps, pxPerSec);
      return {
        frame:
          drag.edge === "start"
            ? clamp(snapped.frame, drag.clip.startFrame, drag.clip.endFrame - 1)
            : clamp(snapped.frame, drag.clip.startFrame + 1, drag.clip.endFrame),
        candidate: snapped.candidate
      };
    },
    [pxPerSec, safeFps, snapCandidates, snapEnabled]
  );

  const beginTrimDrag = useCallback(
    (
      event: ReactPointerEvent<SVGRectElement>,
      clip: DrawableClip,
      trackIndex: number,
      edge: "start" | "end"
    ) => {
      if (editing || !onTrimClip || tracks[trackIndex]?.locked) {
        return;
      }
      event.stopPropagation();
      trimDragRef.current = {
        clip,
        edge,
        pointerId: event.pointerId,
        startClientX: event.clientX,
        trackIndex,
        frame: edge === "start" ? clip.startFrame : clip.endFrame,
        element: event.currentTarget
      };
      event.currentTarget.setPointerCapture?.(event.pointerId);
    },
    [editing, onTrimClip, tracks]
  );

  const updateTrimDrag = useCallback(
    (event: ReactPointerEvent<SVGRectElement>) => {
      const drag = trimDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      const result = calculateTrimFrame(event, drag);
      drag.frame = result.frame;
      const start = drag.edge === "start" ? result.frame : drag.clip.startFrame;
      const end = drag.edge === "end" ? result.frame : drag.clip.endFrame;
      const preview = trimPreviewRef.current;
      if (preview) {
        preview.setAttribute("visibility", "visible");
        preview.setAttribute("x", String((start / safeFps) * pxPerSec));
        preview.setAttribute(
          "y",
          String(HEADER_HEIGHT + drag.trackIndex * (TRACK_HEIGHT + TRACK_GAP) + (TRACK_HEIGHT - CLIP_HEIGHT) / 2)
        );
        preview.setAttribute("width", String(Math.max(2, ((end - start) / safeFps) * pxPerSec)));
      }
      const origin = drag.edge === "start" ? drag.clip.startFrame : drag.clip.endFrame;
      drag.element.setAttribute(
        "transform",
        `translate(${((result.frame - origin) / safeFps) * pxPerSec} 0)`
      );
      showSnapGuide(result.candidate);
    },
    [calculateTrimFrame, pxPerSec, safeFps, showSnapGuide]
  );

  const finishTrimDrag = useCallback(
    (event: ReactPointerEvent<SVGRectElement>) => {
      const drag = trimDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      const result = calculateTrimFrame(event, drag);
      drag.element.removeAttribute("transform");
      trimPreviewRef.current?.setAttribute("visibility", "hidden");
      snapGuideRef.current?.setAttribute("visibility", "hidden");
      trimDragRef.current = null;
      const original = drag.edge === "start" ? drag.clip.startFrame : drag.clip.endFrame;
      if (result.frame !== original) {
        onTrimClip?.(drag.clip.clipId, drag.edge, result.frame);
      }
    },
    [calculateTrimFrame, onTrimClip]
  );

  const cancelTrimDrag = useCallback(() => {
    const drag = trimDragRef.current;
    drag?.element.removeAttribute("transform");
    trimDragRef.current = null;
    trimPreviewRef.current?.setAttribute("visibility", "hidden");
    snapGuideRef.current?.setAttribute("visibility", "hidden");
  }, []);

  return (
    <div className="overflow-auto" data-testid="timeline-scroll-surface">
      <div
        className="relative flex min-w-full"
        style={{ width: LABEL_WIDTH + timelineWidth, minHeight: svgHeight }}
      >
        <div
          className="sticky left-0 z-20 shrink-0 border-r border-line bg-panel"
          style={{ width: LABEL_WIDTH, height: svgHeight }}
          aria-label="轨道控制"
        >
          <div className="flex h-7 items-center px-2 text-2xs font-semibold uppercase tracking-wide text-fg-faint">
            轨道
          </div>
          {tracks.map((track) => (
            <TrackHeader
              key={track.track_id}
              track={track}
              editing={editing}
              onChange={onTrackStateChange}
            />
          ))}
        </div>

        <svg
          ref={svgRef}
          role="img"
          aria-label="时间线轨道图"
          width={timelineWidth}
          height={svgHeight}
          className="block shrink-0"
          onClick={handleSeekClick}
        >
          <defs>
            <linearGradient id="tl-label-scrim" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0" stopColor="var(--color-preview)" stopOpacity="0" />
              <stop offset="0.5" stopColor="var(--color-preview)" stopOpacity="0" />
              <stop offset="1" stopColor="var(--color-preview)" stopOpacity="0.72" />
            </linearGradient>
          </defs>
          <rect width={timelineWidth} height={svgHeight} fill="var(--color-panel)" />

          <g>
            {ticks.map((tick) => (
              <g key={tick.sec} transform={`translate(${tick.sec * pxPerSec} 0)`}>
                <line
                  y1={tick.major ? HEADER_HEIGHT - 10 : HEADER_HEIGHT - 5}
                  y2={tick.major ? svgHeight : HEADER_HEIGHT}
                  stroke={tick.major ? "var(--color-line)" : "var(--color-line-strong)"}
                  strokeOpacity={tick.major ? 0.95 : 0.65}
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

          <rect
            ref={dropHighlightRef}
            x={0}
            y={HEADER_HEIGHT}
            width={timelineWidth}
            height={TRACK_HEIGHT}
            rx={3}
            fillOpacity={0.08}
            strokeWidth={1.5}
            strokeDasharray="5 3"
            visibility="hidden"
            pointerEvents="none"
          />

          <g transform={`translate(0 ${HEADER_HEIGHT})`}>
            {tracks.map((track, trackIndex) => {
              const trackY = trackIndex * (TRACK_HEIGHT + TRACK_GAP);
              const tone = trackTone(track.track_id);
              return (
                <g key={track.track_id} transform={`translate(0 ${trackY})`}>
                  <rect
                    x={0}
                    y={0}
                    width={timelineWidth}
                    height={TRACK_HEIGHT}
                    rx={3}
                    fill="var(--color-ink)"
                    opacity={track.muted ? 0.66 : 1}
                  />
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
                      <g
                        key={clip.clipId}
                        data-clip-group
                        data-clip-id={clip.clipId}
                        opacity={track.muted ? 0.5 : track.locked ? 0.72 : 1}
                      >
                        <rect
                          data-clip-body
                          x={x}
                          y={clipY}
                          width={width}
                          height={CLIP_HEIGHT}
                          rx={3}
                          fill={tone}
                          fillOpacity={selected ? 0.95 : 0.8}
                          pointerEvents="none"
                        />
                        {thumbUrl ? (
                          <>
                            <clipPath id={`tl-film-${sid}`}>
                              <rect x={x} y={clipY} width={width} height={CLIP_HEIGHT} rx={3} />
                            </clipPath>
                            <g clipPath={`url(#tl-film-${sid})`} pointerEvents="none">
                              {Array.from({ length: tileCount }, (_, tileIndex) => (
                                <image
                                  key={tileIndex}
                                  href={thumbUrl}
                                  x={x + tileIndex * FILM_TILE_WIDTH}
                                  y={clipY}
                                  width={FILM_TILE_WIDTH}
                                  height={CLIP_HEIGHT}
                                  preserveAspectRatio="xMidYMid slice"
                                />
                              ))}
                              {Array.from({ length: Math.max(0, tileCount - 1) }, (_, tileIndex) => (
                                <line
                                  key={`sep-${tileIndex}`}
                                  x1={x + (tileIndex + 1) * FILM_TILE_WIDTH}
                                  x2={x + (tileIndex + 1) * FILM_TILE_WIDTH}
                                  y1={clipY}
                                  y2={clipY + CLIP_HEIGHT}
                                  stroke="var(--color-ink)"
                                  strokeOpacity={0.35}
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
                        {clip.linked && width > 30 ? (
                          <Link2
                            x={x + 6}
                            y={clipY + 8}
                            width={12}
                            height={12}
                            color={thumbUrl ? "var(--color-on-media)" : "var(--color-fg)"}
                            strokeWidth={2}
                            pointerEvents="none"
                            aria-hidden
                          />
                        ) : null}
                        {width > 44 ? (
                          <text
                            x={x + (clip.linked ? 23 : 7)}
                            y={thumbUrl ? clipY + CLIP_HEIGHT - 7 : clipY + CLIP_HEIGHT / 2 + 4}
                            fill={thumbUrl ? "var(--color-on-media)" : "var(--color-fg)"}
                            fontSize={11}
                            fontWeight={600}
                            pointerEvents="none"
                          >
                            {truncateLabel(clip.label, width - (clip.linked ? 30 : 14))}
                          </text>
                        ) : null}
                        <rect
                          data-testid="timeline-clip"
                          data-clip-id={clip.clipId}
                          x={x}
                          y={clipY}
                          width={width}
                          height={CLIP_HEIGHT}
                          rx={3}
                          fill="transparent"
                          stroke={selected ? "var(--color-focus-ring)" : "var(--color-line)"}
                          strokeWidth={selected ? 2 : 1}
                          role="button"
                          tabIndex={0}
                          aria-disabled={editing || track.locked}
                          aria-label={`${trackLabel(track.track_id)}片段 ${clip.label}`}
                          style={{
                            cursor: editing
                              ? "wait"
                              : track.locked
                                ? "not-allowed"
                                : editMode === "blade"
                                  ? "crosshair"
                                  : editMode === "select" && onMoveClip
                                    ? "grab"
                                    : "pointer"
                          }}
                          onPointerDown={(event) => beginClipDrag(event, clip, track, trackIndex)}
                          onPointerMove={updateClipDrag}
                          onPointerUp={finishClipDrag}
                          onPointerCancel={cancelClipDrag}
                          onClick={(event) => {
                            event.stopPropagation();
                            if (suppressClipClickRef.current) {
                              suppressClipClickRef.current = false;
                              return;
                            }
                            if (editing) {
                              return;
                            }
                            if (editMode === "blade" && clip.assetId && onSplitClip && !track.locked) {
                              const rect = event.currentTarget.getBoundingClientRect();
                              const ratio = rect.width > 0 ? (event.clientX - rect.left) / rect.width : 0.5;
                              const raw = clip.startFrame + Math.round(ratio * (clip.endFrame - clip.startFrame));
                              const snapped = snapEnabled
                                ? snapPoint(raw, snapCandidates, clip, safeFps, pxPerSec).frame
                                : raw;
                              onSplitClip(clip.clipId, clamp(snapped, clip.startFrame + 1, clip.endFrame - 1));
                              return;
                            }
                            onClipClick?.(clip.clipId);
                          }}
                          onKeyDown={(event) => {
                            if (event.key === "Enter" || event.key === " ") {
                              event.preventDefault();
                              onClipClick?.(clip.clipId);
                            }
                          }}
                        >
                          <title>{clip.label}</title>
                        </rect>
                        {selected && editMode === "trim" && onTrimClip && !track.locked ? (
                          <>
                            <rect
                              data-testid="timeline-trim-start"
                              x={x - TRIM_HANDLE_WIDTH / 2}
                              y={clipY}
                              width={TRIM_HANDLE_WIDTH}
                              height={CLIP_HEIGHT}
                              rx={2}
                              fill="var(--color-focus-ring)"
                              role="slider"
                              aria-label={`修剪 ${clip.clipId} 入点`}
                              aria-valuemin={clip.startFrame}
                              aria-valuemax={clip.endFrame - 1}
                              aria-valuenow={clip.startFrame}
                              style={{ cursor: editing ? "wait" : "ew-resize" }}
                              onPointerDown={(event) => beginTrimDrag(event, clip, trackIndex, "start")}
                              onPointerMove={updateTrimDrag}
                              onPointerUp={finishTrimDrag}
                              onPointerCancel={cancelTrimDrag}
                            />
                            <rect
                              data-testid="timeline-trim-end"
                              x={x + width - TRIM_HANDLE_WIDTH / 2}
                              y={clipY}
                              width={TRIM_HANDLE_WIDTH}
                              height={CLIP_HEIGHT}
                              rx={2}
                              fill="var(--color-focus-ring)"
                              role="slider"
                              aria-label={`修剪 ${clip.clipId} 出点`}
                              aria-valuemin={clip.startFrame + 1}
                              aria-valuemax={clip.endFrame}
                              aria-valuenow={clip.endFrame}
                              style={{ cursor: editing ? "wait" : "ew-resize" }}
                              onPointerDown={(event) => beginTrimDrag(event, clip, trackIndex, "end")}
                              onPointerMove={updateTrimDrag}
                              onPointerUp={finishTrimDrag}
                              onPointerCancel={cancelTrimDrag}
                            />
                          </>
                        ) : null}
                      </g>
                    );
                  })}
                </g>
              );
            })}
          </g>

          <rect
            ref={trimPreviewRef}
            x={0}
            y={0}
            width={1}
            height={CLIP_HEIGHT}
            rx={3}
            fill="var(--color-accent)"
            fillOpacity={0.28}
            stroke="var(--color-accent)"
            strokeWidth={1.5}
            visibility="hidden"
            pointerEvents="none"
          />

          <g ref={snapGuideRef} visibility="hidden" pointerEvents="none">
            <line
              x1={0}
              x2={0}
              y1={0}
              y2={svgHeight}
              stroke="var(--color-accent)"
              strokeWidth={1.5}
            />
            <rect x={5} y={4} width={126} height={18} rx={3} fill="var(--color-accent)" />
            <text
              ref={snapGuideTextRef}
              x={10}
              y={17}
              fill="white"
              fontSize={10}
              fontWeight={600}
            />
          </g>

          {playheadX !== null ? (
            <g data-testid="timeline-playhead" pointerEvents="none">
              <line
                x1={playheadX}
                x2={playheadX}
                y1={0}
                y2={svgHeight}
                stroke="var(--color-fg)"
                strokeWidth={1.5}
              />
              <rect x={playheadX - 5} y={0} width={10} height={7} rx={2} fill="var(--color-fg)" />
              <path
                d={`M ${playheadX - 5} 7 L ${playheadX + 5} 7 L ${playheadX} 12 Z`}
                fill="var(--color-fg)"
              />
            </g>
          ) : null}
        </svg>
      </div>
    </div>
  );
}

function TrackHeader({
  track,
  editing,
  onChange
}: {
  track: DrawableTrack;
  editing: boolean;
  onChange?: (trackId: string, patch: TimelineTrackStatePatch) => void;
}): ReactElement {
  const TrackIcon = TRACK_ICON[track.kind];
  const audio = track.kind === "audio";
  const [gain, setGain] = useState(track.gainDb);
  useEffect(() => setGain(track.gainDb), [track.gainDb]);
  const commitGain = (): void => {
    if (Math.abs(gain - track.gainDb) >= 0.01) {
      onChange?.(track.track_id, { gain_db: gain });
    }
  };

  return (
    <div
      className={`border-b border-line px-2 ${track.locked ? "bg-active/55" : "bg-panel"}`}
      style={{ height: TRACK_HEIGHT, marginBottom: TRACK_GAP }}
      data-track-header={track.track_id}
    >
      <div className="flex h-7 items-center gap-1">
        <TrackIcon
          size={14}
          color={trackTone(track.track_id)}
          strokeWidth={1.8}
          aria-hidden
        />
        <span className="min-w-0 flex-1 truncate text-[11px] font-semibold text-fg-muted">
          {trackLabel(track.track_id)}
        </span>
        {audio ? (
          <button
            type="button"
            className={`grid size-5 place-items-center rounded-sm text-[9px] font-bold ${
              track.solo ? "bg-accent text-white" : "text-fg-faint hover:bg-hover hover:text-fg"
            }`}
            aria-label={`${trackLabel(track.track_id)}${track.solo ? "取消独奏" : "独奏"}`}
            aria-pressed={track.solo}
            disabled={editing}
            onClick={() => onChange?.(track.track_id, { solo: !track.solo })}
          >
            S
          </button>
        ) : null}
        <button
          type="button"
          className={`grid size-5 place-items-center rounded-sm text-[9px] font-bold ${
            track.muted ? "bg-warn text-ink" : "text-fg-faint hover:bg-hover hover:text-fg"
          }`}
          aria-label={`${trackLabel(track.track_id)}${track.muted ? "取消静音" : "静音"}`}
          aria-pressed={track.muted}
          disabled={editing || track.track_id === "visual_base"}
          onClick={() => onChange?.(track.track_id, { muted: !track.muted })}
        >
          M
        </button>
        <button
          type="button"
          className={`grid size-5 place-items-center rounded-sm ${
            track.locked ? "bg-active text-fg" : "text-fg-faint hover:bg-hover hover:text-fg"
          }`}
          aria-label={`${trackLabel(track.track_id)}${track.locked ? "解锁" : "锁定"}`}
          aria-pressed={track.locked}
          disabled={editing}
          onClick={() => onChange?.(track.track_id, { locked: !track.locked })}
        >
          {track.locked ? <Lock size={11} aria-hidden /> : <Unlock size={11} aria-hidden />}
        </button>
      </div>
      {audio ? (
        <div className="flex h-[18px] items-center gap-1.5" title={`轨道音量 ${gain.toFixed(0)} dB`}>
          {track.muted ? (
            <VolumeX size={11} className="shrink-0 text-fg-faint" aria-hidden />
          ) : (
            <Volume2 size={11} className="shrink-0 text-fg-faint" aria-hidden />
          )}
          <input
            aria-label={`${trackLabel(track.track_id)}轨道音量`}
            className="h-1 min-w-0 flex-1 accent-accent"
            type="range"
            min={-60}
            max={12}
            step={1}
            value={gain}
            disabled={editing}
            onChange={(event) => setGain(Number(event.target.value))}
            onPointerUp={commitGain}
            onBlur={commitGain}
            onKeyUp={(event) => {
              if (event.key.startsWith("Arrow")) {
                commitGain();
              }
            }}
          />
          <span className="w-8 text-right font-mono text-[9px] tabular-nums text-fg-faint">
            {gain.toFixed(0)}dB
          </span>
        </div>
      ) : (
        <div className="flex h-[18px] items-center gap-1 text-[9px] text-fg-faint">
          {track.locked ? <Lock size={10} aria-hidden /> : null}
          {track.muted ? "已静音" : track.locked ? "编辑已锁定" : ""}
        </div>
      )}
    </div>
  );
}

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
        // 解码失败时保留纯色音频块。
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
  if (slice.length < 2) {
    return null;
  }
  const centerY = clipY + CLIP_HEIGHT / 2;
  const half = CLIP_HEIGHT / 2 - 4;
  const top: string[] = [];
  const bottom: string[] = [];
  for (let index = 0; index < slice.length; index += 1) {
    const pointX = x + (index / (slice.length - 1)) * width;
    const amplitude = Math.min(1, slice[index] ?? 0);
    top.push(`${pointX.toFixed(2)} ${(centerY - amplitude * half).toFixed(2)}`);
    bottom.push(`${pointX.toFixed(2)} ${(centerY + amplitude * half).toFixed(2)}`);
  }
  bottom.reverse();
  return `M ${top.join(" L ")} L ${bottom.join(" L ")} Z`;
}

function buildSnapCandidates(
  tracks: DrawableTrack[],
  ticks: Tick[],
  playheadSec: number | null,
  fps: number,
  durationFrames: number
): SnapCandidate[] {
  const candidates: SnapCandidate[] = [
    { frame: 0, label: "时间线起点", priority: 0 },
    { frame: durationFrames, label: "时间线终点", priority: 0 }
  ];
  if (playheadSec !== null) {
    candidates.push({ frame: Math.round(playheadSec * fps), label: "播放头", priority: 0 });
  }
  for (const tick of ticks) {
    const frame = Math.round(tick.sec * fps);
    if (frame >= 0 && frame <= durationFrames) {
      candidates.push({ frame, label: tick.major ? "主刻度" : "辅助刻度", priority: tick.major ? 2 : 3 });
    }
  }
  for (const track of tracks) {
    for (const clip of track.clips) {
      candidates.push(
        {
          frame: clip.startFrame,
          label: "片段入点",
          priority: 1,
          clipId: clip.clipId,
          parentBlockId: clip.parentBlockId
        },
        {
          frame: clip.endFrame,
          label: "片段出点",
          priority: 1,
          clipId: clip.clipId,
          parentBlockId: clip.parentBlockId
        }
      );
    }
  }
  const deduped = new Map<number, SnapCandidate>();
  for (const candidate of candidates) {
    const current = deduped.get(candidate.frame);
    if (!current || candidate.priority < current.priority) {
      deduped.set(candidate.frame, candidate);
    }
  }
  return [...deduped.values()];
}

function snapMovingClip(
  rawStart: number,
  duration: number,
  candidates: SnapCandidate[],
  moving: DrawableClip,
  fps: number,
  pxPerSec: number,
  durationFrames: number
): SnapResult {
  let best: { distancePx: number; delta: number; candidate: SnapCandidate } | null = null;
  for (const candidate of eligibleSnapCandidates(candidates, moving)) {
    for (const anchor of [rawStart, rawStart + duration]) {
      const delta = candidate.frame - anchor;
      const snappedStart = rawStart + delta;
      if (snappedStart < 0 || snappedStart + duration > durationFrames) {
        continue;
      }
      const distancePx = Math.abs((delta / fps) * pxPerSec);
      if (
        distancePx <= SNAP_THRESHOLD_PX &&
        (!best || distancePx < best.distancePx ||
          (distancePx === best.distancePx && candidate.priority < best.candidate.priority))
      ) {
        best = { distancePx, delta, candidate };
      }
    }
  }
  return best
    ? { frame: rawStart + best.delta, candidate: best.candidate }
    : { frame: rawStart, candidate: null };
}

function snapPoint(
  rawFrame: number,
  candidates: SnapCandidate[],
  moving: DrawableClip,
  fps: number,
  pxPerSec: number
): SnapResult {
  let best: { distancePx: number; candidate: SnapCandidate } | null = null;
  for (const candidate of eligibleSnapCandidates(candidates, moving)) {
    const distancePx = Math.abs(((candidate.frame - rawFrame) / fps) * pxPerSec);
    if (
      distancePx <= SNAP_THRESHOLD_PX &&
      (!best || distancePx < best.distancePx ||
        (distancePx === best.distancePx && candidate.priority < best.candidate.priority))
    ) {
      best = { distancePx, candidate };
    }
  }
  return best ? { frame: best.candidate.frame, candidate: best.candidate } : { frame: rawFrame, candidate: null };
}

function eligibleSnapCandidates(
  candidates: SnapCandidate[],
  moving: DrawableClip
): SnapCandidate[] {
  return candidates.filter((candidate) => {
    if (candidate.clipId === moving.clipId) {
      return false;
    }
    return !(
      moving.linked &&
      moving.parentBlockId &&
      candidate.parentBlockId === moving.parentBlockId
    );
  });
}

function isTrackDropCompatible(
  clip: DrawableClip,
  source: DrawableTrack,
  target: DrawableTrack
): boolean {
  if (clip.linked && source.track_id !== target.track_id) {
    return false;
  }
  return trackFamily(source.kind) === trackFamily(target.kind);
}

function trackFamily(kind: TrackKind): "visual" | "audio" | "text" {
  if (kind === "audio") {
    return "audio";
  }
  if (kind === "subtitle") {
    return "text";
  }
  return "visual";
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(Math.max(value, min), max);
}

function sanitizeId(clipId: string): string {
  return clipId.replace(/[^a-zA-Z0-9_-]/g, "-");
}

type Tick = { sec: number; major: boolean };

function buildTicks(durationSec: number, pxPerSec: number): Tick[] {
  const steps = [0.5, 1, 2, 5, 10, 30, 60];
  const majorStep = steps.find((step) => step * pxPerSec >= 72) ?? 60;
  const minorStep = majorStep / (majorStep >= 10 ? 5 : 2);
  const ticks: Tick[] = [];
  for (let sec = 0; sec <= durationSec + 0.0001; sec += minorStep) {
    const rounded = Number(sec.toFixed(4));
    const majorRatio = rounded / majorStep;
    ticks.push({ sec: rounded, major: Math.abs(majorRatio - Math.round(majorRatio)) < 0.0001 });
  }
  return ticks;
}

function formatTick(sec: number): string {
  const minutes = Math.floor(sec / 60);
  const rest = Math.floor(sec % 60);
  return `${String(minutes).padStart(2, "0")}:${String(rest).padStart(2, "0")}`;
}

function formatFrameTime(frame: number, fps: number): string {
  const seconds = Math.max(0, frame / fps);
  const minutes = Math.floor(seconds / 60);
  const rest = seconds - minutes * 60;
  return `${String(minutes).padStart(2, "0")}:${rest.toFixed(2).padStart(5, "0")}`;
}

function truncateLabel(label: string, maxWidth: number): string {
  const maxChars = Math.max(1, Math.floor(maxWidth / 7));
  return label.length > maxChars ? `${label.slice(0, Math.max(1, maxChars - 1))}…` : label;
}

function normalizeTracks(timeline: TimelineJson): DrawableTrack[] {
  return timeline.tracks.map((track) => ({
    track_id: track.track_id,
    track_type: typeof track.track_type === "string" ? track.track_type : null,
    kind: trackKind(track.track_id),
    muted: track.muted === true,
    solo: track.solo === true,
    locked: track.locked === true,
    gainDb: typeof track.gain_db === "number" ? track.gain_db : 0,
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
  const label =
    trackId === "subtitles"
      ? `${clip.text ?? clip.timeline_clip_id}`.trim()
      : `${clip.asset_id ?? clip.timeline_clip_id}`.trim();
  return {
    clipId: clip.timeline_clip_id,
    trackId,
    startFrame: clip.timeline_start_frame,
    endFrame: clip.timeline_end_frame,
    label,
    assetId: typeof clip.asset_id === "string" ? clip.asset_id : null,
    assetKind: typeof clip.asset_kind === "string" ? clip.asset_kind : null,
    sourceStartFrame: typeof clip.source_start_frame === "number" ? clip.source_start_frame : null,
    sourceEndFrame: typeof clip.source_end_frame === "number" ? clip.source_end_frame : null,
    playbackRate:
      typeof clip.playback_rate === "number" && clip.playback_rate > 0 ? clip.playback_rate : 1,
    gainDb: typeof clip.gain_db === "number" ? clip.gain_db : 0,
    linked: clip.linked === true,
    parentBlockId: typeof clip.parent_block_id === "string" ? clip.parent_block_id : null
  };
}

function trackKind(trackId: string): TrackKind {
  if (trackId === "subtitles") {
    return "subtitle";
  }
  if (["voiceover", "original_audio", "bgm", "sfx"].includes(trackId)) {
    return "audio";
  }
  if (trackId === "visual_overlay") {
    return "overlay";
  }
  return "video";
}

function trackLabel(trackId: string): string {
  const labels: Record<string, string> = {
    visual_base: "主视频",
    visual_primary: "主视频",
    visual_overlay: "叠加",
    voiceover: "配音",
    original_audio: "原声",
    bgm: "音乐",
    sfx: "音效",
    subtitles: "字幕"
  };
  return labels[trackId] ?? trackId;
}

function trackTone(trackId: string): string {
  if (trackId === "subtitles") {
    return "var(--color-track-subtitle)";
  }
  if (trackId === "bgm") {
    return "var(--color-track-music)";
  }
  if (["voiceover", "original_audio", "sfx"].includes(trackId)) {
    return "var(--color-track-sfx)";
  }
  return "var(--color-track-video)";
}
