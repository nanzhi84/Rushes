import { useEffect, useState } from "react";

/** 运行态本地计时：真实事件间隙也每秒刷新，避免长步骤看起来像卡死。 */
export function useElapsedSeconds(active: boolean): number {
  const [elapsedSeconds, setElapsedSeconds] = useState(0);

  useEffect(() => {
    if (!active) {
      setElapsedSeconds(0);
      return;
    }
    const startedAt = Date.now();
    setElapsedSeconds(0);
    const timer = window.setInterval(() => {
      setElapsedSeconds(Math.max(0, Math.floor((Date.now() - startedAt) / 1000)));
    }, 1_000);
    return () => window.clearInterval(timer);
  }, [active]);

  return elapsedSeconds;
}

export function formatElapsedTime(totalSeconds: number): string {
  const safeSeconds = Math.max(0, Math.floor(totalSeconds));
  const seconds = safeSeconds % 60;
  const totalMinutes = Math.floor(safeSeconds / 60);
  const minutes = totalMinutes % 60;
  const hours = Math.floor(totalMinutes / 60);
  if (hours > 0) {
    return `${hours}:${padTime(minutes)}:${padTime(seconds)}`;
  }
  return `${padTime(minutes)}:${padTime(seconds)}`;
}

function padTime(value: number): string {
  return String(value).padStart(2, "0");
}
