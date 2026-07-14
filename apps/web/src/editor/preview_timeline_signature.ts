import type { TimelineJson } from "../api/client";

// 内容签名刻意覆盖完整时间线，而不只覆盖 clip id。相同内容的查询刷新会
// 直接 no-op；时间、音量、淡入淡出、字幕等任一真实变更仍会进入增量更新。
export function timelineRuntimeSignature(timeline: TimelineJson): string {
  return JSON.stringify(timeline);
}
