type ResumableAudioClock = {
  readonly state: string;
  resume: () => Promise<void>;
};

// Diffusion Core 4.0.3 会触发 AudioContext.resume()，但没有等待它完成就把
// renderer 标记为 playing。浏览器首次授权音频时，currentTime 因而可能一直停在
// 0。适配层先显式等待时钟进入 running，再开始组合播放。
export async function resumePreviewClock(
  clock: ResumableAudioClock,
  timeoutMs = 3_000
): Promise<void> {
  if (clock.state === "suspended") {
    let timeout: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([
        clock.resume(),
        new Promise<never>((_resolve, reject) => {
          timeout = setTimeout(
            () => reject(new Error("浏览器未允许启动音频预览时钟")),
            timeoutMs
          );
        })
      ]);
    } finally {
      if (timeout !== undefined) {
        clearTimeout(timeout);
      }
    }
  }
  if (clock.state !== "running") {
    throw new Error(`音频预览时钟状态异常: ${clock.state}`);
  }
}
