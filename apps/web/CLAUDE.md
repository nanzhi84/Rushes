# apps/web — Vite + React

- 栈：Vite 7、React 19、TanStack Router/Query、zustand、Vitest/jsdom。
- pnpm 固定为 10.13.1；运行 `npx -y pnpm@10.13.1 --dir apps/web <script>`。
- `apps/web/openapi.json` 是冻结契约，`src/api/generated/schema.d.ts` 是生成物。API 变化后运行 `make contracts`，不得手改生成文件。
- 领域事件名必须来自 `src/api/event_types.ts`，与 `go/internal/contracts.EventRegistry` 对齐；回合结束只看独立 turn-stream 的 `turn_ended/turn_error`，不要伪造领域事件。
- 普通请求使用 Bearer token；SSE 和媒体 GET/HEAD 使用 query token。TimelineViewer 的人工编辑必须调用 timeline patch API，并继续经 AgentService / Reducer 落库，禁止只改前端本地状态。
- 工作台设计令牌集中在 `src/index.css` 的 `@theme`，组件不要散落裸色值。
