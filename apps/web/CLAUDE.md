# apps/web — Vite + React 前端

- 栈：Vite 7 + React 19 + `@tanstack/react-router` + `@tanstack/react-query` + zustand，测试用 vitest（jsdom）。
- **pnpm 版本经 `npx` 固定**：`packageManager: "pnpm@10.13.1"`。命令一律 `npx -y pnpm@10.13.1 --dir apps/web <script>`（`typecheck` / `test -- --run` / `build`）。
- **API 类型是生成产物**：`scripts/gen_web_types.sh` 从 `apps.api.main.create_app` 导出 OpenAPI → `openapi-typescript` → `src/api/generated/schema.d.ts`。**改了 API 的请求/响应模型后必须重新生成**（`npx -y pnpm@10.13.1 --dir apps/web gen:types` 或直接跑脚本），否则 `src/api/client.ts` 类型对不上。
- 结构：`src/routes/`（页面 + 同名 `.test.tsx`：首页卡片墙 `ProjectsOverview` / 项目详情 `ProjectDetailPage` / 工作台 `CaseAgentConsole`）、`src/components/`（`Shell`（TopBar/EntityActionDialog/ResizeHandle）/ `Console` / `Materials`（AssetsPanel/FsBrowserDialog）/ `PreviewPlayer` / `TimelineViewer`）、`src/api/client.ts`（类型化 fetch 封装）、`src/auth.ts`、`src/state/`。深色设计令牌集中在 `src/index.css` 的 `@theme`，组件不写裸色值。
- 鉴权对齐后端：普通请求带 Bearer token；SSE 与媒体流用 `?token=`（浏览器 header 限制，见 apps/api）。TimelineViewer 只读（不做时间线编辑交互）。
