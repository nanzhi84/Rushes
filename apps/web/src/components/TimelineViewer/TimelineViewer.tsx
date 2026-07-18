import {
  forwardRef,
  memo,
  useCallback,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState
} from "react";
import type {
  PointerEvent as ReactPointerEvent,
  ReactElement,
  WheelEvent as ReactWheelEvent
} from "react";
import {
  Link2,
  Lock,
  Unlock,
  Volume2,
  VolumeX
} from "lucide-react";
import WaveSurfer from "wavesurfer.js";
import { api } from "../../api/client";

export type TimelineJson = {
  fps: number;
  duration_frames: number;
  tracks: TimelineTrackJson[];
};

type TimelineTrackJson = {
  track_id: string;
  track_type?: string;
  clips?: TimelineClipJson[];
  muted?: boolean;
  solo?: boolean;
  locked?: boolean;
  gain_db?: number;
};

type TimelineClipJson = {
  timeline_clip_id?: string;
  track_id?: string;
  timeline_start_frame?: number;
  timeline_end_frame?: number;
  asset_id?: string;
  asset_kind?: string;
  role?: string;
  text?: string;
  source_start_frame?: number;
  source_end_frame?: number;
  playback_rate?: number;
  gain_db?: number;
  fade_in_frames?: number;
  fade_out_frames?: number;
  parent_block_id?: string;
  linked?: boolean;
  effects?: Array<Record<string, unknown>>;
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
  onDeselect?: () => void;
  onSeek?: (sec: number) => void;
  onZoomChange?: (pxPerSec: number) => void;
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
  onClipFadeChange?: (clipId: string, fadeInFrames: number, fadeOutFrames: number) => void;
  onTrackStateChange?: (trackId: string, patch: TimelineTrackStatePatch) => void;
};

export type TimelineViewerHandle = {
  setPlayheadSec: (sec: number | null, follow?: boolean) => void;
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
  fadeInFrames: number;
  fadeOutFrames: number;
  linked: boolean;
  parentBlockId: string | null;
  beatFrames: number[];
  strongBeatFrames: number[];
  downbeatFrames: number[];
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

type FadeDragState = {
  clip: DrawableClip;
  edge: "in" | "out";
  pointerId: number;
  startClientX: number;
  element: SVGCircleElement;
};

type AssetWaveform = {
  durationSec: number;
  peaks: number[];
};

type SeekDragState = {
  pointerId: number;
  lastPreviewAt: number;
  lastEmittedSec: number;
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

type BeatMarker = {
  frame: number;
  kind: "beat" | "strong" | "downbeat";
};

const LABEL_WIDTH = 184;
const HEADER_HEIGHT = 28;
const TRACK_HEIGHT = 58;
const TRACK_GAP = 1;
const CLIP_HEIGHT = 42;
const FILM_TILE_WIDTH = 56;
const TRIM_HANDLE_WIDTH = 8;
const CLIP_VISUAL_GAP = 2;
const WAVEFORM_SAMPLE_COUNT = 6000;
const SNAP_THRESHOLD_PX = 8;
const SEEK_PREVIEW_INTERVAL_MS = 50;
const MIN_PX_PER_SEC = 8;
const MAX_PX_PER_SEC = 320;
// 视口窗口化：只渲染滚动可视区 ±overscan 命中的 clip。overscan 必须 ≥ step，
// 保证两次量化更新之间可视区始终有已渲染内容，滚动不露白。step 越小重渲染越频繁，
// overscan 越大预渲染越多；两者按「一屏内低百位 DOM 节点」平衡。
const TIMELINE_WINDOW_OVERSCAN_PX = 512;
const TIMELINE_WINDOW_STEP_PX = 128;

// 时间线是数千节点的 SVG，reconcile 昂贵；用 memo 把它挡在流式对话高频重渲染之外。
// 上层（DraftEditorView）传入的 props 已保持引用稳定：timeline 来自 EditorSession 快照，
// 所有 handler 走 useCallback，playhead 已节流并改用命令式 DOM 更新。默认浅比较即可生效。
export const TimelineViewer = memo(
  forwardRef<TimelineViewerHandle, TimelineViewerProps>(
  function TimelineViewer(
    {
      timeline,
      pxPerSec = 96,
      playheadSec = null,
      selectedClipId = null,
      onClipClick,
      onDeselect,
      onSeek,
      onZoomChange,
      editMode = "select",
      dropMode = "insert",
      snapEnabled = true,
      editing = false,
      onSplitClip,
      onMoveClip,
      onTrimClip,
      onClipFadeChange,
      onTrackStateChange
    },
    forwardedRef
  ): ReactElement {
  const safeFps = timeline.fps > 0 ? timeline.fps : 30;
  const durationSec = Math.max(0, timeline.duration_frames / safeFps);
  const timelineWidth = Math.max(1, durationSec * pxPerSec);
  const tracks = useMemo(() => normalizeTracks(timeline), [timeline]);
  const svgHeight = HEADER_HEIGHT + tracks.length * (TRACK_HEIGHT + TRACK_GAP);
  const ticks = useMemo(() => buildTicks(durationSec, pxPerSec), [durationSec, pxPerSec]);
  const clampedPlayheadSec =
    playheadSec === null ? null : clamp(playheadSec, 0, durationSec);
  const scrollSurfaceRef = useRef<HTMLDivElement | null>(null);
  const playheadRef = useRef<SVGGElement | null>(null);
  const svgRef = useRef<SVGSVGElement | null>(null);
  const snapGuideRef = useRef<SVGGElement | null>(null);
  const snapGuideTextRef = useRef<SVGTextElement | null>(null);
  const dropHighlightRef = useRef<SVGRectElement | null>(null);
  const trimPreviewRef = useRef<SVGRectElement | null>(null);
  const clipDragRef = useRef<ClipDragState | null>(null);
  const trimDragRef = useRef<TrimDragState | null>(null);
  const fadeDragRef = useRef<FadeDragState | null>(null);
  const seekDragRef = useRef<SeekDragState | null>(null);
  const suppressClipClickRef = useRef(false);
  const pendingZoomRef = useRef<{ anchorSec: number; viewportX: number } | null>(null);
  // 滚动可视窗口（SVG 本地坐标，已扣除左侧粘性轨道头）。width<=0 表示尚未测量或非浏览器
  // 环境（如 jsdom 未桩接布局），此时退化为全量渲染，保持既有行为不变。
  const [viewport, setViewport] = useState<{ left: number; width: number }>({ left: 0, width: 0 });
  const scrollRafRef = useRef<number | null>(null);

  const measureViewport = useCallback(() => {
    const scroller = scrollSurfaceRef.current;
    if (!scroller) {
      return;
    }
    const width = Math.max(0, scroller.clientWidth - LABEL_WIDTH);
    // 量化到 step，滚动细粒度抖动不触发重渲染；overscan≥step 兜住量化误差。
    const left =
      Math.floor(Math.max(0, scroller.scrollLeft) / TIMELINE_WINDOW_STEP_PX) *
      TIMELINE_WINDOW_STEP_PX;
    setViewport((current) =>
      current.left === left && current.width === width ? current : { left, width }
    );
  }, []);

  const handleScroll = useCallback(() => {
    if (scrollRafRef.current !== null) {
      return;
    }
    scrollRafRef.current = requestAnimationFrame(() => {
      scrollRafRef.current = null;
      measureViewport();
    });
  }, [measureViewport]);

  useLayoutEffect(() => {
    measureViewport();
    const scroller = scrollSurfaceRef.current;
    if (!scroller || typeof ResizeObserver === "undefined") {
      return;
    }
    const observer = new ResizeObserver(() => measureViewport());
    observer.observe(scroller);
    return () => {
      observer.disconnect();
      if (scrollRafRef.current !== null) {
        cancelAnimationFrame(scrollRafRef.current);
        scrollRafRef.current = null;
      }
    };
  }, [measureViewport]);

  const updatePlayhead = useCallback(
    (sec: number | null, follow = false) => {
      const playhead = playheadRef.current;
      if (!playhead || sec === null || !Number.isFinite(sec)) {
        playhead?.setAttribute("visibility", "hidden");
        return;
      }
      const x = clamp(sec, 0, durationSec) * pxPerSec;
      playhead.setAttribute("visibility", "visible");
      playhead.setAttribute("transform", `translate(${x} 0)`);
      if (!follow) {
        return;
      }
      const scroller = scrollSurfaceRef.current;
      if (!scroller) {
        return;
      }
      const viewportWidth = Math.max(1, scroller.clientWidth - LABEL_WIDTH);
      const visibleX = x - scroller.scrollLeft;
      const followBoundary = viewportWidth * 0.7;
      if (visibleX > followBoundary) {
        scroller.scrollLeft = Math.max(0, x - followBoundary);
      } else if (visibleX < 0) {
        scroller.scrollLeft = Math.max(0, x - viewportWidth * 0.2);
      }
    },
    [durationSec, pxPerSec]
  );

  useImperativeHandle(
    forwardedRef,
    () => ({ setPlayheadSec: updatePlayhead }),
    [updatePlayhead]
  );

  useLayoutEffect(() => {
    updatePlayhead(playheadSec, false);
  }, [playheadSec, updatePlayhead]);

  const waveforms = useAssetWaveforms(tracks, WAVEFORM_SAMPLE_COUNT);
  const snapCandidates = useMemo(
    () => buildSnapCandidates(tracks, ticks, clampedPlayheadSec, safeFps, timeline.duration_frames),
    [clampedPlayheadSec, safeFps, ticks, timeline.duration_frames, tracks]
  );
  // 只把命中可视窗口的 clip 交给渲染；吸附/拖拽计算仍走全量 tracks（snapCandidates 不受影响）。
  // 选中 clip 始终保留，避免选中后滚动到窗口外时手柄/选中态被摘除。
  const visibleClipsByTrack = useMemo(() => {
    const map = new Map<string, DrawableClip[]>();
    const windowing = viewport.width > 0;
    const rangeStart = viewport.left - TIMELINE_WINDOW_OVERSCAN_PX;
    const rangeEnd = viewport.left + viewport.width + TIMELINE_WINDOW_OVERSCAN_PX;
    for (const track of tracks) {
      if (!windowing) {
        map.set(track.track_id, track.clips);
        continue;
      }
      map.set(
        track.track_id,
        track.clips.filter((clip) => {
          if (clip.clipId === selectedClipId) {
            return true;
          }
          const startX = (clip.startFrame / safeFps) * pxPerSec;
          const endX = (clip.endFrame / safeFps) * pxPerSec;
          return endX >= rangeStart && startX <= rangeEnd;
        })
      );
    }
    return map;
  }, [tracks, viewport, safeFps, pxPerSec, selectedClipId]);
  // 刻度同样随时长线性增长（长时间线细刻度可达数百上千个），一并按可视窗口裁剪；
  // 吸附候选仍用全量 ticks（buildSnapCandidates 不受影响）。
  const visibleTicks = useMemo(() => {
    if (viewport.width <= 0) {
      return ticks;
    }
    const rangeStart = viewport.left - TIMELINE_WINDOW_OVERSCAN_PX;
    const rangeEnd = viewport.left + viewport.width + TIMELINE_WINDOW_OVERSCAN_PX;
    return ticks.filter((tick) => {
      const x = tick.sec * pxPerSec;
      return x >= rangeStart && x <= rangeEnd;
    });
  }, [ticks, viewport, pxPerSec]);

  useLayoutEffect(() => {
    const pending = pendingZoomRef.current;
    const scroller = scrollSurfaceRef.current;
    if (!pending || !scroller) {
      return;
    }
    const target = pending.anchorSec * pxPerSec - pending.viewportX;
    scroller.scrollLeft = clamp(target, 0, Math.max(0, scroller.scrollWidth - scroller.clientWidth));
    pendingZoomRef.current = null;
  }, [pxPerSec]);

  const handleZoomWheel = useCallback(
    (event: ReactWheelEvent<HTMLDivElement>) => {
      if ((!event.ctrlKey && !event.metaKey) || !onZoomChange) {
        return;
      }
      event.preventDefault();
      const scroller = scrollSurfaceRef.current;
      if (!scroller) {
        return;
      }
      const rect = scroller.getBoundingClientRect();
      const viewportX = clamp(event.clientX - rect.left - LABEL_WIDTH, 0, scroller.clientWidth);
      const anchorSec = clamp((scroller.scrollLeft + viewportX) / pxPerSec, 0, durationSec);
      const next = Math.round(
        clamp(pxPerSec * Math.exp(-event.deltaY * 0.0025), MIN_PX_PER_SEC, MAX_PX_PER_SEC)
      );
      if (next === pxPerSec) {
        return;
      }
      pendingZoomRef.current = { anchorSec, viewportX };
      onZoomChange(next);
    },
    [durationSec, onZoomChange, pxPerSec]
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

  const seekSecAt = useCallback(
    (clientX: number) => {
      const rect = svgRef.current?.getBoundingClientRect();
      const localX = clientX - (rect?.left ?? 0);
      return clamp(localX / pxPerSec, 0, durationSec);
    },
    [durationSec, pxPerSec]
  );

  const beginSeekDrag = useCallback(
    (event: ReactPointerEvent<SVGSVGElement>) => {
      if (event.button !== 0) {
        return;
      }
      const target = event.target as Element | null;
      if (target?.closest("[data-clip-group]") && !target.closest("[data-playhead-handle]")) {
        return;
      }
      onDeselect?.();
      if (!onSeek) {
        return;
      }
      event.preventDefault();
      const sec = seekSecAt(event.clientX);
      seekDragRef.current = {
        pointerId: event.pointerId,
        lastPreviewAt: performance.now(),
        lastEmittedSec: sec
      };
      event.currentTarget.setPointerCapture?.(event.pointerId);
      updatePlayhead(sec, false);
      onSeek(sec);
    },
    [onDeselect, onSeek, seekSecAt, updatePlayhead]
  );

  const updateSeekDrag = useCallback(
    (event: ReactPointerEvent<SVGSVGElement>) => {
      const drag = seekDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId || !onSeek) {
        return;
      }
      event.preventDefault();
      const sec = seekSecAt(event.clientX);
      updatePlayhead(sec, false);
      const now = performance.now();
      if (now - drag.lastPreviewAt >= SEEK_PREVIEW_INTERVAL_MS) {
        drag.lastPreviewAt = now;
        drag.lastEmittedSec = sec;
        onSeek(sec);
      }
    },
    [onSeek, seekSecAt, updatePlayhead]
  );

  const finishSeekDrag = useCallback(
    (event: ReactPointerEvent<SVGSVGElement>) => {
      const drag = seekDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId || !onSeek) {
        return;
      }
      const sec = seekSecAt(event.clientX);
      seekDragRef.current = null;
      event.currentTarget.releasePointerCapture?.(event.pointerId);
      updatePlayhead(sec, false);
      if (Math.abs(sec - drag.lastEmittedSec) > Number.EPSILON) {
        onSeek(sec);
      }
    },
    [onSeek, seekSecAt, updatePlayhead]
  );

  const cancelSeekDrag = useCallback(() => {
    seekDragRef.current = null;
  }, []);

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

  const calculateFadeFrames = useCallback(
    (event: ReactPointerEvent<SVGCircleElement>, drag: FadeDragState) => {
      const delta = Math.round(((event.clientX - drag.startClientX) / pxPerSec) * safeFps);
      const duration = drag.clip.endFrame - drag.clip.startFrame;
      if (drag.edge === "in") {
        return {
          fadeInFrames: clamp(
            drag.clip.fadeInFrames + delta,
            0,
            Math.max(0, duration - drag.clip.fadeOutFrames)
          ),
          fadeOutFrames: drag.clip.fadeOutFrames
        };
      }
      return {
        fadeInFrames: drag.clip.fadeInFrames,
        fadeOutFrames: clamp(
          drag.clip.fadeOutFrames - delta,
          0,
          Math.max(0, duration - drag.clip.fadeInFrames)
        )
      };
    },
    [pxPerSec, safeFps]
  );

  const beginFadeDrag = useCallback(
    (
      event: ReactPointerEvent<SVGCircleElement>,
      clip: DrawableClip,
      edge: "in" | "out"
    ) => {
      if (editing || !onClipFadeChange) {
        return;
      }
      event.stopPropagation();
      fadeDragRef.current = {
        clip,
        edge,
        pointerId: event.pointerId,
        startClientX: event.clientX,
        element: event.currentTarget
      };
      event.currentTarget.setPointerCapture?.(event.pointerId);
    },
    [editing, onClipFadeChange]
  );

  const updateFadeDrag = useCallback(
    (event: ReactPointerEvent<SVGCircleElement>) => {
      const drag = fadeDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      event.preventDefault();
      const next = calculateFadeFrames(event, drag);
      const deltaFrames =
        drag.edge === "in"
          ? next.fadeInFrames - drag.clip.fadeInFrames
          : drag.clip.fadeOutFrames - next.fadeOutFrames;
      drag.element.setAttribute(
        "transform",
        `translate(${(deltaFrames / safeFps) * pxPerSec} 0)`
      );
    },
    [calculateFadeFrames, pxPerSec, safeFps]
  );

  const finishFadeDrag = useCallback(
    (event: ReactPointerEvent<SVGCircleElement>) => {
      const drag = fadeDragRef.current;
      if (!drag || drag.pointerId !== event.pointerId) {
        return;
      }
      const next = calculateFadeFrames(event, drag);
      drag.element.removeAttribute("transform");
      fadeDragRef.current = null;
      event.currentTarget.releasePointerCapture?.(event.pointerId);
      if (
        next.fadeInFrames !== drag.clip.fadeInFrames ||
        next.fadeOutFrames !== drag.clip.fadeOutFrames
      ) {
        onClipFadeChange?.(drag.clip.clipId, next.fadeInFrames, next.fadeOutFrames);
      }
    },
    [calculateFadeFrames, onClipFadeChange]
  );

  const cancelFadeDrag = useCallback(() => {
    fadeDragRef.current?.element.removeAttribute("transform");
    fadeDragRef.current = null;
  }, []);

  return (
    <div
      ref={scrollSurfaceRef}
      className="h-full overflow-auto"
      data-testid="timeline-scroll-surface"
      onWheel={handleZoomWheel}
      onScroll={handleScroll}
    >
      <div
        className="relative flex min-h-full min-w-full items-center"
        style={{ width: LABEL_WIDTH + timelineWidth }}
        data-testid="timeline-track-stack"
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
          className="block shrink-0 select-none touch-none"
          onPointerDown={beginSeekDrag}
          onPointerMove={updateSeekDrag}
          onPointerUp={finishSeekDrag}
          onPointerCancel={cancelSeekDrag}
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
            {visibleTicks.map((tick) => (
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
                    fill="var(--color-panel)"
                    opacity={track.muted ? 0.66 : 1}
                  />
                  <line
                    x1={0}
                    x2={timelineWidth}
                    y1={TRACK_HEIGHT - 0.5}
                    y2={TRACK_HEIGHT - 0.5}
                    stroke="var(--color-line-strong)"
                    strokeOpacity={0.9}
                    pointerEvents="none"
                  />
                  {(visibleClipsByTrack.get(track.track_id) ?? track.clips).map((clip) => {
                    const x = (clip.startFrame / safeFps) * pxPerSec;
                    const width = Math.max(
                      ((clip.endFrame - clip.startFrame) / safeFps) * pxPerSec,
                      2
                    );
                    const visualGeometry = clipVisualGeometry(x, width);
                    const visualX = visualGeometry.x;
                    const visualWidth = visualGeometry.width;
                    const selected = selectedClipId === clip.clipId;
                    const clipY = (TRACK_HEIGHT - CLIP_HEIGHT) / 2;
                    const sid = sanitizeId(clip.clipId);
                    const thumbUrl =
                      (track.kind === "video" || track.kind === "overlay") && clip.assetId
                        ? api.mediaThumbnailUrl(clip.assetId)
                        : null;
                    const waveform = clip.assetId ? waveforms.get(clip.assetId) ?? null : null;
                    const wavePath = track.kind === "audio"
                      ? buildWavePath(
                          waveform,
                          safeFps,
                          clip.sourceStartFrame,
                          clip.sourceEndFrame,
                          visualX,
                          clipY,
                          visualWidth,
                          clip.fadeInFrames,
                          clip.fadeOutFrames,
                          clip.endFrame - clip.startFrame
                        )
                      : null;
                    const fadeInWidth = Math.min(
                      visualWidth,
                      (clip.fadeInFrames / safeFps) * pxPerSec
                    );
                    const fadeOutWidth = Math.min(
                      visualWidth,
                      (clip.fadeOutFrames / safeFps) * pxPerSec
                    );
                    const clipBeatMarkers = buildBeatMarkersForClip(clip);
                    return (
                      <g
                        key={clip.clipId}
                        data-clip-group
                        data-clip-id={clip.clipId}
                        opacity={track.muted ? 0.5 : track.locked ? 0.72 : 1}
                      >
                        <rect
                          data-clip-body
                          x={visualX}
                          y={clipY}
                          width={visualWidth}
                          height={CLIP_HEIGHT}
                          rx={3}
                          fill={tone}
                          fillOpacity={selected ? 0.95 : 0.8}
                          pointerEvents="none"
                        />
                        {thumbUrl ? (
                          <>
                            {/* 胶片瓦片降级：同一 poster 改用 SVG pattern 平铺，节点数与 clip 宽度
                                解耦——替代原先每 56px 一个 <image>（单个长 clip 最多可达数百个）。
                                pattern 原点对齐 clip 左上角，配一条右边界分隔线复现胶片格视觉。 */}
                            <pattern
                              id={`tl-film-${sid}`}
                              patternUnits="userSpaceOnUse"
                              x={visualX}
                              y={clipY}
                              width={FILM_TILE_WIDTH}
                              height={CLIP_HEIGHT}
                            >
                              <image
                                href={thumbUrl}
                                x={0}
                                y={0}
                                width={FILM_TILE_WIDTH}
                                height={CLIP_HEIGHT}
                                preserveAspectRatio="xMidYMid slice"
                              />
                              <line
                                x1={FILM_TILE_WIDTH}
                                x2={FILM_TILE_WIDTH}
                                y1={0}
                                y2={CLIP_HEIGHT}
                                stroke="var(--color-ink)"
                                strokeOpacity={0.35}
                              />
                            </pattern>
                            <rect
                              data-clip-film
                              x={visualX}
                              y={clipY}
                              width={visualWidth}
                              height={CLIP_HEIGHT}
                              rx={3}
                              fill={`url(#tl-film-${sid})`}
                              pointerEvents="none"
                            />
                            <rect
                              x={visualX}
                              y={clipY}
                              width={visualWidth}
                              height={CLIP_HEIGHT}
                              rx={3}
                              fill="url(#tl-label-scrim)"
                              pointerEvents="none"
                            />
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
                        {track.kind === "audio" && fadeInWidth > 0 ? (
                          <path
                            d={buildFadeOverlayPath(visualX, clipY, fadeInWidth, CLIP_HEIGHT, "in")}
                            fill="var(--color-panel)"
                            fillOpacity={0.38}
                            stroke="var(--color-fg)"
                            strokeOpacity={0.42}
                            pointerEvents="none"
                          />
                        ) : null}
                        {track.kind === "audio" && fadeOutWidth > 0 ? (
                          <path
                            d={buildFadeOverlayPath(
                              visualX + visualWidth - fadeOutWidth,
                              clipY,
                              fadeOutWidth,
                              CLIP_HEIGHT,
                              "out"
                            )}
                            fill="var(--color-panel)"
                            fillOpacity={0.38}
                            stroke="var(--color-fg)"
                            strokeOpacity={0.42}
                            pointerEvents="none"
                          />
                        ) : null}
                        {track.kind === "audio" && clipBeatMarkers.length > 0 ? (
                          <g data-testid="timeline-beat-markers" data-clip-id={clip.clipId}>
                            {clipBeatMarkers.map((marker) => {
                              const markerX = (marker.frame / safeFps) * pxPerSec;
                              const downbeat = marker.kind === "downbeat";
                              const strong = marker.kind === "strong";
                              return (
                                <g
                                  key={`${marker.kind}-${marker.frame}`}
                                  transform={`translate(${markerX} 0)`}
                                  pointerEvents="none"
                                >
                                  <line
                                    y1={clipY + 5}
                                    y2={clipY + CLIP_HEIGHT - 2}
                                    stroke={
                                      downbeat
                                        ? "var(--color-accent)"
                                        : strong
                                          ? "var(--color-warn)"
                                          : "var(--color-fg)"
                                    }
                                    strokeWidth={downbeat ? 1.6 : strong ? 1.2 : 0.75}
                                    strokeOpacity={downbeat ? 0.95 : strong ? 0.78 : 0.34}
                                  />
                                  <path
                                    d={downbeat ? "M -4 4 L 0 0 L 4 4 L 0 8 Z" : strong ? "M -3 3 L 0 0 L 3 3 L 0 6 Z" : "M -1.5 1.5 A 1.5 1.5 0 1 0 1.5 1.5 A 1.5 1.5 0 1 0 -1.5 1.5"}
                                    transform={`translate(0 ${clipY + 1})`}
                                    fill={downbeat ? "var(--color-accent)" : strong ? "var(--color-warn)" : "var(--color-fg)"}
                                    fillOpacity={downbeat ? 1 : strong ? 0.9 : 0.58}
                                  >
                                    <title>
                                      {downbeat ? "小节强拍" : strong ? "音乐重拍" : "音乐拍点"} · {formatFrameTime(marker.frame, safeFps)}
                                    </title>
                                  </path>
                                </g>
                              );
                            })}
                          </g>
                        ) : null}
                        {clip.linked && width > 30 ? (
                          <Link2
                            x={visualX + 6}
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
                            x={visualX + (clip.linked ? 23 : 7)}
                            y={thumbUrl ? clipY + CLIP_HEIGHT - 7 : clipY + CLIP_HEIGHT / 2 + 4}
                            fill={thumbUrl ? "var(--color-on-media)" : "var(--color-fg)"}
                            fontSize={11}
                            fontWeight={600}
                            pointerEvents="none"
                          >
                            {truncateLabel(clip.label, visualWidth - (clip.linked ? 30 : 14))}
                          </text>
                        ) : null}
                        <rect
                          data-testid="timeline-clip"
                          data-clip-id={clip.clipId}
                          x={visualX}
                          y={clipY}
                          width={visualWidth}
                          height={CLIP_HEIGHT}
                          rx={3}
                          fill="transparent"
                          stroke={selected ? "var(--color-focus-ring)" : "var(--color-line-strong)"}
                          strokeWidth={selected ? 2.5 : 1.5}
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
                        {selected && track.kind === "audio" && onClipFadeChange && !track.locked ? (
                          <>
                            <circle
                              data-testid="timeline-fade-in-handle"
                              cx={visualX + fadeInWidth}
                              cy={clipY + 5}
                              r={4}
                              fill="var(--color-fg)"
                              stroke={tone}
                              strokeWidth={1.5}
                              role="slider"
                              aria-label={`${clip.label} 淡入`}
                              aria-valuemin={0}
                              aria-valuemax={clip.endFrame - clip.startFrame - clip.fadeOutFrames}
                              aria-valuenow={clip.fadeInFrames}
                              style={{ cursor: editing ? "wait" : "ew-resize" }}
                              onPointerDown={(event) => beginFadeDrag(event, clip, "in")}
                              onPointerMove={updateFadeDrag}
                              onPointerUp={finishFadeDrag}
                              onPointerCancel={cancelFadeDrag}
                            />
                            <circle
                              data-testid="timeline-fade-out-handle"
                              cx={visualX + visualWidth - fadeOutWidth}
                              cy={clipY + 5}
                              r={4}
                              fill="var(--color-fg)"
                              stroke={tone}
                              strokeWidth={1.5}
                              role="slider"
                              aria-label={`${clip.label} 淡出`}
                              aria-valuemin={0}
                              aria-valuemax={clip.endFrame - clip.startFrame - clip.fadeInFrames}
                              aria-valuenow={clip.fadeOutFrames}
                              style={{ cursor: editing ? "wait" : "ew-resize" }}
                              onPointerDown={(event) => beginFadeDrag(event, clip, "out")}
                              onPointerMove={updateFadeDrag}
                              onPointerUp={finishFadeDrag}
                              onPointerCancel={cancelFadeDrag}
                            />
                          </>
                        ) : null}
                        {selected && editMode === "trim" && onTrimClip && !track.locked ? (
                          <>
                            <rect
                              data-testid="timeline-trim-start"
                              data-trim-handle
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
                              data-trim-handle
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

          <g
            ref={playheadRef}
            data-testid="timeline-playhead"
            visibility="hidden"
            style={{ willChange: "transform" }}
          >
            <rect
              data-playhead-handle
              x={-8}
              y={0}
              width={16}
              height={svgHeight}
              fill="transparent"
              pointerEvents="all"
              style={{ cursor: "ew-resize" }}
            />
            <line
              x1={0}
              x2={0}
              y1={0}
              y2={svgHeight}
              stroke="var(--color-fg)"
              strokeWidth={1.5}
            />
            <rect x={-5} y={0} width={10} height={7} rx={2} fill="var(--color-fg)" />
            <path d="M -5 7 L 5 7 L 0 12 Z" fill="var(--color-fg)" />
          </g>
        </svg>
      </div>
    </div>
  );
  }
  )
);

function TrackHeader({
  track,
  editing,
  onChange
}: {
  track: DrawableTrack;
  editing: boolean;
  onChange?: (trackId: string, patch: TimelineTrackStatePatch) => void;
}): ReactElement {
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
      className={`flex flex-col justify-center border-b border-line-strong px-2 ${
        track.locked ? "bg-active/55" : "bg-panel"
      }`}
      style={{ height: TRACK_HEIGHT, marginBottom: TRACK_GAP }}
      data-track-header={track.track_id}
    >
      <div className="flex h-7 items-center gap-1.5">
        <span
          className="grid h-5 min-w-8 place-items-center rounded-sm px-1 text-[10px] font-bold tracking-wide text-white"
          style={{ backgroundColor: trackTone(track.track_id) }}
          aria-hidden
        >
          {trackBadge(track.track_id)}
        </span>
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

function useAssetWaveforms(
  tracks: DrawableTrack[],
  sampleCount: number
): Map<string, AssetWaveform> {
  const assetIds = useMemo(
    () => [
      ...new Set(
        tracks
          .filter((track) => track.kind === "audio")
          .flatMap((track) => track.clips)
          .flatMap((clip) => (clip.assetId ? [clip.assetId] : []))
      )
    ],
    [tracks]
  );
  const assetKey = assetIds.join("|");
  const [waveforms, setWaveforms] = useState<Map<string, AssetWaveform>>(() => new Map());
  useEffect(() => {
    setWaveforms(new Map());
    if (assetIds.length === 0) {
      return;
    }
    let cancelled = false;
    const instances = assetIds.map((assetId) => {
      const container = document.createElement("div");
      const wavesurfer = WaveSurfer.create({
        container,
        url: api.mediaProxyUrl(assetId),
        height: 0,
        interact: false
      });
      const handleReady = (): void => {
        if (cancelled) {
          return;
        }
        try {
          const channels =
            typeof wavesurfer.exportPeaks === "function"
              ? wavesurfer.exportPeaks({ maxLength: sampleCount })
              : [];
          const peaks = normalizeWavePeaks(channels);
          if (peaks.length > 1) {
            setWaveforms((current) => {
              const next = new Map(current);
              next.set(assetId, {
                durationSec: Math.max(0, wavesurfer.getDuration?.() ?? 0),
                peaks
              });
              return next;
            });
          }
        } catch {
          // 代理尚未生成或浏览器无法解码该音轨时保留纯色素材块。
        }
      };
      wavesurfer.on("ready", handleReady);
      wavesurfer.on("decode", handleReady);
      return wavesurfer;
    });
    return () => {
      cancelled = true;
      for (const wavesurfer of instances) {
        wavesurfer.destroy();
      }
    };
  // assetKey 是稳定的素材集合签名；避免纯轨道位置变化触发所有音频重新解码。
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [assetKey, sampleCount]);
  return waveforms;
}

function normalizeWavePeaks(channels: ArrayLike<ArrayLike<number>>): number[] {
  const channelList = Array.from(channels);
  const length = channelList.reduce((maxLength, channel) => Math.max(maxLength, channel.length), 0);
  const result = new Array<number>(length).fill(0);
  for (const channel of channelList) {
    for (let index = 0; index < channel.length; index += 1) {
      result[index] = Math.max(result[index] ?? 0, Math.abs(Number(channel[index]) || 0));
    }
  }
  return result;
}

function buildWavePath(
  waveform: AssetWaveform | null,
  fps: number,
  sourceStartFrame: number | null,
  sourceEndFrame: number | null,
  x: number,
  clipY: number,
  width: number,
  fadeInFrames: number,
  fadeOutFrames: number,
  clipDurationFrames: number
): string | null {
  if (!waveform || waveform.peaks.length < 2 || fps <= 0 || width <= 0) {
    return null;
  }
  const peaks = waveform.peaks;
  const durationSec = waveform.durationSec > 0
    ? waveform.durationSec
    : Math.max(0, (sourceEndFrame ?? 0) / fps);
  if (durationSec <= 0) {
    return null;
  }
  const startSec = Math.max(0, (sourceStartFrame ?? 0) / fps);
  const endSec = Math.max(startSec, (sourceEndFrame ?? Math.round(durationSec * fps)) / fps);
  const total = peaks.length;
  const i0 = clamp(Math.floor((startSec / durationSec) * total), 0, total - 1);
  const i1 = clamp(Math.ceil((endSec / durationSec) * total), i0 + 1, total);
  const slice = peaks.slice(i0, i1);
  if (slice.length < 2) {
    return null;
  }
  const pointCount = Math.min(slice.length, Math.max(2, Math.ceil(width)));
  const centerY = clipY + CLIP_HEIGHT / 2;
  const half = CLIP_HEIGHT / 2 - 4;
  const top: string[] = [];
  const bottom: string[] = [];
  for (let index = 0; index < pointCount; index += 1) {
    const ratio = index / (pointCount - 1);
    const sourceIndex = Math.min(slice.length - 1, Math.round(ratio * (slice.length - 1)));
    const pointX = x + ratio * width;
    const envelope = fadeEnvelope(ratio, fadeInFrames, fadeOutFrames, clipDurationFrames);
    const amplitude = Math.min(1, slice[sourceIndex] ?? 0) * envelope;
    top.push(`${pointX.toFixed(2)} ${(centerY - amplitude * half).toFixed(2)}`);
    bottom.push(`${pointX.toFixed(2)} ${(centerY + amplitude * half).toFixed(2)}`);
  }
  bottom.reverse();
  return `M ${top.join(" L ")} L ${bottom.join(" L ")} Z`;
}

function fadeEnvelope(
  ratio: number,
  fadeInFrames: number,
  fadeOutFrames: number,
  durationFrames: number
): number {
  if (durationFrames <= 0) {
    return 1;
  }
  const frame = ratio * durationFrames;
  const fadeIn = fadeInFrames > 0 ? clamp(frame / fadeInFrames, 0, 1) : 1;
  const fadeOut = fadeOutFrames > 0
    ? clamp((durationFrames - frame) / fadeOutFrames, 0, 1)
    : 1;
  return easeInOutQuad(Math.min(fadeIn, fadeOut));
}

function buildFadeOverlayPath(
  x: number,
  y: number,
  width: number,
  height: number,
  edge: "in" | "out"
): string {
  const points: string[] = [];
  const count = Math.max(4, Math.ceil(width / 6));
  for (let index = 0; index <= count; index += 1) {
    const ratio = index / count;
    const gain = easeInOutQuad(edge === "in" ? ratio : 1 - ratio);
    points.push(`${(x + ratio * width).toFixed(2)} ${(y + gain * height).toFixed(2)}`);
  }
  return `M ${x.toFixed(2)} ${y.toFixed(2)} L ${points.join(" L ")} L ${(x + width).toFixed(2)} ${y.toFixed(2)} Z`;
}

function easeInOutQuad(value: number): number {
  return value < 0.5 ? 2 * value * value : 1 - ((-2 * value + 2) ** 2) / 2;
}

function clipVisualGeometry(x: number, width: number): { x: number; width: number } {
  if (width < CLIP_VISUAL_GAP + 3) {
    return { x, width };
  }
  const inset = CLIP_VISUAL_GAP / 2;
  return { x: x + inset, width: Math.max(2, width - CLIP_VISUAL_GAP) };
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
      for (const frame of clip.downbeatFrames) {
        candidates.push({ frame, label: "小节强拍", priority: 0 });
      }
      for (const frame of clip.strongBeatFrames) {
        candidates.push({ frame, label: "音乐强拍", priority: 1 });
      }
      for (const frame of clip.beatFrames) {
        candidates.push({ frame, label: "音乐拍点", priority: 3 });
      }
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

function buildBeatMarkersForClip(clip: DrawableClip): BeatMarker[] {
  const markers = new Map<number, BeatMarker>();
  const add = (frames: number[], kind: BeatMarker["kind"]): void => {
    const priority = kind === "downbeat" ? 0 : kind === "strong" ? 1 : 2;
    for (const frame of frames) {
      if (frame < clip.startFrame || frame > clip.endFrame) {
        continue;
      }
      const current = markers.get(frame);
      const currentPriority = current?.kind === "downbeat" ? 0 : current?.kind === "strong" ? 1 : 2;
      if (!current || priority < currentPriority) {
        markers.set(frame, { frame, kind });
      }
    }
  };
  add(clip.beatFrames, "beat");
  add(clip.strongBeatFrames, "strong");
  add(clip.downbeatFrames, "downbeat");
  return [...markers.values()].sort((left, right) => left.frame - right.frame);
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
  const sourceTracks = [...timeline.tracks];
  for (const trackId of ["visual_base", "bgm", "sfx"]) {
    if (!sourceTracks.some((track) => track.track_id === trackId)) {
      sourceTracks.push({
        track_id: trackId,
        track_type: trackId === "visual_base" ? "primary_visual" : "audio",
        clips: []
      });
    }
  }
  return sourceTracks
    .map((track) => ({
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
    }))
    .filter(
      (track) =>
        ["visual_base", "bgm", "sfx"].includes(track.track_id) ||
        track.clips.length > 0 ||
        track.muted ||
        track.solo ||
        track.locked ||
        track.gainDb !== 0
    )
    .sort((left, right) => trackOrder(left.track_id) - trackOrder(right.track_id));
}

function trackOrder(trackId: string): number {
  const order = [
    "visual_base",
    "visual_overlay",
    "original_audio",
    "voiceover",
    "bgm",
    "sfx",
    "subtitles"
  ];
  const index = order.indexOf(trackId);
  return index >= 0 ? index : order.length;
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
  const beatGrid = (clip.effects ?? []).find((effect) => effect.kind === "beat_grid");
  const timelineStartFrame = clip.timeline_start_frame;
  const sourceStartFrame = typeof clip.source_start_frame === "number" ? clip.source_start_frame : 0;
  const sourceEndFrame = typeof clip.source_end_frame === "number"
    ? clip.source_end_frame
    : sourceStartFrame + clip.timeline_end_frame - clip.timeline_start_frame;
  const playbackRate =
    typeof clip.playback_rate === "number" && clip.playback_rate > 0 ? clip.playback_rate : 1;
  const mapBeatFrames = (value: unknown): number[] =>
    effectFrameArray(value).flatMap((sourceFrame) => {
      if (sourceFrame < sourceStartFrame || sourceFrame > sourceEndFrame) {
        return [];
      }
      return [
        timelineStartFrame + Math.round((sourceFrame - sourceStartFrame) / playbackRate)
      ];
    });
  return {
    clipId: clip.timeline_clip_id,
    trackId,
    startFrame: clip.timeline_start_frame,
    endFrame: clip.timeline_end_frame,
    label,
    assetId: typeof clip.asset_id === "string" ? clip.asset_id : null,
    assetKind: typeof clip.asset_kind === "string" ? clip.asset_kind : null,
    sourceStartFrame,
    sourceEndFrame,
    playbackRate,
    gainDb: typeof clip.gain_db === "number" ? clip.gain_db : 0,
    fadeInFrames:
      typeof clip.fade_in_frames === "number" && clip.fade_in_frames > 0
        ? Math.round(clip.fade_in_frames)
        : 0,
    fadeOutFrames:
      typeof clip.fade_out_frames === "number" && clip.fade_out_frames > 0
        ? Math.round(clip.fade_out_frames)
        : 0,
    linked: clip.linked === true,
    parentBlockId: typeof clip.parent_block_id === "string" ? clip.parent_block_id : null,
    beatFrames: mapBeatFrames(beatGrid?.beat_frames),
    strongBeatFrames: mapBeatFrames(beatGrid?.strong_beat_frames),
    downbeatFrames: mapBeatFrames(beatGrid?.downbeat_frames)
  };
}

function effectFrameArray(value: unknown): number[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.flatMap((frame) =>
    typeof frame === "number" && Number.isInteger(frame) && frame >= 0 ? [frame] : []
  );
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

function trackBadge(trackId: string): string {
  const badges: Record<string, string> = {
    visual_base: "V1",
    visual_primary: "V1",
    visual_overlay: "V2",
    original_audio: "A1",
    voiceover: "A2",
    bgm: "A1",
    sfx: "A2",
    subtitles: "T1"
  };
  return badges[trackId] ?? "·";
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
