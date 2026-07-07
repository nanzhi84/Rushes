# packages/agent_harness — 单回合主循环（写路径）

- **`loop.py` 的 `run_turn` 是回合主循环。** 每步向 planner 要一个 `PlannerStep`，其 `content` 就是**散文协议**：
  - 只有 `content` → 助手回复，回合结束；
  - `content + tool_call` → 台词落库、工具执行、回合继续；
  - 只有 `tool_call` → 静默工具步；
  - 两者都无 → 非法输出（计入 `illegal_output_limit`）。
- **`policy_gate.py` 的 `PolicyGate` 是工具前裁决**：先剪枝（`_PROHIBITED_ARGUMENT_KEY_*` 拦截 LLM 往参数塞帧号/时间码/编码/路径等低层字段），再给 `deny / ask / defer / allow` 四态裁决；`ask`/`defer` 会造 `DecisionCreated` 等事件。
- **唯一写路径是 `reducer.py` 的 `apply`**：领域事件经它落 `event_log` + 物化表，别处不许直接写业务表。新事件要同时进 `REDUCER_DISPATCH_EVENTS` 和 `contracts.events` 注册表（`check_contracts.py` 卡一致性）。
- **`TurnListener`（`loop.py` 里的 duck-typed Protocol）** 是进程内 turn 事件出口，`emit` 同步且**绝不能把回合搞崩**（loop 每次调用都 `suppress(Exception)`）。具体 hub 实现在 apps 侧（`apps/api/turn_stream.py`）。工具进度经 `metadata["turn_progress"]` 回调转成 `subagent_progress` 事件。
- 其它构件：`context_builder`（拼上下文块）、`compaction`（历史压缩）、`tool_router` / `decision_answering` / `turn_queue`（串行化回合）、`state_validator`（提交前时间线不变量）。
- **golden 回放**在 `tests/golden/`：用 `ScriptedPlanner` / `MockProvider` 固定脚本驱动 `run_turn`，断言工具轨迹与事件序列——协议级回归网，改主循环先看它会不会红。
