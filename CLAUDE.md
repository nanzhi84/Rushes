# Rushes

Chat-first 的本地视频剪辑 Agent：把一堆素材「聊」成一条成片，工作区就是一个本地 SQLite。

- **`PRD.md`（仓库根）是唯一实现依据。** 需求/语义/阶段划分一律以它为准，本文件只讲工程约定，不复述 PRD 内容。
- 面向用户的文案、报错、Agent 台词**一律简体中文**。

## 常用命令

```bash
uv run pytest -q                       # 全量测试；覆盖率门槛 90%（pyproject --cov-fail-under=90）
uv run ruff check                      # lint
uv run ruff format --check             # 格式
uv run mypy                            # 严格类型（files 已在 pyproject 配好，直接跑）
uv run python scripts/check_contracts.py   # 导入边界 + 事件/工具注册表一致性

# web（pnpm 版本用 npx 固定为 10.13.1）
npx -y pnpm@10.13.1 --dir apps/web typecheck
npx -y pnpm@10.13.1 --dir apps/web test -- --run
npx -y pnpm@10.13.1 --dir apps/web build
bash scripts/gen_web_types.sh          # 改过 API 后重新生成前端类型（见 apps/web）

pnpm --dir e2e exec playwright test    # 全栈 E2E（另见 e2e/ 与 scripts/e2e_paths/）
```

## 架构一图流

```
apps/{api,worker,web}                 # api=FastAPI 单进程 / worker=SQLite job 轮询 / web=Vite+React
  └── packages（分层，导入方向由 scripts/check_contracts.py 强制）
contracts        # 纯数据契约，谁都可依赖，自己不依赖别人
domain    → contracts
storage   → contracts
events    → storage, contracts
media     → storage, contracts        # 纯本地媒体，禁止 import providers
providers → contracts                 # 云端 LLM/VLM/ASR/TTS 网关
timeline  → (未纳入 check_contracts 的边界组)  # 帧级时间线，仅供 tools 调用
tools     → domain, storage, events, media, providers, timeline, contracts  # 唯一同时够到 media/providers/timeline 的聚合层
agent_harness → tools, domain, storage, events, contracts   # 主循环写路径
apps      → 以上全部
```

导入边界的权威定义是 `scripts/check_contracts.py` 的 `ALLOWED_IMPORTS`；改分层先改它。

## 关键约定

- **事件溯源，单写路径**：所有状态变更都是「领域事件 →（`packages/agent_harness/reducer.py` 的 `apply`）→ event_log + 物化表」。别的地方**不许**直接写业务表。
- **httpx 连国内 API 必须强制 IPv4 + `trust_env=False`**：本机 IPv6 会 TLS reset。见 `packages/providers/{aliyun,volcengine,openai_compatible}`（`local_address="0.0.0.0"`）。
- 新增领域事件要同时进 `contracts.events` 注册表和 reducer dispatch；新增工具要 spec+handler 配对、`requires_artifacts` 谓词进 `PRECONDITION_REGISTRY`——两者都由 check_contracts 卡。
- 子目录另有各自的 `CLAUDE.md`，触碰对应代码时会自动加载。
