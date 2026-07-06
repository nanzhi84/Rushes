# Spec B：对话流式输出 + 助手文本链路补全

日期：2026-07-06 ｜ 状态：已定稿（用户授权免评审） ｜ 实施顺序：三份重构 spec 中第 2 个（A → B → C）

## 背景与目标

现状两大问题：

1. **助手文本根本不显示**：`respond`/`refuse` 的 message 写入 messages 表，但没有 GET 接口，前端 `messagesQuery` 是禁用桩（`CaseAgentConsole.tsx:47-52`）。用户只能看到结构化事件卡片。
2. **零流式**：LLM 调用整段返回（`packages/providers/openai_compatible/llm.py` 无 stream）；turn 内最多 12 步工具执行期间用户面对空白。

目标体验（用户拍板）：**全过程可见 + 文本 token 流**——turn 内每一步（正在调哪个工具、进度）实时进入对话流成为过程条目；助手散文逐字流式。

协议决策（用户拍板）：**content 即散文，退役 respond**——`tool_choice` 从 `"required"` 改为 `"auto"`，模型自然语言走 `content` 字段；纯文本输出（无工具调用）= 回复用户并结束 turn。

## 现状要点（探索结论）

- SSE 已有但本质是 event_log 表的 50ms 轮询尾随（`apps/api/main.py:1450-1476`），只推已落库的离散领域事件，不适合 token 频率。
- turn 在 API 进程内执行（`apps/api/main.py:297-311` `default_runner` → `run_turn`，`TurnQueue` 挂在 ApiState）→ **进程内 pub/sub 可行**。
- planner `tool_choice="required"`（`packages/providers/planner.py:85`），散文藏在 `respond` 工具 message 参数里；`respond`/`refuse`/`finish_turn` 是 harness 内建终止工具（`packages/tools/builtin/handlers.py`）。
- 主循环 `packages/agent_harness/loop.py:125-443`：每轮 planner 一次完整调用返回一个 ToolCall；observation 内存回灌；终止条件挂在内建工具与限流上。
- 前端 `AssistantThread.tsx:92-103` 已有 assistant 气泡渲染能力，缺数据源；`TurnEnded` 已用于解锁输入框（`CaseAgentConsole.tsx:99-101`）。

## 设计

### B1. 协议层：content 即散文

新的单步语义（planner 返回 `content: str | None` + `tool_call: ToolCall | None`）：

| 模型输出 | harness 行为 |
|---|---|
| content + tool_call | content 是**过程叙述**：流式推送 + 落 messages 表（`kind=narration`）；随后照常执行工具 |
| 仅 tool_call | 照常执行工具 |
| 仅 content（非空） | **最终回复**：流式推送 + 落 messages 表（`kind=reply`），发 `TurnEnded`，turn 结束 |
| 双空 | 非法输出，计入现有 `max_illegal_outputs` 重试 |

- 模型单步只取第一个 tool_call（保持 PRD 单工具约束），多余的丢弃并记 trace。
- **退役 `respond`/`refuse`/`finish_turn` 三个内建工具**：从 registry、specs、PolicyGate 特判、PRD §6.11 中移除。拒绝语义就是散文；「无话可说地结束」= 模型输出一句简短 content 不带工具调用。
- messages 表增加 `kind` 列（`user | reply | narration`），带迁移；context_builder 把历史 narration/reply 作为 assistant 消息回灌（narration 参与上下文，压缩规则沿用现有 compaction 策略）。
- PolicyGate 的 deny/ask/defer 路径、decision 等待、`running_jobs`、限流等其余终止条件全部不变。

### B2. Provider/Planner 层：流式调用

- `openai_compatible/llm.py` 新增 `invoke_stream`：httpx `stream()` + SSE 行解析，产出增量事件（content delta、tool_call 参数累积、usage），结束时给出与现有 `invoke` 同构的完整归一化结果。保留非流式 `invoke` 作为降级路径（配置开关 / 供应商不支持 stream 时整段一次性作为单个 delta 下发）。
- `gateway` / `GatewayLLMPlanner` 暴露 `plan_stream`（async generator）：yield `PlannerDelta`（text_delta / tool_call_started …），最终 return 完整 PlannerStep。qwen-max（OpenAI 兼容）支持 content+tool_calls 混合流式。
- 遵循既有网络纪律：httpx 强制 IPv4 + `trust_env=False`（本机 IPv6 连国内 API 会断）。

### B3. 传输层：进程内 turn-stream 通道

- 新增 per-case 内存广播器（asyncio，挂 ApiState）：`run_turn` 全程向其发布：
  - `turn_started` / `turn_ended` / `turn_error`
  - `text_delta {message_id, kind, delta}` / `message_completed {message_id}`
  - `tool_step_started {step_id, tool, label}` / `tool_step_finished {step_id, status, summary}`
  - （Spec C 接入）`subagent_progress {asset_id, note}`
- 新 SSE 端点 `GET /api/projects/{pid}/cases/{cid}/turn-stream`：连接时先回放当前进行中 turn 的缓冲快照（若有），再续实时增量；turn 结束后缓冲清空。断线重连靠快照恢复，不做严格 Last-Event-ID 续传——终态一致性由 messages 表与领域事件 SSE 保证。
- 既有领域事件 SSE（event_log 尾随）保持不变，两通道并存、职责分离：领域事件 = 持久事实；turn-stream = 瞬态过程。

### B4. REST：补齐消息历史

- 新增 `GET /api/projects/{pid}/cases/{cid}/messages`（升序、支持 limit/cursor 简单分页），返回 user/reply/narration 全量。

### B5. 前端

- 启用 `messagesQuery` 接 GET messages：历史回放（narration 用弱化样式，reply 正常气泡）。
- 订阅 turn-stream：
  - `text_delta` 增量渲染「进行中」assistant 气泡（区分 narration/reply 样式）；
  - `tool_step_*` 渲染为过程条目（「正在执行 时间线修改…」→ ✓/✗），复用/扩展 StructuredInteractionRenderer 的条目体系；
  - `turn_ended` 封口气泡 + 触发 messages refetch 校准终态（与既有 TurnEnded 领域事件解锁输入框逻辑合并）。
- 工具名到中文过程文案的映射表（如 `timeline.apply_patch` → 修改时间线）集中定义，未映射的工具显示原名。

### B6. Golden trace 与回放

协议变更使现有 golden trace 全部失效：更新 trace 断言/夹具并**重录 golden cases**，作为本 PR 交付物之一（PRD §4.7 要求维持回放能力）。

## PRD 修订清单（随本 PR 提交）

- §4.1 主循环、§4.2 单轮时序图：planner 输出从「必出一个工具调用」改为「content 和/或 tool_call」，终止语义更新。
- §6.11 内建动作：删除 respond/refuse/finish_turn。
- §4.4 PolicyGate：移除对内建终止工具的特判描述。
- §2.6 / §2.4 Console：补 turn-stream 过程可见性与流式文本的交互描述。
- §7.9 附近补 messages.kind 字段说明。

## 验收标准

1. 发消息后：对话流实时出现过程条目与逐字增长的助手文本；turn 结束后刷新页面，历史消息（含 narration）完整回放。
2. 全仓 grep 无 `respond`/`refuse`/`finish_turn` 内建工具注册；`tool_choice` 无 `"required"`。
3. 断开 turn-stream 重连后能拿到进行中 turn 的快照并续流。
4. 非流式降级路径可用（配置关闭 stream 时功能等价、整段到达）。
5. golden trace 重录完成，回放测试绿。
6. 既有 e2e path drivers（脚本化 planner 以 respond 收尾的）全部迁移到新终止语义（纯 content 收尾）。
7. CI 全绿，e2e 增加「流式回复可见」用例。
