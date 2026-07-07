# e2e — Playwright 全栈 E2E（进 CI）

- 独立的 Playwright 包，**不复用 `apps/web` 的 lockfile / 测试配置**。pnpm 版本同样固定 `pnpm@10.13.1`。跑：`pnpm --dir e2e exec playwright test`（本机需 `ffmpeg`/`ffprobe`）。
- **`global-setup.ts` 每次清空重建仓库根的 `.e2e-workspace/`**，再拉起三个本地进程：API `127.0.0.1:18000`（`uvicorn apps.api.main:create_app_from_env --factory`）、worker（绑同一 workspace）、web dev server `127.0.0.1:15173`（经 `RUSHES_WEB_PROXY_TARGET` 代理到 API）。token 固定 `e2e-token`。
- **无 LLM key 时纯降级**：`global-setup` 不设 `RUSHES_LLM_*`，于是 `create_app` 的 `_planner_from_env` 返回 None，turn runner 退化为空 `ScriptedPlanner([])`（只走 content，不做真实规划）。所以这套 E2E 覆盖的是**不依赖真实模型**的路径（当前 PRD §17-M9 路径 3：Project/Case 管理、素材导入、seed 已导出态、移动后素材链接保持）。
- 需要**真实 LLM/VLM** 的端到端验收在 `scripts/e2e_paths/`（另一套，不进 CI）。
