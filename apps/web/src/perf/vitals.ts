// 开发期采集核心 Web Vitals（INP/LCP/CLS）并打到 console，作为本地交互性能基准，
// 便于量化 F1/F2 前后收益。不上传任何数据、不接任何后端。
//
// 生产构建下 import.meta.env.DEV 为常量 false，整个动态 import 分支被 tree-shake，
// web-vitals 不进入产物（零 bundle 与运行时开销）。

type VitalMetric = { name: string; value: number; rating: string };

export function initWebVitals(): void {
  if (!import.meta.env.DEV) {
    return;
  }
  void import("web-vitals")
    .then(({ onINP, onLCP, onCLS }) => {
      const report = (metric: VitalMetric): void => {
        // 毫秒指标取整；CLS 为无量纲小数，保留三位。
        const value = metric.name === "CLS" ? metric.value.toFixed(3) : Math.round(metric.value);
        // eslint-disable-next-line no-console
        console.info(`[web-vitals] ${metric.name}=${value} (${metric.rating})`);
      };
      onINP(report);
      onLCP(report);
      onCLS(report);
    })
    .catch(() => {
      // web-vitals 未安装或加载失败时忽略，绝不影响应用启动。
    });
}
