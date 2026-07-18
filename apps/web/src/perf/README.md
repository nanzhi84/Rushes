# 前端渲染性能度量与回归基准（#95 F6）

本目录是前端渲染性能的度量基建与本地回归基准，配合 F1（memo 边界）/F2（时间线窗口化）
量化收益。**不上传任何数据**，只在开发期向 console / DevTools 输出。

## 组成

- `vitals.ts` — 开发期采集核心 Web Vitals（INP / LCP / CLS）打到 console。
  由 `main.tsx` 的 `initWebVitals()` 启动。生产构建下整段动态 import 被 tree-shake，
  web-vitals 不进入产物。
- `marks.ts` — 关键交互的 User Timing 区间标记，DevTools Performance 面板可直接观察：
  - `timeline:zoom` — Ctrl/⌘ 滚轮缩放
  - `timeline:seek` — 点击/拖动播放头换算并回传
  - `timeline:clip-drag` — 拖拽片段每帧的预览计算
  - `timeline:scroll-window` — 滚动窗口化的 rAF 回调耗时

  生产构建下 `import.meta.env.DEV` 为常量 false，导出被替换为 noop，零开销。
- `../test/fixtures/stressDraft.ts` — 压力草稿造数：300 clip（7 轨、4 音轨）+ 长口播
  波形 peaks + 500 条对话消息。确定性伪随机，跨运行稳定。

## 自动回归

`marks.test.ts` 与 `stressDraft.perf.test.tsx` 随 `make web`（vitest）常态执行，
断言压力草稿窗口化后 DOM 节点从数千（>2000）降到低百位（<500）。

## 手动 profiling（观察交互指标）

1. `pnpm --dir apps/web dev` 启动，打开 DevTools → Performance。
2. 临时 scratch 组件挂载压力草稿（用完即删，勿提交进路由）：

   ```tsx
   import { makeStressTimeline } from "@/test/fixtures/stressDraft";
   import { TimelineViewer } from "@/components/TimelineViewer";
   // <TimelineViewer timeline={makeStressTimeline()} pxPerSec={96} onSeek={() => {}} />
   ```

3. 录制期间缩放 / 拖拽 / 滚动 / seek，在 Performance 的 “Timings” 轨查看
   `timeline:*` 区间耗时；同时观察 console 的 `[web-vitals]` 输出（INP 反映交互跟手度）。

## Bundle 预算

`scripts/check-bundle-budget.mjs` 在 `vite build` 后校验主入口 chunk 的 gzip 体积
≤ 350 kB（`make web` 与 CI 均执行，超预算即失败）。降体积走路由级 code-split /
manualChunks / 懒加载（F4/F5），不要放宽预算。
