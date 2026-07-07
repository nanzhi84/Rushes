import {
  MediaPlayer,
  MediaProvider,
  useMediaRemote,
  useMediaState
} from "@vidstack/react";
import { useCallback, useEffect, useMemo, useRef } from "react";
import type { MouseEvent, ReactElement } from "react";

export type PreviewPlayerProps = {
  src: string;
  fps: number;
  onFirstPlay?: () => void;
  onTimeUpdate?: (sec: number) => void;
  seekSec?: number | null;
  /** width：按宽度撑开（侧栏卡片）；height：填满父容器高度（工作台预览区）。 */
  fit?: "width" | "height";
};

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
      className={`flex w-full flex-col overflow-hidden rounded-lg border border-line bg-black text-white ${
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
  const playing = useMediaState("playing");
  const paused = useMediaState("paused");
  const remote = useMediaRemote();
  const firstPlayReportedRef = useRef(false);
  const lastSeekSecRef = useRef<number | null | undefined>(undefined);
  const latestTimeRef = useRef(currentTime);
  const timeReportFrameRef = useRef<number | null>(null);
  const safeFps = useMemo(() => (fps > 0 ? fps : 30), [fps]);
  const currentFrame = Math.round(currentTime * safeFps);

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

  return (
    <div className="flex items-center justify-between gap-3 border-t border-line bg-panel px-3 py-2">
      <div className="text-xs text-fg-muted">
        当前帧 <span className="font-mono tabular-nums text-fg">{currentFrame}</span>
      </div>
      <div className="flex items-center gap-2">
        <button
          type="button"
          className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white hover:bg-accent-strong focus:outline-none focus:ring-2 focus:ring-white/40"
          aria-label={paused ? "播放" : "暂停"}
          onClick={(event) => {
            if (paused) {
              remote.play(event.nativeEvent);
            } else {
              remote.pause(event.nativeEvent);
            }
          }}
        >
          {paused ? "播放" : "暂停"}
        </button>
        <button
          type="button"
          className="rounded-md border border-line px-3 py-1.5 text-sm font-medium text-fg hover:bg-hover focus:outline-none focus:ring-2 focus:ring-white/40"
          aria-label="后退一帧"
          onClick={(event) => stepFrame(-1, event)}
        >
          -1 帧
        </button>
        <button
          type="button"
          className="rounded-md border border-line px-3 py-1.5 text-sm font-medium text-fg hover:bg-hover focus:outline-none focus:ring-2 focus:ring-white/40"
          aria-label="前进一帧"
          onClick={(event) => stepFrame(1, event)}
        >
          +1 帧
        </button>
      </div>
    </div>
  );
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
