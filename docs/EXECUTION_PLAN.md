# Rushes M0 执行计划（agent harness）

依据：`chat_first_editing_agent_prd_v1_2.md`（唯一实现依据）。本文件只记录交付切分与工作纪律，不复述 PRD 内容；冲突时以 PRD 为准。

## 分工

- Claude：规划、任务拆分、代码验收、PR 管理。
- Codex（gpt-5.5 / xhigh / fast）：全部代码实现。
- 每阶段一个 PR，CI 必须绿（ruff + mypy + pytest，覆盖率 ≥90%）才可合并。

## PR 切分（M0 = Contracts + Harness 骨架，PRD §17-M0 / §19.1）

| PR | 分支 | 交付 | PRD 依据 |
|---|---|---|---|
| 00 | `feat/m0-00-scaffold` | uv 工程、ruff/mypy/pytest 配置、GitHub Actions CI、覆盖率门禁 | §14.1 / §15 |
| 01 | `feat/m0-01-contracts` | packages/contracts 全量 schema + 事件判别联合 + ToolSpec/PatchOpSpec/ProviderDescriptor | §7 / §4.5 / §16 |
| 02 | `feat/m0-02-storage` | SQLite WAL + Alembic 迁移 + repositories + CAS 对象存储 + event_log | §3.2 / §3.7 / §14.3 |
| 03 | `feat/m0-03-reducer` | Reducer（strict/merge 权威事件表 + decision_effects）+ StateValidator | §4.5 / §4.6 / §7.6.1 |
| 04 | `feat/m0-04-policygate` | domain 谓词 / case_stage + PolicyGate（四值裁决 + PendingToolCall outbox）+ ContextBuilder | §4.3 / §4.4 / §5.2 / §5.3 |
| 05 | `feat/m0-05-loop` | ToolRegistry/Router + 内建 4 + interaction 6 工具 + Turn Queue + 主循环 + AgentTrace | §4.1 / §4.9 / §6 / §16.1 |
| 06 | `feat/m0-06-jobs-golden` | Job Runner（claim 模式）+ golden 回放框架 + 前 3 个 golden case + check_contracts.py | §4.7 / §4.10 / §14.3 |
| 07 | `feat/m0-07-api-sse` | FastAPI 骨架 + §13.0 安全基线 + SSE Last-Event-ID 回放 → M0 验收 Gherkin 全绿 | §13 / §17-M0 |

M0 验收后按 §17 顺序推进 M1+（Tree/Project/Case UI 起）。M-1 POC（云端 ASR 口癖契约、端到端画质、TTS 时间戳）需要真实 API key 与真实素材，与 harness 构建并行、由用户配合时执行。

## 工作纪律

- 实现全部发生在 `.claude/worktrees/<slug>` 独立 worktree，分支 `feat/<slug>`，PR 合入 `main`。
- 依赖方向单向（PRD §15），`scripts/check_contracts.py` 静态断言（PR-06 起 CI 强制）。
- 新增 DomainEvent 必须同步登记权威事件表；工具只经 registry 暴露，禁止 `if tool_name` 分支。
- 覆盖率统计范围：`packages/`（后续加入 `apps/`）；`--cov-fail-under=90`。
