import type { KeyframeOptions, Time } from "@diffusionstudio/core";
import { frameTime } from "./frame_time";

export function videoFadeAnimation(
  durationFrames: number,
  fadeInFrames: number,
  fadeOutFrames: number
): KeyframeOptions<"opacity", number> | null {
  const duration = Math.max(0, Math.round(durationFrames));
  const fadeIn = Math.min(duration, Math.max(0, Math.round(fadeInFrames)));
  const fadeOut = Math.min(duration - fadeIn, Math.max(0, Math.round(fadeOutFrames)));
  if (duration === 0 || (fadeIn === 0 && fadeOut === 0)) {
    return null;
  }
  const frames: Array<{ time: Time; value: number }> = [];
  if (fadeIn > 0) {
    frames.push({ time: frameTime(0), value: 0 }, { time: frameTime(fadeIn), value: 100 });
  } else {
    frames.push({ time: frameTime(0), value: 100 });
  }
  const fadeOutStart = duration - fadeOut;
  if (fadeOut > 0) {
    if (fadeOutStart > fadeIn) {
      frames.push({ time: frameTime(fadeOutStart), value: 100 });
    }
    frames.push({ time: frameTime(duration), value: 0 });
  } else if (frames.at(-1)?.time !== frameTime(duration)) {
    frames.push({ time: frameTime(duration), value: 100 });
  }
  return { key: "opacity", frames };
}
