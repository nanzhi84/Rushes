import {
  MediaPlayer,
  MediaProvider,
  useMediaRemote,
  useMediaState
} from "@vidstack/react";
import {
  Maximize,
  Minimize,
  Pause,
  Play,
  StepBack,
  StepForward,
  Volume1,
  Volume2,
  VolumeX
} from "lucide-react";
import { useCallback, useEffect, useMemo, useRef } from "react";
import type { ChangeEvent, MouseEvent, ReactElement } from "react";

export type PreviewPlayerProps = {
  src: string;
  fps: number;
  onFirstPlay?: () => void;
  onTimeUpdate?: (sec: number) => void;
  seekSec?: number | null;
  /** width：按宽度撑开（侧栏卡片）；height：填满父容器高度（工作台预览区）。 */
  fit?: "width" | "height";
};

/** lucide 统一 16px 档、stroke 1.5-1.75（图标纪律）。 */
const ICON_SIZE = 16;
const ICON_STROKE = 1.75;

export function PreviewPlayer({
  src,
  fps,
  onFirstPlay,
  onTimeUpdate,
  seekSec = null,
  fit = "width"
}: PreviewPlayerProps): ReactElement {
  return (
    <MediaPlayer
      src={src}
      playsInline
      className={`flex w-full flex-col overflow-hidden rounded-lg border border-line bg-black text-white shadow-raised ${
        fit === "height" ? "h-full min-h-0" : ""
      }`}
      aria-label="预览播放器"
    >
      <div className={fit === "height" ? "relative min-h-0 flex-1" : "relative aspect-[9/16] w-full"}>
        <MediaProvider />
      </div>
      <PreviewPlayerControls
        fps={fps}
        onFirstPlay={onFirstPlay}
        onTimeUpdate={onTimeUpdate}
        seekSec={seekSec}
      />
    </MediaPlayer>
  );
}

function PreviewPlayerControls({
  fps,
  onFirstPlay,
  onTimeUpdate,
  seekSec
}: {
  fps: number;
  onFirstPlay?: () => void;
  onTimeUpdate?: (sec: number) => void;
  seekSec?: number | null;
}): ReactElement {
  const currentTime = useMediaState("currentTime");
  const duration = useMediaState("duration");
  const bufferedEnd = useMediaState("bufferedEnd");
  const playing = useMediaState("playing");
  const paused = useMediaState("paused");
  const volume = useMediaState("volume");
  const muted = useMediaState("muted");
  const fullscreen = useMediaState("fullscreen");
  const remote = useMediaRemote();
  const firstPlayReportedRef = useRef(false);
  const lastSeekSecRef = useRef<number | null | undefined>(undefined);
  const latestTimeRef = useRef(currentTime);
  const timeReportFrameRef = useRef<number | null>(null);
  const safeFps = useMemo(() => (fps > 0 ? fps : 30), [fps]);

  const safeDuration = Number.isFinite(duration) && duration > 0 ? duration : 0;
  const playedPct = safeDuration > 0 ? clampPct((currentTime / safeDuration) * 100) : 0;
  const bufferedPct =
    safeDuration > 0
      ? Math.max(playedPct, clampPct(((bufferedEnd ?? 0) / safeDuration) * 100))
      : 0;
  const effectiveVolume = muted ? 0 : (volume ?? 1);

  useEffect(() => {
    if (!playing || firstPlayReportedRef.current) {
      return;
    }
    firstPlayReportedRef.current = true;
    onFirstPlay?.();
  }, [onFirstPlay, playing]);

  useEffect(() => {
    if (seekSec === null || seekSec === undefined) {
      lastSeekSecRef.current = null;
      return;
    }
    if (lastSeekSecRef.current === seekSec) {
      return;
    }
    lastSeekSecRef.current = seekSec;
    remote.seek(Math.max(0, seekSec));
  }, [remote, seekSec]);

  useEffect(() => {
    latestTimeRef.current = currentTime;
    if (!onTimeUpdate || timeReportFrameRef.current !== null) {
      return;
    }
    timeReportFrameRef.current = scheduleFrame(() => {
      timeReportFrameRef.current = null;
      onTimeUpdate(latestTimeRef.current);
    });
  }, [currentTime, onTimeUpdate]);

  useEffect(
    () => () => {
      if (timeReportFrameRef.current !== null) {
        cancelFrame(timeReportFrameRef.current);
      }
    },
    []
  );

  const stepFrame = useCallback(
    (direction: -1 | 1, event: MouseEvent<HTMLButtonElement>) => {
      const nextTime = Math.max(0, currentTime + direction / safeFps);
      remote.seek(nextTime, event.nativeEvent);
    },
    [currentTime, remote, safeFps]
  );

  const handleScrub = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      remote.seek(Number(event.target.value), event.nativeEvent);
    },
    [remote]
  );

  const handleVolume = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const next = Number(event.target.value);
      remote.changeVolume(next, event.nativeEvent);
      if (next > 0 && muted) {
        remote.unmute(event.nativeEvent);
      }
    },
    [muted, remote]
  );

  const VolumeIcon = effectiveVolume <= 0 ? VolumeX : effectiveVolume < 0.5 ? Volume1 : Volume2;

  return (
    <div className="flex flex-col gap-2 border-t border-line bg-raised px-3 pb-2 pt-2.5">
      {/* 可拖 scrub 进度条（accent 已播 / fg-faint 缓冲 / line-strong 未缓冲） */}
      <ScrubBar
        currentTime={currentTime}
        duration={safeDuration}
        playedPct={playedPct}
        bufferedPct={bufferedPct}
        onScrub={handleScrub}
      />

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <button
          type="button"
          className="grid size-8 place-items-center rounded-md bg-accent text-white transition-colors duration-[var(--duration-fast)] ease-standard hover:bg-accent-strong focus:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring"
          aria-label={paused ? "播放" : "暂停"}
          onClick={(event) => {
            if (paused) {
              remote.play(event.nativeEvent);
            } else {
              remote.pause(event.nativeEvent);
            }
          }}
        >
          {paused ? (
            <Play size={ICON_SIZE} strokeWidth={ICON_STROKE} fill="currentColor" />
          ) : (
            <Pause size={ICON_SIZE} strokeWidth={ICON_STROKE} fill="currentColor" />
          )}
        </button>
        <button
          type="button"
          className="grid size-8 place-items-center rounded-md border border-line text-fg-muted transition-colors duration-[var(--duration-fast)] ease-standard hover:bg-hover hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring"
          aria-label="后退一帧"
          title="后退一帧"
          onClick={(event) => stepFrame(-1, event)}
        >
          <StepBack size={ICON_SIZE} strokeWidth={ICON_STROKE} />
        </button>
        <button
          type="button"
          className="grid size-8 place-items-center rounded-md border border-line text-fg-muted transition-colors duration-[var(--duration-fast)] ease-standard hover:bg-hover hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring"
          aria-label="前进一帧"
          title="前进一帧"
          onClick={(event) => stepFrame(1, event)}
        >
          <StepForward size={ICON_SIZE} strokeWidth={ICON_STROKE} />
        </button>

        <div className="font-mono text-xs tabular-nums text-fg-muted">
          <span className="text-fg">{formatTimecode(currentTime, safeFps)}</span>
          <span className="px-1 text-fg-faint">/</span>
          <span>{formatTimecode(safeDuration, safeFps)}</span>
        </div>

        <div className="ml-auto flex items-center gap-1.5">
          <div className="flex items-center gap-1.5">
            <button
              type="button"
              className="grid size-8 place-items-center rounded-md text-fg-muted transition-colors duration-[var(--duration-fast)] ease-standard hover:bg-hover hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring"
              aria-label={muted ? "取消静音" : "静音"}
              title={muted ? "取消静音" : "静音"}
              onClick={(event) => remote.toggleMuted(event.nativeEvent)}
            >
              <VolumeIcon size={ICON_SIZE} strokeWidth={ICON_STROKE} />
            </button>
            <VolumeBar volume={effectiveVolume} onChange={handleVolume} />
          </div>
          <button
            type="button"
            className="grid size-8 place-items-center rounded-md text-fg-muted transition-colors duration-[var(--duration-fast)] ease-standard hover:bg-hover hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring"
            aria-label={fullscreen ? "退出全屏" : "全屏"}
            title={fullscreen ? "退出全屏" : "全屏"}
            onClick={(event) => remote.toggleFullscreen(undefined, event.nativeEvent)}
          >
            {fullscreen ? (
              <Minimize size={ICON_SIZE} strokeWidth={ICON_STROKE} />
            ) : (
              <Maximize size={ICON_SIZE} strokeWidth={ICON_STROKE} />
            )}
          </button>
        </div>
      </div>
    </div>
  );
}

/** 进度条：透明原生 range 在上层接管拖拽/键盘，token 色轨在下层绘制质感。 */
function ScrubBar({
  currentTime,
  duration,
  playedPct,
  bufferedPct,
  onScrub
}: {
  currentTime: number;
  duration: number;
  playedPct: number;
  bufferedPct: number;
  onScrub: (event: ChangeEvent<HTMLInputElement>) => void;
}): ReactElement {
  return (
    <div className="group relative flex h-4 w-full items-center">
      <div className="pointer-events-none absolute inset-x-0 h-1.5 rounded-full bg-line-strong" />
      <div
        className="pointer-events-none absolute left-0 h-1.5 rounded-full bg-fg-faint"
        style={{ width: `${bufferedPct}%` }}
      />
      <div
        className="pointer-events-none absolute left-0 h-1.5 rounded-full bg-accent"
        style={{ width: `${playedPct}%` }}
      />
      <div
        className="pointer-events-none absolute size-3 -translate-x-1/2 rounded-full bg-fg opacity-0 shadow-raised transition-opacity duration-[var(--duration-fast)] ease-standard group-hover:opacity-100 group-focus-within:opacity-100"
        style={{ left: `${playedPct}%` }}
      />
      <input
        type="range"
        aria-label="播放进度"
        min={0}
        max={duration || 0}
        step="any"
        value={Math.min(currentTime, duration || 0)}
        disabled={duration <= 0}
        onChange={onScrub}
        className="absolute inset-0 m-0 h-full w-full cursor-pointer appearance-none bg-transparent opacity-0 disabled:cursor-not-allowed"
      />
    </div>
  );
}

/** 音量：轨道走中性 fg-muted（accent 减负，只留主进度用 accent）。 */
function VolumeBar({
  volume,
  onChange
}: {
  volume: number;
  onChange: (event: ChangeEvent<HTMLInputElement>) => void;
}): ReactElement {
  const pct = clampPct(volume * 100);
  return (
    <div className="group relative flex h-4 w-16 items-center">
      <div className="pointer-events-none absolute inset-x-0 h-1 rounded-full bg-line-strong" />
      <div
        className="pointer-events-none absolute left-0 h-1 rounded-full bg-fg-muted"
        style={{ width: `${pct}%` }}
      />
      <div
        className="pointer-events-none absolute size-2.5 -translate-x-1/2 rounded-full bg-fg shadow-raised opacity-0 transition-opacity duration-[var(--duration-fast)] ease-standard group-hover:opacity-100 group-focus-within:opacity-100"
        style={{ left: `${pct}%` }}
      />
      <input
        type="range"
        aria-label="音量"
        min={0}
        max={1}
        step={0.01}
        value={volume}
        onChange={onChange}
        className="absolute inset-0 m-0 h-full w-full cursor-pointer appearance-none bg-transparent opacity-0"
      />
    </div>
  );
}

function clampPct(value: number): number {
  if (!Number.isFinite(value)) {
    return 0;
  }
  return Math.min(100, Math.max(0, value));
}

/** 时间码 mm:ss:ff（ff 为当前秒内帧号，按 fps 折算）。 */
function formatTimecode(sec: number, fps: number): string {
  const safeSec = Number.isFinite(sec) && sec > 0 ? sec : 0;
  const totalFrames = Math.round(safeSec * fps);
  const frames = totalFrames % fps;
  const totalSeconds = Math.floor(totalFrames / fps);
  const seconds = totalSeconds % 60;
  const minutes = Math.floor(totalSeconds / 60);
  return `${pad2(minutes)}:${pad2(seconds)}:${pad2(frames)}`;
}

function pad2(value: number): string {
  return value.toString().padStart(2, "0");
}

function scheduleFrame(callback: () => void): number {
  if (typeof window.requestAnimationFrame === "function") {
    return window.requestAnimationFrame(callback);
  }
  return window.setTimeout(callback, 16);
}

function cancelFrame(frameId: number): void {
  if (typeof window.cancelAnimationFrame === "function") {
    window.cancelAnimationFrame(frameId);
    return;
  }
  window.clearTimeout(frameId);
}
