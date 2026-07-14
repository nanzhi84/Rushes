# Rushes Playwright E2E

本目录直接验收 Go API、Go worker 与 React 前端，不使用真实模型密钥。

```bash
pnpm --dir apps/web install --frozen-lockfile
pnpm --dir e2e install --frozen-lockfile
pnpm --dir e2e exec playwright install chromium
pnpm --dir e2e exec playwright test
```

本机还需 `go`、`ffmpeg` 与 `ffprobe`。`global-setup.ts` 会重建隔离的 `.playwright-workspace`、生成确定性视频 fixture、编译两个 Go 二进制并启动，不会清理手工测试环境：

- API：`127.0.0.1:18001`
- worker：共享 `.playwright-workspace/rushes.db`
- web：`127.0.0.1:15174`

可通过 `RUSHES_E2E_WORKSPACE`、`RUSHES_E2E_API_PORT` 和 `RUSHES_E2E_WEB_PORT` 覆盖默认值。

覆盖范围：

- `path3-draft-materials.spec.ts`：导入 → ingest/理解 → 时间线 → 预览 → 决策确认 → 720×1280 最终导出 → 跨草稿秒链去重。
- `streaming-console.spec.ts`：无密钥降级回复仍按 turn-stream 流式显示并正确结束回合。
- `understanding-progress-cancel.spec.ts`：素材理解不显示独立顶栏状态，通过停止整轮协作取消；已完成摘要保留，未开始素材回到可重试状态。

失败产物保存在 `.playwright-workspace/logs`、`e2e/test-results` 和 `e2e/playwright-report`。
