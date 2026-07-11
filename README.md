# Rushes

Rushes 是一个本地优先的对话式视频剪辑 Agent：导入素材后，Agent 会理解画面、生成帧级时间线、渲染预览，并在用户确认后导出 MP4。

本仓库的后端是一次面向生产语义的精简核心重写：**Go 1.26 + CloudWeGo Eino + chi + modernc SQLite + ffmpeg**。保留「导入 → 理解 → 对话编辑 → 预览 → 导出」完整主线，删除被取代的后端与未进入主线的长尾能力。

## 快速开始

需要 Go 1.26、Node.js 24、ffmpeg/ffprobe，以及 pnpm 10.13.1。macOS 可运行：

```bash
brew install go ffmpeg node
make install-web
make dev
```

`make dev` 会构建并拉起 Go API、Go worker 和 Vite，终端会打印带本地访问 token 的 URL。端口默认是 API `8010`、Web `8011`，可用 `RUSHES_API_PORT` / `RUSHES_WEB_PORT` 覆盖。

真实模型可在仓库根目录 `.env` 配置；显式 `export` 的变量优先于 `.env`：

```dotenv
RUSHES_DASHSCOPE_API_KEY=sk-...
RUSHES_QWEN_PLANNER_MODEL=qwen3.7-max
RUSHES_QWEN_CHAT_MODEL=qwen3.7-max
RUSHES_QWEN_VISION_MODEL=qwen3.7-plus
```

没有模型密钥时，本地导入、SQLite、worker、时间线、渲染和 UI 仍可演示，聊天会明确进入无模型降级路径。

## 架构

```text
React / Vite
   │ REST + domain SSE + turn-stream
   ▼
chi API ───────────────► Eino ReAct Agent ─────► 15 个精简工具
   │                         │                         │
   │                         └── TurnQueue / Hub ─────┘
   │
   ├──► Reducer（唯一业务写路径）──► SQLite WAL / event_log / 物化表
   │                                      ▲
   └──► media Range/HEAD                   │ JobSucceeded / JobFailed
                                          │
                              Go worker ───┘
                              claim / lease / ffmpeg
```

目录职责：

- `go/internal/contracts`：21 个领域事件、strict/merge 版本模式与 SSE 路由。
- `go/internal/storage`：纯 Go SQLite、迁移、读模型和对象路径。
- `go/internal/reducer`：事件校验、乐观锁、幂等、物化与侧行同事务提交。
- `go/internal/agent` / `tools` / `providers`：Eino ReAct、TurnQueue、流式协议、Qwen/Ark 适配。
- `go/internal/worker` / `media` / `timeline`：任务租约、ffmpeg 进程、帧级时间线和渲染。
- `go/internal/api`：chi、鉴权、OpenAPI server、三条 SSE、Range/HEAD 媒体端点。
- `apps/web` 与 `e2e`：React 前端和直接指向 Go 后端的 Playwright 主线。

更完整的运行时与不变量说明见 [`docs/architecture.md`](docs/architecture.md)。

## 五个工程设计锚点

### 1. 事件溯源单写路径：乐观锁与幂等并存

所有业务状态只能通过 Reducer 写入。`strict` 事件先校验草稿 `state_version`，提交时再次 CAS；并发编辑冲突返回 `version_conflict`，不会覆盖新状态。`merge` 事件按稳定 merge key 在 `event_log` 去重，worker 重试不会生成重复结果。事件、物化表和 message/material summary 等侧行在同一个 `BEGIN IMMEDIATE` 事务提交，避免“事件成功、读模型缺失”的半状态。

### 2. SQLite 原子 claim 与心跳租约

worker 使用单写连接、WAL、busy timeout 与 `_txlock=immediate`。任务认领由一条条件 `UPDATE` 完成，不依赖进程内锁；`worker_id + heartbeat_at` 构成租约，启动时回收超过 60 秒的 running job。失败按 `min(60, 2^(attempt-1))` 秒退避，`(kind, idempotency_key)` 保证重复入队安全。

### 3. Eino ReAct 与自研 turn-stream

Agent 使用 `flow/agent/react` 和 `utils.InferTool`，注册期检查工具入参是否撞到 PolicyGate 禁止字段，执行前再验证 artifact precondition。HTTP 消息端点只负责 202 入队；每个草稿由 TurnQueue 保序、不同草稿并行。TurnStreamHub 支持当前回合快照重放、8 种帧、慢订阅者淘汰和 context 协作取消，前端断线重连不会丢掉正在生成的回复。

### 4. 自定义 Transport 解决真实网络问题

Qwen 与 Ark 都注入同一个 `http.Client`：`DialContext` 固定 `tcp4`，`Proxy` 显式为 nil，超时统一放在 client。这样避开本机 IPv6 到国内模型端点的 TLS reset，也不会意外继承系统代理。规划、聊天、视觉三档模型分别使用 60/60/180 秒超时。

### 5. ffmpeg 进程组、取消与机器可读进度

所有媒体任务从统一执行层启动。ffmpeg 运行在独立进程组，取消时向整个进程组发 SIGINT，让 MP4 有机会写完 moov；`WaitDelay` 防止子孙进程握住管道导致永久等待。进度来自 `-progress pipe:1` 的 `out_time_us/progress`，不解析易漂移的 stderr 文案。预览还保存渲染时宽高、FPS、时长快照，后续自检不会拿新时间线误判旧成片。

## 开发与验收

```bash
make contracts   # 禁止旧 Python 后端回流 + OpenAPI/SSE 契约零漂移
make test        # Go 全量 -race（含 macOS/Linux 语义）
make coverage    # 手写 Go 核心总覆盖率 >= 90%
make lint        # go vet + golangci-lint/depguard
make web         # TypeScript + Vitest + production build
make e2e         # Go API/worker 主线 Playwright
```

真实 provider spike 默认跳过；提供密钥后可强制执行：

```bash
cd go
RUSHES_REQUIRE_LIVE_MODELS=1 \
RUSHES_DASHSCOPE_API_KEY=... \
RUSHES_ARK_API_KEY=... \
RUSHES_ARK_MODEL=... \
go test -tags=integration ./spikes -run 'TestQwen|TestArk' -v
```

CI 在 Ubuntu 与 macOS 上执行 Go `-race`，并运行契约对拍、90% 覆盖率、golangci-lint、govulncheck、前端三连和 Playwright。
