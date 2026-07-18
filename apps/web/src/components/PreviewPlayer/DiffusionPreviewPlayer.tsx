import { Pause, Play, StepBack, StepForward } from "lucide-react";
import { memo, useCallback, useEffect, useRef, useState } from "react";
import type {
  ChangeEvent,
  KeyboardEvent,
  PointerEvent as ReactPointerEvent,
  ReactElement
} from "react";
import { api } from "../../api/client";
import type { TimelineJson } from "../../api/client";
import type { DiffusionPreviewEngine } from "../../editor/diffusion_preview_engine";
import { PreviewPlayer } from "./PreviewPlayer";

export type DiffusionPreviewPlayerProps = {
  timeline: TimelineJson;
  fallbackSrc?: string | null;
  onFirstPlay?: () => void;
  onTimeUpdate?: (sec: number) => void;
  seekSec?: number | null;
};

type Phase = "loading" | "ready" | "error";

// 预览播放器持有原生解码器与 rAF 播放循环，重挂载代价高；用 memo 让它不随左栏
// 流式对话的高频重渲染而重建。上层 props 已稳定：timeline 来自 EditorSession 快照、
// 回调走 useCallback、seekSec/fallbackSrc 为按值稳定的基本类型。默认浅比较即可生效。
export const DiffusionPreviewPlayer = memo(function DiffusionPreviewPlayer({
  timeline,
  fallbackSrc = null,
  onFirstPlay,
  onTimeUpdate,
  seekSec = null
}: DiffusionPreviewPlayerProps): ReactElement {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const engineRef = useRef<DiffusionPreviewEngine | null>(null);
  const latestTimelineRef = useRef(timeline);
  const firstPlayRef = useRef(false);
  const onFirstPlayRef = useRef(onFirstPlay);
  const onTimeUpdateRef = useRef(onTimeUpdate);
  const lastUiUpdateRef = useRef(0);
  const pendingSeekFrameRef = useRef<number | null>(null);
  const seekInFlightRef = useRef(false);
  const [phase, setPhase] = useState<Phase>("loading");
  const [playing, setPlaying] = useState(false);
  const [currentSec, setCurrentSec] = useState(0);
  const [playbackNotice, setPlaybackNotice] = useState<string | null>(null);
  const durationSec = timeline.duration_frames / (timeline.fps > 0 ? timeline.fps : 30);
  latestTimelineRef.current = timeline;
  onFirstPlayRef.current = onFirstPlay;
  onTimeUpdateRef.current = onTimeUpdate;

  useEffect(() => {
    // preview_id 只会在服务端确认“与当前 timeline version 完全一致”时返回。
    // 这时优先使用单文件渲染预览：浏览器只维护一个原生解码器，避免复杂
    // 时间线为每个切片各建 WebCodecs decoder 导致初始化和播放卡死。
    if (fallbackSrc) {
      setPhase("ready");
      return;
    }
    if (!supportsLocalPreview()) {
      setPhase("error");
      return;
    }
    let cancelled = false;
    let offTime: (() => void) | null = null;
    const start = async (): Promise<void> => {
      const host = hostRef.current;
      if (!host) {
        return;
      }
      try {
        const { DiffusionPreviewEngine: Engine } = await import(
          "../../editor/diffusion_preview_engine"
        );
        if (cancelled) {
          return;
        }
        const engine = new Engine((assetId, assetKind) =>
          assetKind === "image" ? api.mediaSourceUrl(assetId) : api.mediaProxyUrl(assetId)
        );
        engineRef.current = engine;
        engine.mount(host);
        offTime = engine.onTime((seconds) => {
          const currentTimeline = latestTimelineRef.current;
          const fps = currentTimeline.fps > 0 ? currentTimeline.fps : 30;
          const end = currentTimeline.duration_frames / fps;
          if (seconds >= end - 1 / fps) {
            setPlaying(false);
          }
          const now = performance.now();
          if (now - lastUiUpdateRef.current >= 80) {
            lastUiUpdateRef.current = now;
            setCurrentSec(seconds);
          }
        });
        await engine.sync(latestTimelineRef.current);
        if (!cancelled) {
          setCurrentSec(engine.composition.currentTime);
          setPhase("ready");
        }
      } catch (error) {
        console.warn("Diffusion Studio 代理预览初始化失败", error);
        if (!cancelled) {
          setPhase("error");
        }
        await engineRef.current?.dispose();
        engineRef.current = null;
      }
    };
    void start();
    return () => {
      cancelled = true;
      offTime?.();
      const engine = engineRef.current;
      engineRef.current = null;
      if (engine) {
        void engine.dispose();
      }
    };
  }, [fallbackSrc]);

  // 播放头跟随显示器刷新率，而不是跟着 React render 或 30fps 解码事件跳动。
  // composition.currentTime 来自 AudioContext 的连续时钟；每个 rAF 只做一次命令式
  // 时间线 transform 更新，React 数字显示仍按 80ms 节流。
  useEffect(() => {
    if (fallbackSrc || !playing || phase !== "ready") {
      return;
    }
    let frameId = 0;
    let cancelled = false;
    let recoveryInFlight = false;
    let recoveryAttempted = false;
    let lastProgressSec = engineRef.current?.composition.currentTime ?? 0;
    let lastProgressAt = performance.now();

    const stopAsRecoverable = async (message: string): Promise<void> => {
      const engine = engineRef.current;
      await engine?.pause().catch(() => undefined);
      if (!cancelled) {
        setPlaying(false);
        setPlaybackNotice(message);
      }
    };

    const recover = async (reason: "clock" | "decode"): Promise<void> => {
      if (recoveryInFlight) {
        return;
      }
      if (recoveryAttempted) {
        await stopAsRecoverable(
          reason === "clock"
            ? "浏览器暂停了音频时钟，点击播放即可继续"
            : "预览解码再次停顿，已自动暂停；点击播放可继续"
        );
        return;
      }
      const engine = engineRef.current;
      if (!engine) {
        return;
      }
      recoveryAttempted = true;
      recoveryInFlight = true;
      setPlaybackNotice("预览短暂停顿，正在自动恢复…");
      try {
        await engine.recoverPlayback();
        if (!cancelled) {
          lastProgressSec = engine.composition.currentTime;
          lastProgressAt = performance.now();
          setPlaybackNotice(null);
        }
      } catch (error) {
        console.warn("Diffusion Studio 代理预览自动恢复失败", error);
        await stopAsRecoverable(
          reason === "clock"
            ? "浏览器暂停了音频时钟，点击播放即可继续"
            : "预览解码停顿，已自动暂停；点击播放可继续"
        );
      } finally {
        recoveryInFlight = false;
      }
    };

    const tick = (): void => {
      const engine = engineRef.current;
      if (!engine) {
        return;
      }
      if (!engine.playing) {
        // 引擎可能因为到达末尾或底层解码停止而自行结束。同步退出 React
        // 播放态，避免 rAF 已停但按钮仍显示“暂停”，让用户误以为预览卡死。
        setPlaying(false);
        return;
      }
      const seconds = engine.composition.currentTime;
      onTimeUpdateRef.current?.(seconds);
      const now = performance.now();
      const currentTimeline = latestTimelineRef.current;
      const fps = currentTimeline.fps > 0 ? currentTimeline.fps : 30;
      const end = currentTimeline.duration_frames / fps;
      if (Math.abs(seconds - lastProgressSec) >= 1 / Math.max(120, fps * 4)) {
        lastProgressSec = seconds;
        lastProgressAt = now;
      } else if (seconds < end - 1 / fps && !recoveryInFlight) {
        if (engine.clockState !== "running") {
          void recover("clock");
        } else if (now - lastProgressAt >= 1_800) {
          void recover("decode");
        }
      }
      if (now - lastUiUpdateRef.current >= 80) {
        lastUiUpdateRef.current = now;
        setCurrentSec(seconds);
      }
      frameId = window.requestAnimationFrame(tick);
    };
    frameId = window.requestAnimationFrame(tick);
    return () => {
      cancelled = true;
      window.cancelAnimationFrame(frameId);
    };
  }, [fallbackSrc, phase, playing]);

  useEffect(() => {
    const engine = engineRef.current;
    if (fallbackSrc || !engine || phase === "loading") {
      return;
    }
    void engine.sync(timeline).catch((error) => {
      console.warn("Diffusion Studio 代理预览同步失败", error);
      setPhase("error");
    });
  }, [fallbackSrc, phase, timeline]);

  useEffect(() => {
    const engine = engineRef.current;
    if (fallbackSrc || !engine || seekSec === null || !Number.isFinite(seekSec)) {
      return;
    }
    const frame = Math.round(Math.max(0, seekSec) * (timeline.fps > 0 ? timeline.fps : 30));
    void engine.seekFrame(frame).then(() => setCurrentSec(Math.max(0, seekSec)));
  }, [fallbackSrc, seekSec, timeline.fps]);

  const togglePlayback = useCallback(async () => {
    const engine = engineRef.current;
    if (!engine) {
      return;
    }
    try {
      if (engine.playing) {
        await engine.pause();
        setPlaying(false);
        return;
      }
      setPlaybackNotice(null);
      await engine.play();
      setPlaying(true);
      if (!firstPlayRef.current) {
        firstPlayRef.current = true;
        onFirstPlayRef.current?.();
      }
    } catch (error) {
      console.warn("Diffusion Studio 代理预览播放失败", error);
      setPlaying(false);
      // AudioContext 首次启动可能被浏览器的手势策略暂时拒绝。引擎本身并未
      // 损坏，因此保留 ready 状态让用户再次点击；不能把一次授权失败升级成
      // 永久禁用的整块预览错误态。
    }
  }, []);

  const flushPendingSeek = useCallback(async (): Promise<void> => {
    const engine = engineRef.current;
    if (!engine || seekInFlightRef.current) {
      return;
    }
    seekInFlightRef.current = true;
    try {
      while (pendingSeekFrameRef.current !== null && engineRef.current === engine) {
        const frame = pendingSeekFrameRef.current;
        pendingSeekFrameRef.current = null;
        await engine.seekFrame(frame);
      }
    } catch (error) {
      pendingSeekFrameRef.current = null;
      console.warn("Diffusion Studio 代理预览定位失败", error);
      if (engineRef.current === engine) {
        setPhase("error");
      }
    } finally {
      seekInFlightRef.current = false;
      if (engineRef.current !== engine) {
        pendingSeekFrameRef.current = null;
      }
    }
  }, []);

  const seek = useCallback(
    (seconds: number) => {
      if (!engineRef.current) {
        return;
      }
      const fps = timeline.fps > 0 ? timeline.fps : 30;
      const safe = Math.min(Math.max(0, seconds), durationSec);

      // 先更新小型预览控制区，再异步驱动解码器。连续拖动只保留当前
      // seek 完成后的最新目标帧，避免受控 range 回弹或堆积解码请求。
      setCurrentSec(safe);
      onTimeUpdateRef.current?.(safe);
      pendingSeekFrameRef.current = Math.round(safe * fps);
      void flushPendingSeek();
    },
    [durationSec, flushPendingSeek, timeline.fps]
  );

  if (fallbackSrc) {
    return (
      <div className="relative h-full min-h-0" data-preview-engine="ffmpeg-current-version">
        <PreviewPlayer
          src={fallbackSrc}
          fps={timeline.fps}
          fit="height"
          onFirstPlay={onFirstPlay}
          onTimeUpdate={onTimeUpdate}
          seekSec={seekSec}
        />
        <span className="absolute right-2 top-2 rounded-sm bg-black/70 px-1.5 py-1 text-[9px] text-white/80">
          当前时间线 · 稳定预览
        </span>
      </div>
    );
  }

  return (
    <div
      className="flex h-full min-h-0 w-full flex-col overflow-hidden border border-line bg-black text-white"
      aria-label="Diffusion Studio 代理预览"
      data-preview-engine="diffusion-studio-core"
    >
      <div className="relative min-h-0 flex-1 overflow-hidden bg-black">
        <div ref={hostRef} className="grid h-full w-full place-items-center overflow-hidden" />
        <span className="absolute right-2 top-2 rounded-sm bg-black/70 px-1.5 py-1 text-[9px] text-white/80">
          Diffusion Core · 编辑代理
        </span>
        {phase === "loading" ? (
          <div className="absolute inset-0 grid place-items-center bg-black/55 text-xs text-white/75">
            正在准备本地即时预览…
          </div>
        ) : null}
        {phase === "error" ? (
          <div className="absolute inset-0 grid place-items-center bg-black/70 px-6 text-center text-xs text-white/75" role="alert">
            当前浏览器无法启动编辑代理预览；最终导出仍会读取原素材。
          </div>
        ) : null}
        {playbackNotice ? (
          <div
            className="absolute bottom-2 left-1/2 -translate-x-1/2 rounded-sm bg-black/75 px-2 py-1 text-[10px] text-white/85"
            role="status"
          >
            {playbackNotice}
          </div>
        ) : null}
      </div>

      <div className="flex shrink-0 flex-col gap-1 border-t border-line bg-raised px-2 pb-1.5 pt-1.5">
        <PreviewScrubber
          currentSec={currentSec}
          durationSec={durationSec}
          fps={timeline.fps}
          disabled={phase !== "ready" || durationSec <= 0}
          onSeek={seek}
        />
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="grid size-7 place-items-center rounded-sm bg-accent text-white disabled:opacity-40"
            aria-label={playing ? "暂停" : "播放"}
            disabled={phase !== "ready"}
            onClick={() => void togglePlayback()}
          >
            {playing ? <Pause size={16} fill="currentColor" /> : <Play size={16} fill="currentColor" />}
          </button>
          <button
            type="button"
            className="grid size-7 place-items-center rounded-sm text-fg-muted hover:bg-hover hover:text-fg disabled:opacity-40"
            aria-label="后退一帧"
            disabled={phase !== "ready"}
            onClick={() => void seek(currentSec - 1 / Math.max(1, timeline.fps))}
          >
            <StepBack size={16} />
          </button>
          <button
            type="button"
            className="grid size-7 place-items-center rounded-sm text-fg-muted hover:bg-hover hover:text-fg disabled:opacity-40"
            aria-label="前进一帧"
            disabled={phase !== "ready"}
            onClick={() => void seek(currentSec + 1 / Math.max(1, timeline.fps))}
          >
            <StepForward size={16} />
          </button>
          <span className="font-mono text-xs tabular-nums text-fg-muted">
            {formatTime(currentSec, timeline.fps)} / {formatTime(durationSec, timeline.fps)}
          </span>
        </div>
      </div>
    </div>
  );
});

export function PreviewScrubber({
  currentSec,
  durationSec,
  fps,
  disabled,
  onSeek
}: {
  currentSec: number;
  durationSec: number;
  fps: number;
  disabled: boolean;
  onSeek: (seconds: number) => void;
}): ReactElement {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const pointerActiveRef = useRef(false);
  const safeDuration = Math.max(0, Number.isFinite(durationSec) ? durationSec : 0);
  const safeCurrent = Math.min(
    Math.max(0, Number.isFinite(currentSec) ? currentSec : 0),
    safeDuration
  );
  const frameStep = 1 / Math.max(1, fps);
  const playedPct = safeDuration > 0 ? (safeCurrent / safeDuration) * 100 : 0;

  const seekFromPointer = (event: ReactPointerEvent<HTMLDivElement>): void => {
    const rect = event.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) {
      return;
    }
    const ratio = Math.min(Math.max(0, (event.clientX - rect.left) / rect.width), 1);
    onSeek(ratio * safeDuration);
  };

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>): void => {
    let target: number | null = null;
    switch (event.key) {
      case "ArrowLeft":
      case "ArrowDown":
        target = safeCurrent - frameStep;
        break;
      case "ArrowRight":
      case "ArrowUp":
        target = safeCurrent + frameStep;
        break;
      case "Home":
        target = 0;
        break;
      case "End":
        target = safeDuration;
        break;
      default:
        return;
    }
    event.preventDefault();
    onSeek(Math.min(Math.max(0, target), safeDuration));
  };

  return (
    <div
      className={`group relative flex h-5 w-full select-none items-center touch-none ${disabled ? "cursor-not-allowed opacity-40" : "cursor-pointer active:cursor-grabbing"}`}
      data-testid="preview-scrubber-hit-area"
      onPointerDown={(event) => {
        if (disabled || event.button !== 0) {
          return;
        }
        event.preventDefault();
        pointerActiveRef.current = true;
        event.currentTarget.setPointerCapture(event.pointerId);
        inputRef.current?.focus({ preventScroll: true });
        seekFromPointer(event);
      }}
      onPointerMove={(event) => {
        if (!pointerActiveRef.current) {
          return;
        }
        event.preventDefault();
        seekFromPointer(event);
      }}
      onPointerUp={(event) => {
        if (!pointerActiveRef.current) {
          return;
        }
        seekFromPointer(event);
        pointerActiveRef.current = false;
        if (event.currentTarget.hasPointerCapture(event.pointerId)) {
          event.currentTarget.releasePointerCapture(event.pointerId);
        }
      }}
      onPointerCancel={(event) => {
        pointerActiveRef.current = false;
        if (event.currentTarget.hasPointerCapture(event.pointerId)) {
          event.currentTarget.releasePointerCapture(event.pointerId);
        }
      }}
      onLostPointerCapture={() => {
        pointerActiveRef.current = false;
      }}
    >
      <div className="pointer-events-none absolute inset-x-0 h-1 rounded-full bg-line-strong" />
      <div
        className="pointer-events-none absolute left-0 h-1 rounded-full bg-accent"
        data-testid="preview-scrubber-progress"
        style={{ width: `${playedPct}%` }}
      />
      <div
        className="pointer-events-none absolute size-3.5 -translate-x-1/2 rounded-full border-2 border-raised bg-accent shadow-raised transition-transform duration-[var(--duration-fast)] ease-standard group-hover:scale-110 group-focus-within:scale-110 group-focus-within:ring-2 group-focus-within:ring-focus-ring group-focus-within:ring-offset-1 group-focus-within:ring-offset-raised motion-reduce:transition-none"
        data-testid="preview-scrubber-thumb"
        style={{ left: `${playedPct}%` }}
      />
      <input
        ref={inputRef}
        type="range"
        aria-label="预览进度"
        aria-valuetext={`${formatTime(safeCurrent, fps)} / ${formatTime(safeDuration, fps)}`}
        aria-keyshortcuts="ArrowLeft ArrowRight ArrowUp ArrowDown Home End"
        title="拖动定位，方向键逐帧移动"
        min={0}
        max={Math.max(safeDuration, 0.001)}
        step={frameStep}
        value={safeCurrent}
        disabled={disabled}
        onChange={(event: ChangeEvent<HTMLInputElement>) => onSeek(Number(event.target.value))}
        onKeyDown={handleKeyDown}
        className="pointer-events-none absolute inset-0 m-0 h-full w-full appearance-none bg-transparent opacity-0 focus:outline-none disabled:cursor-not-allowed"
      />
    </div>
  );
}

function supportsLocalPreview(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof document !== "undefined" &&
    typeof window.AudioContext !== "undefined" &&
    typeof window.VideoDecoder !== "undefined"
  );
}

function formatTime(value: number, requestedFps: number): string {
  const safe = Math.max(0, Number.isFinite(value) ? value : 0);
  const fps = requestedFps > 0 ? Math.round(requestedFps) : 30;
  const minutes = Math.floor(safe / 60);
  const seconds = Math.floor(safe % 60);
  const frames = Math.min(fps - 1, Math.floor((safe % 1) * fps));
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}:${String(frames).padStart(2, "0")}`;
}
