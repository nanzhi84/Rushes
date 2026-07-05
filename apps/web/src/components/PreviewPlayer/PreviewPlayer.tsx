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
};

export function PreviewPlayer({ src, fps, onFirstPlay }: PreviewPlayerProps): ReactElement {
  return (
    <div className="overflow-hidden rounded-lg border border-[#d9dee7] bg-[#0f172a] text-white">
      <MediaPlayer
        src={src}
        playsInline
        className="block aspect-[9/16] w-full bg-black"
        aria-label="预览播放器"
      >
        <MediaProvider />
        <PreviewPlayerControls fps={fps} onFirstPlay={onFirstPlay} />
      </MediaPlayer>
    </div>
  );
}

function PreviewPlayerControls({
  fps,
  onFirstPlay
}: {
  fps: number;
  onFirstPlay?: () => void;
}): ReactElement {
  const currentTime = useMediaState("currentTime");
  const playing = useMediaState("playing");
  const paused = useMediaState("paused");
  const remote = useMediaRemote();
  const firstPlayReportedRef = useRef(false);
  const safeFps = useMemo(() => (fps > 0 ? fps : 30), [fps]);
  const currentFrame = Math.round(currentTime * safeFps);

  useEffect(() => {
    if (!playing || firstPlayReportedRef.current) {
      return;
    }
    firstPlayReportedRef.current = true;
    onFirstPlay?.();
  }, [onFirstPlay, playing]);

  const stepFrame = useCallback(
    (direction: -1 | 1, event: MouseEvent<HTMLButtonElement>) => {
      const nextTime = Math.max(0, currentTime + direction / safeFps);
      remote.seek(nextTime, event.nativeEvent);
    },
    [currentTime, remote, safeFps]
  );

  return (
    <div className="flex items-center justify-between gap-3 border-t border-white/10 bg-[#17202a] px-3 py-2">
      <div className="text-xs text-[#cbd5e1]">
        当前帧 <span className="font-mono text-white">{currentFrame}</span>
      </div>
      <div className="flex items-center gap-2">
        <button
          type="button"
          className="rounded-md bg-white px-3 py-1.5 text-sm font-medium text-[#17202a] hover:bg-[#e2e8f0] focus:outline-none focus:ring-2 focus:ring-white/40"
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
          className="rounded-md border border-white/15 px-3 py-1.5 text-sm font-medium text-white hover:bg-white/10 focus:outline-none focus:ring-2 focus:ring-white/40"
          aria-label="后退一帧"
          onClick={(event) => stepFrame(-1, event)}
        >
          -1 帧
        </button>
        <button
          type="button"
          className="rounded-md border border-white/15 px-3 py-1.5 text-sm font-medium text-white hover:bg-white/10 focus:outline-none focus:ring-2 focus:ring-white/40"
          aria-label="前进一帧"
          onClick={(event) => stepFrame(1, event)}
        >
          +1 帧
        </button>
      </div>
    </div>
  );
}
