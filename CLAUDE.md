# Rushes

本地优先的对话式视频剪辑 Agent。后端是 Go 1.26 + Eino + chi + modernc SQLite，前端是 React 19 + Vite 7。

- 面向用户的文案、错误与 Agent 台词一律使用简体中文。
- 根目录 `.env` 是所有本地手工、开发和 E2E 流程的统一配置源；显式 export 的同名变量优先，任何日志和测试输出都不得泄漏密钥。
- **供用户手测的本地环境必须加载仓库根 `.env`**：无论通过 dev、test、E2E、tmux 还是手工命令启动，都要确保 API 和 worker 在启动前按 `scripts/dev_all.sh` 与 `go/internal/config.LoadDotEnv` 的语义加载 `.env`（已显式 export 的变量优先，`.env` 只补未设置项）。不要把后端密钥注入 Web；确定性的自动化测试若必须禁用真实 provider，应显式覆盖/清空并说明。交付前检查运行中 API/worker 已继承所需变量并完成健康检查，但绝不输出密钥值。

## 常用命令

```bash
make dev                              # Go API + worker + web
make contracts                        # OpenAPI 与两套 SSE golden 对拍
make test                             # cd go && go test -race ./...
make coverage                         # 手写 Go 核心覆盖率 >= 90%
make lint                             # go vet + golangci-lint/depguard
make web                              # typecheck + vitest + build
make e2e                              # Playwright 指向 Go 后端
```

真实 provider 测试带 `integration` build tag，默认必须跳过；只有配置真实密钥并明确要求时才用 `RUSHES_REQUIRE_LIVE_MODELS=1` 强制运行。

## 写路径与分层

- `go/internal/reducer` 是唯一业务写路径；事件、物化状态和 ResultRows 必须在同一个 immediate transaction 内提交。
- `go/internal/contracts` 的 EventRegistry 是事件事实源；新增事件必须同时实现校验、Reducer apply、SSE routing 与测试。
- `go/internal/tools` 的 registry 是工具事实源；LLM 工具必须经过 precondition、PolicyGate 字段检查和 Agent 统一执行入口。
- `go/internal/worker` 只通过 claim/heartbeat/retry 协议处理 job，终态继续走 Reducer。
- `go/.golangci.yml` 的 depguard 是依赖方向权威；不要通过中间包或相对导入绕过。
- `apps/web/openapi.json` 是冻结 HTTP 契约；修改后运行 `make contracts`，生成物必须零 diff。
