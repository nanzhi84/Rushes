// 关键交互性能标记（User Timing API）。仅开发期写入，DevTools Performance 面板可直接
// 观察 token→paint、拖拽、缩放、seek、rAF 回调等区间耗时，作为 F1/F2 优化前后的本地对拍
// 依据；不上传任何数据。生产构建下 import.meta.env.DEV 为常量 false，下列导出被替换为
// 空实现，调用点只剩一次 noop 调用，实际零开销（且 web-vitals 相关分支被 tree-shake）。

const noop = (): void => {};

const canMark =
  import.meta.env.DEV &&
  typeof performance !== "undefined" &&
  typeof performance.mark === "function";

/** 标记一个区间的开始。name 建议用 "domain:action" 形式，如 "timeline:zoom"。 */
export const markStart: (name: string) => void = canMark
  ? (name) => {
      try {
        performance.mark(`${name}:start`);
      } catch {
        // User Timing 缓冲不可用时静默忽略，绝不影响交互。
      }
    }
  : noop;

/** 标记区间结束并生成一条 measure（缺少起始标记时静默忽略）。 */
export const markEnd: (name: string) => void = canMark
  ? (name) => {
      try {
        performance.mark(`${name}:end`);
        performance.measure(name, `${name}:start`, `${name}:end`);
      } catch {
        // 起始标记缺失或缓冲不可用时忽略。
      }
    }
  : noop;

/** 便捷区间：返回结束函数，用于包裹一次性区间（如单帧 rAF 回调）。 */
export const perfSpan: (name: string) => () => void = canMark
  ? (name) => {
      markStart(name);
      return () => markEnd(name);
    }
  : () => noop;
