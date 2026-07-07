# Spec B 实施计划：对话流式输出 + 助手文本链路补全

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** content 即散文协议（退役 respond/refuse/finish_turn）、LLM 流式调用、进程内 turn-stream 通道、前端消息历史回放 + token 级流式渲染 + 工具过程条目。

**Architecture:** 自底向上四层：协议层（PlannerStep：content+tool_call 二元组，纯文本=回复并结束 turn）→ provider/gateway 流式（httpx SSE + on_delta 回调，非流式可降级）→ 传输层（ApiState 上的进程内 TurnStreamHub + 新 SSE 端点，与既有 event_log 轮询 SSE 并存）→ 前端（GET /messages 历史 + turn-stream 增量气泡 + 工具过程条目）。

**Tech Stack:** Python 3.12 + FastAPI + SQLAlchemy Core + httpx + pydantic v2（`uv run`）；React + TypeScript + @tanstack/react-query + vitest（`npx -y pnpm@10.13.1 --dir apps/web`）。

**工作目录：** `/Users/yoryon/Projects/Rushes/.worktrees/refactor-b`（分支 `refactor/b-streaming-console`）

## Global Constraints

- Spec 全文：`docs/superpowers/specs/2026-07-06-b-streaming-console-design.md`（验收标准 7 条为准）。
- 单步语义表（协议核心，所有任务对齐）：content+tool_call → content 是过程叙述（narration，流式+落库）随后执行工具；仅 tool_call → 照常执行；仅 content（非空）→ 最终回复（reply，落库+TurnEnded+turn 结束）；双空 → 非法输出计入 `max_illegal_outputs`。多个 tool_calls 只取第一个，丢弃记 trace。
- messages 表新增 `kind` 列：`user | reply | narration`（user 行 kind="user"）。
- 退役 `respond`/`refuse`/`finish_turn`：从 specs.py、registry、policy_gate 特判、PRD §6.11 移除；`decision_answer` 内建保留。
- httpx 一律 `trust_env=False` + IPv4 transport（沿用 llm.py 现状，流式客户端同样处理）。
- PolicyGate deny/ask/defer、decision 等待、running_jobs、限流等其余终止条件语义不变。
- 面向用户文案简体中文；每任务提交前相应检查绿（Python：ruff check/format+mypy；前端：typecheck）。
- 提交信息中文 + 两行尾注（Co-Authored-By: Claude Fable 5 <noreply@anthropic.com> / Claude-Session: https://claude.ai/code/session_01FJ3rEvooREoJUdT1DbCFH6）。
- 基线：pytest 535 passed（覆盖率门槛 90%）、web 32 passed。

---

### Task 1: messages.kind 列 + 消息历史 GET 端点

**Files:**
- Modify: `packages/storage/schema.py:246-254`（messages 表加列）
- Modify: `packages/storage/data_migrations.py`（幂等 ALTER）
- Modify: `packages/storage/repositories/messages.py`（list_for_case 已存在，加 limit 参数即可）
- Modify: `apps/api/main.py`（新 GET 端点；`enqueue_message` 写入 user 行补 kind）
- Test: `tests/storage/test_data_migrations.py`、`tests/api/` 新增消息端点用例

**Interfaces:**
- Produces: `schema.messages` 新列 `Column("kind", Text, nullable=False, server_default="reply")`；`apply_data_migrations` 内新增 `_ensure_message_kind_column`（`PRAGMA table_info(messages)` 查无 kind 则 `ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'reply'`，并 `UPDATE messages SET kind='user' WHERE role='user'`）；`GET /api/projects/{project_id}/cases/{case_id}/messages?limit=200` → `{"case_id", "messages": [{message_id, role, kind, content, created_at}]}` 升序。Task 2 落库 narration/reply、Task 7 前端消费。

- [ ] **Step 1:** 迁移测试先行（老库无 kind 列 → 迁移后有且 user 行 kind='user'；重复执行幂等）→ RED
- [ ] **Step 2:** schema 加列 + data_migrations 实现 → GREEN
- [ ] **Step 3:** API 端点测试先行（插入 user/reply/narration 三行断言升序与字段完整；不存在的 case 404）→ RED → 实现端点（`_require_project` 同款风格校验 case 归属）+ `enqueue_message` 的 message_row 补 `"kind": "user"` → GREEN
- [ ] **Step 4:** 全量 pytest + ruff/format + mypy → Commit `feat(storage,api): messages.kind 列与消息历史端点`

### Task 2: PlannerStep 协议 + 主循环终止语义改造 + 退役内建工具

**Files:**
- Modify: `packages/agent_harness/loop.py`（:57 LLMPlanner 协议、:66 ScriptedPlanner、:205-443 主循环、`_force_respond`/`_force_finish` → `_force_reply`）
- Modify: `packages/agent_harness/policy_gate.py:473`（respond 特判删除）
- Modify: `packages/tools/specs.py:462-499,1451,1496-1498`（三内建 spec/handler 移除，decision_answer 保留）
- Modify: `packages/tools/builtin/handlers.py`（删 respond/refuse/finish_turn 函数及其 Input 模型引用）
- Modify: `packages/agent_harness/__init__.py`（导出同步）
- Test: `tests/agent_harness/`（loop 测试全面迁移）、`tests/tools/`（specs 计数/注册断言）

**Interfaces:**
- Produces（Task 4/5 消费，签名固定）:

```python
# packages/agent_harness/loop.py
@dataclass(frozen=True, slots=True)
class PlannerStep:
    content: str | None = None
    tool_call: ToolCall | None = None

class LLMPlanner(Protocol):
    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,   # 收 content 增量
    ) -> PlannerStep: ...
```

- ScriptedPlanner 脚本项扩展：既有 `ToolCall | Mapping`（视为纯工具步）之外，允许 `{"content": "...", "tool_call": {...}?}` 形式；`plan` 返回 PlannerStep；脚本耗尽时返回 `PlannerStep(content="（脚本耗尽，结束本回合）")`（原 finish_turn 兜底改为纯文本回复）。
- 主循环改造（伪码，实施对照 :238-432）：

```python
step = replay_step or await planner.plan(context_bundle, context_bundle.allowed_tools, on_delta=...)
if step.content:
    kind = "narration" if step.tool_call is not None else "reply"
    _persist_assistant_message(engine, loaded, turn_id, step.content, kind)
if step.tool_call is None:
    if not step.content:
        illegal_outputs += 1  # 双空，复用现有 deny 分支的 illegal 计数与 force 路径
        ...continue/force...
    _apply_events([TurnEnded(reason="reply", ...)], ...)
    outcome = "finished"; break
tool_call = step.tool_call
...（既有 adjudicate/execute 流程不变）
```

- `_force_respond(message=...)` / `_force_finish` 统一替换为 `_force_reply(engine, state, turn_id, message, reason, accumulator, tracer)`：直接写 assistant reply 消息行 + `TurnEnded(payload={"reason": reason})`，不再经 router/内建工具；`_maybe_answer_pending_decision_from_user_message` 与 replay 路径中的 ToolCall 语义封装为 `PlannerStep(tool_call=...)`。
- 消息落库助手 `_persist_assistant_message`：message_id 沿用 `msg_{turn}_{seq}` 风格（turn 内自增 seq），复用 MessagesRepository。
- turn_observations 回灌：narration 文本以 `助手叙述: {content}` 追加进 turn_observations（保持 planner 每步可见自己说过什么）。
- 测试迁移规则（机械）：脚本 `{"tool_name":"respond","arguments":{"message":X}}` → `{"content": X}`；`finish_turn` → `{"content": "<简短收尾>"}`；`refuse` → `{"content": X}`；断言 `tool_name=="respond"` 的改为断言 messages 表 reply 行/TurnEnded reason。
- [ ] **Step 1:** 先写新协议核心单测（纯 content 结束 turn 且落 reply 行；content+tool_call 落 narration 行并继续执行工具；双空两次后 force；12 步上限 force 落 reply 行）→ RED
- [ ] **Step 2:** 实现协议改造与退役 → GREEN（该步会打红大量既有 loop 测试，按机械规则迁移到全绿）
- [ ] **Step 3:** 全量 pytest + ruff/format + mypy + `uv run python scripts/check_contracts.py`（tool registry 检查同步）→ Commit `feat(harness): content 即散文协议，退役 respond/refuse/finish_turn`

### Task 3: provider 流式调用 invoke_stream

**Files:**
- Modify: `packages/providers/openai_compatible/llm.py`
- Test: `tests/providers/` 新增流式解析用例（httpx.MockTransport 喂 SSE 字节流）

**Interfaces:**
- Produces: `async def invoke_stream(self, request: ProviderRequest, *, on_delta: Callable[[Mapping[str, Any]], None]) -> ProviderResult`——请求体在 `_request_body` 基础上加 `"stream": True` 与 `"stream_options": {"include_usage": True}`；用 `client.stream("POST", _CHAT_COMPLETIONS_PATH, json=body)` + `aiter_lines()` 解析 `data:` 行：`choices[0].delta.content` 增量即回调 `on_delta({"type":"text","text":chunk})`；`delta.tool_calls` 按 index 累积 name/arguments 字符串；`[DONE]` 后组装成与 `_normalize_chat_response` 同构的 normalized_output（content 全文 + tool_calls 列表 + usage），复用 `_success_result` 的归一化路径。HTTP≥400 / 传输错误沿用既有 `_http_error`/`_transport_error`（流式不做中途重试，失败即 error result——重试语义留给上层降级）。
- 保留既有 `invoke` 不动（降级路径）。

- [ ] **Step 1:** 测试先行：MockTransport 返回手工构造的 SSE 流（含 content 分片、tool_call 分片、usage、[DONE]），断言 on_delta 收到的分片序列与最终 normalized_output（content 全文/工具名/参数 JSON/usage）→ RED
- [ ] **Step 2:** 实现 → GREEN；补一条 HTTP 400 流式错误用例
- [ ] **Step 3:** ruff/format/mypy + 全量 pytest → Commit `feat(providers): openai_compatible 流式调用`

### Task 4: gateway 流式 + GatewayLLMPlanner 新协议

**Files:**
- Modify: `packages/providers/gateway.py`（读文件后为 `call` 增加流式变体或 on_delta 透传，保持 recorder 记账）
- Modify: `packages/providers/planner.py`（PlannerToolCall 保留；plan 返回结构改为含 content；tool_choice 默认 `"auto"`）
- Modify: `packages/providers/mock/scripted.py`（MockProvider 脚本项支持 `{"content": "...", "tool_call": {...}?}`；invoke_stream 支持：把 content 按固定 8 字符分片回调再返回终值——供 golden/流式测试）
- Test: `tests/providers/`

**Interfaces:**
- Produces: `GatewayLLMPlanner.plan(context, tools, *, on_delta=None) -> Mapping`，返回值 `model_dump()` 含 `{"content": str|None, "tool_name": str|None, "arguments": dict, "tool_call_id": str|None}`；loop 侧（Task 2 已定 PlannerStep）在 apps/api 装配处把该 mapping 适配成 PlannerStep——**为避免双向依赖，适配函数 `planner_step_from_mapping` 放 `packages/agent_harness/loop.py` 并在 Task 4 里接线**（读 apps/api/main.py `_planner_from_env` 与 default_runner，确认 planner 注入点后包一层适配器类 `MappingPlannerAdapter`）。
- 语义：`tool_choice="auto"`（构造函数默认值改掉）；`_first_tool_call` 逻辑保留（仍只取第一个）；原 :116-118「无工具调用→合成 respond」与 `_provider_error_respond` 删除——无工具调用时返回 content 本身（协议已允许），provider error 返回 `{"content": "LLM provider 调用失败：...", ...}` 纯文本回复。
- gateway：优先最小改动——`call(request, provider_id=None, on_delta=None)`，provider 有 `invoke_stream` 且 on_delta 非空则走流式，否则走 `invoke` 后整段回调一次（降级路径天然覆盖）。recorder 记账用最终 ProviderResult，不记增量。

- [ ] **Step 1:** 测试先行（MockProvider content/工具/混合三种脚本 × 流式与降级两路 → plan 输出与 on_delta 分片断言）→ RED
- [ ] **Step 2:** 实现 → GREEN
- [ ] **Step 3:** ruff/format/mypy + 全量 pytest → Commit `feat(providers): gateway/planner 流式与 content 协议`

### Task 5: TurnStreamHub + loop 事件发射 + SSE 端点

**Files:**
- Create: `apps/api/turn_stream.py`
- Modify: `packages/agent_harness/loop.py`（run_turn 加 `turn_listener` 参数并在关键点发射）
- Modify: `apps/api/main.py`（ApiState 挂 hub；default_runner 接 listener；新 SSE 路由）
- Test: `tests/api/`（端点快照+实时）、`tests/agent_harness/`（listener 发射序列）

**Interfaces:**
- Produces:

```python
# packages/agent_harness/loop.py
class TurnListener(Protocol):
    def emit(self, event: Mapping[str, Any]) -> None: ...   # 同步、绝不抛错由实现兜底
# run_turn(..., turn_listener: TurnListener | None = None)
```

事件形状（dict，`type` 区分）：`turn_started {turn_id}`、`text_delta {message_id, kind, delta}`（kind 在 step 未定型前先发 "assistant"，`message_completed {message_id, kind, content}` 时定型 narration/reply）、`tool_step_started {step_id, tool}`、`tool_step_finished {step_id, tool, status}`、`turn_ended {outcome, reason}`、`turn_error {message}`。发射点：turn 开始/planner on_delta 透传/消息落库后/execute_tool 前后（含 deny/ask/defer 的 finished 状态）/循环 break 后。
```python
# apps/api/turn_stream.py
class TurnStreamHub:
    def listener_for(self, case_id: str) -> TurnListener  # 供 runner
    async def subscribe(self, case_id: str) -> tuple[list[dict], asyncio.Queue]  # (当前turn快照, 实时队列)
```
快照 = 当前进行中 turn 的全部已发事件（`turn_ended` 后清空缓冲）；订阅者队列有限容量（如 1024，满则丢弃订阅者并关闭其 SSE）。emit 由 hub 用 `loop.call_soon_threadsafe` 兼容（run_turn 在事件循环内跑，直接 put_nowait 即可——读 turn_queue._CaseWorker 确认后按实际情况实现）。
- SSE 路由 `GET /api/projects/{project_id}/cases/{case_id}/turn-stream`：连接即发快照事件流，然后阻塞读队列；帧格式沿用 `encode_sse_row` 风格（`event: turn_stream`,`data: <json>`）；不做 Last-Event-ID。
- default_runner：`run_turn(..., turn_listener=state.turn_stream_hub.listener_for(item.case_id))`。

- [ ] **Step 1:** hub 单测先行（订阅先后收快照+实时、turn_ended 清缓冲、慢订阅者被踢）→ RED → 实现 hub → GREEN
- [ ] **Step 2:** loop 发射测试（ScriptedPlanner 两步脚本断言事件序列完整有序）→ RED → run_turn 接 listener → GREEN
- [ ] **Step 3:** API 端点测试（httpx ASGI 客户端读 SSE 前 N 帧）→ GREEN
- [ ] **Step 4:** 全量 pytest + ruff/format/mypy → Commit `feat(api,harness): turn-stream 进程内流式通道`

### Task 6: golden 重录 + e2e 路径驱动迁移

**Files:**
- Modify: `tests/golden/framework.py`、`tests/golden/test_cases.py` 及其夹具（先读框架再动）
- Modify: `scripts/e2e_paths/`（驱动脚本/断言中 respond/finish_turn 语义 → content 收尾）
- Test: `uv run pytest tests/golden tests/scripts -q`

- [ ] **Step 1:** 读 tests/golden/framework.py 与用例，列出脚本形状与断言点
- [ ] **Step 2:** 按 Task 2 的机械规则迁移脚本与断言（respond→content 步；断言 TurnEnded reason/messages 行）
- [ ] **Step 3:** e2e_paths 同理（grep respond/finish_turn）；`uv run pytest tests/golden tests/scripts -q` 绿
- [ ] **Step 4:** Commit `test(golden,e2e): 迁移到 content 终止协议并重录`

### Task 7: 前端——历史回放 + 流式气泡 + 过程条目

**Files:**
- Modify: `apps/web/src/api/client.ts`（`getCaseMessages`、turn-stream EventSource 工厂）
- Create: `apps/web/src/components/Console/useTurnStream.ts`
- Modify: `apps/web/src/routes/CaseAgentConsole.tsx`（:47-52 messagesQuery 启用真 fetch；接 useTurnStream；:99-101 TurnEnded 处理合并封口+refetch）
- Modify: `apps/web/src/components/Console/runtime.ts`、`AssistantThread.tsx`（narration 弱化样式、进行中气泡、过程条目）
- Modify: `apps/web/src/components/Console/StructuredInteractionRenderer/events.ts`（如需扩展条目类型）
- Test: `apps/web/src/routes/` 页面测试 + hook 单测

**Interfaces:**
- Consumes: Task 1 的 GET messages 返回形状、Task 5 的事件形状（type 区分，字段如上）。
- Produces: `useTurnStream(projectId, caseId)` → `{ inProgressMessages: {message_id, kind, text}[], toolSteps: {step_id, tool, status}[], turnActive: boolean }`；EventSource 断线自动重连（浏览器默认），重连后依赖服务端快照恢复。
- 渲染规则：历史 reply=正常 assistant 气泡、narration=弱化样式（沿用现有 className 体系加一档次级样式）；进行中消息 text_delta 增量 append；tool_step 显示为过程条目（中文文案映射表：`timeline.apply_patch`→修改时间线、`render.preview`→渲染预览等，未映射显示工具名；running→✓/✗）；turn_ended 封口 + `invalidateQueries(messages)`。
- [ ] **Step 1:** 页面测试先行（mock messages 接口渲染历史含 narration 弱化；mock EventSource 推 text_delta 断言气泡增长、tool_step 条目出现、turn_ended 后输入框解锁）→ RED
- [ ] **Step 2:** 实现 → GREEN
- [ ] **Step 3:** `npx -y pnpm@10.13.1 --dir apps/web typecheck && ... test -- --run && ... build` 全绿 → Commit `feat(web): 消息历史回放与流式过程渲染`

### Task 8: PRD 修订 + Playwright 流式用例 + 全量收口

**Files:**
- Modify: `PRD.md`（spec「PRD 修订清单」五条：§4.1/§4.2 planner 输出语义、§6.11 删三内建、§4.4 特判描述、§2.4/§2.6 过程可见性、§7.9 messages.kind）
- Modify: `e2e/tests/`（新增或扩展用例：发消息后断言对话流出现助手文本——依赖 e2e 栈的 scripted planner 输出 content）
- Test: 全套

- [ ] **Step 1:** PRD 逐条修订（范围纪律同 Spec A：只动清单内容；报告列出行号+改前改后）
- [ ] **Step 2:** Playwright 用例（读 e2e/ 现有 path3 结构与 fixture 方式后新增；本地跑通或说明环境障碍）
- [ ] **Step 3:** 全量验证：`uv run ruff check && uv run ruff format --check && uv run mypy && uv run pytest -q && uv run python scripts/check_contracts.py` + web 三连 + 验收 grep：`grep -rn "\"respond\"\|'respond'\|finish_turn\|tool_choice.*required" --include="*.py" packages apps scripts | grep -v test` 应仅剩合理残留（报告解释每一条）
- [ ] **Step 4:** Commit `docs(prd),test(e2e): Spec B 契约同步与流式用例`
