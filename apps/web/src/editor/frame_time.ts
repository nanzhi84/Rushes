// Rushes 的持久时间基始终是整数帧；Diffusion Core 接受 `123f` 格式，
// 因此适配层无需把时间线转换为秒或微秒。
export function frameTime(frame: number): `${number}f` {
  return `${Math.max(0, Math.round(frame))}f`;
}

// Diffusion 的 AudioClip/VideoClip 把可见起点定义为 delay + range[0]。
// 当时间线片段取自素材中段时，内部 delay 必须允许为负数，才能让片段仍从
// timeline_start_frame 出现；这个有符号偏移只存在于预览适配层，不会写回时间线。
export function frameOffsetTime(frame: number): `${number}f` {
  return `${Number.isFinite(frame) ? Math.round(frame) : 0}f`;
}
