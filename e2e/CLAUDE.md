# e2e — Playwright 全栈验收

- 独立 pnpm 包，固定 pnpm 10.13.1；运行 `pnpm --dir e2e exec playwright test`。
- `global-setup.ts` 每次重建隔离的 `.playwright-workspace`，编译并启动 Go API `18001`、Go worker、Vite `15174`，不依赖也不清理手工测试环境。
- fixture 必须由本地 ffmpeg 确定性生成；测试不得访问真实 provider，也不得读取或输出 `.env` 密钥。
- 三条主线分别验证：完整导入到最终导出、无密钥流式对话、理解 N/M 中途取消。
- 失败后先检查 `.playwright-workspace/logs/{api,worker,web}.log` 和 Playwright trace。
