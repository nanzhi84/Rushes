"""Single-turn harness loop."""

from __future__ import annotations

import contextlib
import hashlib
import json
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal, Protocol

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.case import CaseState
from contracts.decision import Decision, PendingToolCall
from contracts.events import (
    Actor,
    CapabilityDegraded,
    DomainEventBase,
    JobEnqueued,
    MemoryCandidateDiscarded,
    TurnEnded,
)
from contracts.project import ProjectState
from contracts.timeline import TimelineState
from contracts.tool import PatchOpSpec, ToolSpec
from contracts.tool_result import ToolError, ToolResult
from domain.decision_effects import HarnessFollowup
from domain.preconditions import PreconditionContext, ProjectArtifactStats, ProjectAudioAsset
from storage import schema
from storage.db import begin_immediate
from storage.repositories import (
    CasesRepository,
    DecisionsRepository,
    MessagesRepository,
    ProjectsRepository,
    TimelineVersionsRepository,
)
from storage.repositories._json import load_json
from tools import PATCH_OP_REGISTRY, ToolExecutionContext, ToolRegistry, build_default_tool_registry
from tools.memory_tools import search_relevant_memories

from .context_builder import ContextBuilder, ContextBuildInput, ContextBundle, ContextMessage
from .decision_answering import DecisionAnswerResolver
from .policy_gate import PolicyContext, PolicyGate, ToolCall, Verdict, mark_replayed, next_replay
from .reducer import ReducerApplyResult, apply
from .tool_router import ToolRouter
from .trace import NullTraceRecorder, TraceRecorder
from .turn_queue import StopToken, TurnQueue, TurnQueueItem

DEFAULT_MAX_ILLEGAL_OUTPUTS = 3
DEFAULT_MAX_NONBLOCKING_TOOLS = 5
DEFAULT_MAX_TOOL_ATTEMPTS = 12


@dataclass(frozen=True, slots=True)
class PlannerStep:
    """One planner step: prose content and/or a native tool call.

    - content only  -> assistant reply, turn ends
    - content + tool_call -> narration persisted, tool executes, turn continues
    - tool_call only -> silent tool step
    - neither -> illegal output (counted toward illegal_output_limit)
    """

    content: str | None = None
    tool_call: ToolCall | None = None

    def model_dump(self, mode: str = "json") -> dict[str, Any]:
        del mode
        return {
            "content": self.content,
            "tool_call": (
                None if self.tool_call is None else self.tool_call.model_dump(mode="json")
            ),
        }


class TurnListener(Protocol):
    """Sink for in-process turn stream events (duck-typed, apps-side impl).

    ``emit`` is synchronous and MUST NEVER raise into the loop; the concrete
    hub implementation catches/degrades internally, and the loop additionally
    guards every call so a buggy listener can never break a turn.
    """

    def emit(self, event: Mapping[str, Any]) -> None: ...


def _emit_turn_event(listener: TurnListener | None, event: dict[str, Any]) -> None:
    if listener is None:
        return
    # 监听器绝不能把回合搞崩：hub 内部已兜底，这里再加一层防御。
    with contextlib.suppress(Exception):
        listener.emit(event)


def _emit_tool_step_finished(
    listener: TurnListener | None,
    step_id: str,
    tool: str,
    status: str,
) -> None:
    _emit_turn_event(
        listener,
        {"type": "tool_step_finished", "step_id": step_id, "tool": tool, "status": status},
    )


def _delta_forwarder(
    listener: TurnListener | None,
    message_id: str,
) -> Callable[[str], None] | None:
    if listener is None:
        return None

    def _forward(delta: str) -> None:
        _emit_turn_event(
            listener,
            {
                "type": "text_delta",
                "message_id": message_id,
                "kind": "assistant",
                "delta": delta,
            },
        )

    return _forward


class LLMPlanner(Protocol):
    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> PlannerStep:
        """Return one planner step (prose content and/or a native tool call)."""


class _PlannerMessage(Protocol):
    def model_dump(self, *args: Any, **kwargs: Any) -> Mapping[str, Any]:
        """Expose ``content``/``tool_name``/``arguments``/``tool_call_id``."""


class _MappingPlanner(Protocol):
    # context is typed loosely (Any): the wrapped provider planner accepts a
    # structural ContextBundleLike, which is broader than ContextBundle and
    # would otherwise break the structural match here.
    async def plan(
        self,
        context: Any,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> _PlannerMessage:
        """Return a mapping-style planner message (see providers.GatewayLLMPlanner)."""


class MappingPlannerAdapter:
    """Adapt a mapping-style planner into the harness ``LLMPlanner`` protocol.

    The production planner lives in ``providers`` (which the harness must not
    import), so it is duck-typed here: its ``plan`` returns an object whose
    ``model_dump()`` carries ``content`` / ``tool_name`` / ``arguments`` /
    ``tool_call_id``. This adapter converts that into a ``PlannerStep``.
    """

    def __init__(self, planner: _MappingPlanner) -> None:
        self._planner = planner

    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> PlannerStep:
        message = await self._planner.plan(context, tools, on_delta=on_delta)
        data = message.model_dump()
        content = data.get("content")
        tool_name = data.get("tool_name")
        tool_call: ToolCall | None = None
        if isinstance(tool_name, str) and tool_name:
            tool_call = ToolCall.from_input(
                {
                    "tool_name": tool_name,
                    "arguments": data.get("arguments") or {},
                    "tool_call_id": data.get("tool_call_id"),
                }
            )
        return PlannerStep(
            content=content if isinstance(content, str) else None,
            tool_call=tool_call,
        )


class ScriptedPlanner:
    """Deterministic planner used by loop tests."""

    def __init__(self, steps: Sequence[PlannerStep | ToolCall | Mapping[str, Any]]) -> None:
        self._steps = [_scripted_step(step) for step in steps]
        self._index = 0

    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> PlannerStep:
        del context, tools
        if self._index >= len(self._steps):
            return PlannerStep(content="（脚本耗尽，结束本回合）")
        step = self._steps[self._index]
        self._index += 1
        if on_delta is not None and step.content:
            on_delta(step.content)
        if step.tool_call is not None and step.tool_call.tool_call_id is None:
            step = PlannerStep(
                content=step.content,
                tool_call=step.tool_call.model_copy(
                    update={"tool_call_id": f"scripted_{self._index}"}
                ),
            )
        return step

    @property
    def calls_remaining(self) -> int:
        return max(0, len(self._steps) - self._index)


def _scripted_step(value: PlannerStep | ToolCall | Mapping[str, Any]) -> PlannerStep:
    if isinstance(value, PlannerStep):
        return value
    if isinstance(value, ToolCall):
        return PlannerStep(tool_call=value)
    data = dict(value)
    if "content" in data or "tool_call" in data:
        content = data.get("content")
        raw_tool_call = data.get("tool_call")
        return PlannerStep(
            content=content if isinstance(content, str) else None,
            tool_call=None if raw_tool_call is None else ToolCall.from_input(raw_tool_call),
        )
    return PlannerStep(tool_call=ToolCall.from_input(data))


@dataclass(frozen=True, slots=True)
class RunTurnResult:
    turn_id: str
    case_id: str
    outcome: str
    tool_calls: tuple[ToolCall, ...] = ()
    tool_results: tuple[ToolResult, ...] = ()
    reducer_results: tuple[ReducerApplyResult, ...] = ()
    replays_enqueued: tuple[str, ...] = ()
    forced_reason: str | None = None


@dataclass(slots=True)
class _LoadedState:
    case_state: CaseState
    project_state: ProjectState | None
    project_artifacts: ProjectArtifactStats
    project_audio_assets: tuple[ProjectAudioAsset, ...]
    decisions: tuple[Decision, ...]
    pending_decision: Decision | None
    messages: tuple[ContextMessage, ...]
    timeline: TimelineState | None
    memory_summaries: tuple[str, ...]


@dataclass(slots=True)
class _RunAccumulator:
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_results: list[ToolResult] = field(default_factory=list)
    reducer_results: list[ReducerApplyResult] = field(default_factory=list)
    replays_enqueued: list[str] = field(default_factory=list)
    message_seq: int = 0


async def run_turn(
    item: TurnQueueItem,
    *,
    engine: Engine,
    planner: LLMPlanner,
    registry: ToolRegistry | None = None,
    patch_op_specs: Mapping[str, PatchOpSpec] | None = None,
    turn_queue: TurnQueue | None = None,
    stop_token: StopToken | None = None,
    turn_id: str | None = None,
    trace_recorder: TraceRecorder | NullTraceRecorder | None = None,
    decision_answer_resolver: DecisionAnswerResolver | None = None,
    max_illegal_outputs: int = DEFAULT_MAX_ILLEGAL_OUTPUTS,
    max_nonblocking_tools: int = DEFAULT_MAX_NONBLOCKING_TOOLS,
    max_tool_attempts: int = DEFAULT_MAX_TOOL_ATTEMPTS,
    tool_gateway: Any | None = None,
    turn_listener: TurnListener | None = None,
) -> RunTurnResult:
    active_registry = registry or build_default_tool_registry()
    active_patch_ops = patch_op_specs or PATCH_OP_REGISTRY.as_mapping()
    policy_gate = PolicyGate(
        tool_specs=active_registry.specs_by_name(),
        patch_op_specs=active_patch_ops,
    )
    router = ToolRouter(active_registry)
    active_turn_id = turn_id or _turn_id(item)
    loaded = _load_state(engine, item.case_id)
    tracer = trace_recorder or TraceRecorder(
        engine=engine,
        case_id=item.case_id,
        turn_id=active_turn_id,
    )
    accumulator = _RunAccumulator()
    token = stop_token or StopToken()
    _emit_turn_event(turn_listener, {"type": "turn_started", "turn_id": active_turn_id})
    try:
        return await _run_turn_body(
            item,
            engine=engine,
            planner=planner,
            router=router,
            policy_gate=policy_gate,
            active_registry=active_registry,
            active_turn_id=active_turn_id,
            loaded=loaded,
            tracer=tracer,
            accumulator=accumulator,
            token=token,
            turn_queue=turn_queue,
            decision_answer_resolver=decision_answer_resolver,
            max_illegal_outputs=max_illegal_outputs,
            max_nonblocking_tools=max_nonblocking_tools,
            max_tool_attempts=max_tool_attempts,
            tool_gateway=tool_gateway,
            turn_listener=turn_listener,
        )
    except Exception as exc:
        _emit_turn_event(turn_listener, {"type": "turn_error", "message": str(exc)})
        raise


async def _run_turn_body(
    item: TurnQueueItem,
    *,
    engine: Engine,
    planner: LLMPlanner,
    router: ToolRouter,
    policy_gate: PolicyGate,
    active_registry: ToolRegistry,
    active_turn_id: str,
    loaded: _LoadedState,
    tracer: TraceRecorder | NullTraceRecorder,
    accumulator: _RunAccumulator,
    token: StopToken,
    turn_queue: TurnQueue | None,
    decision_answer_resolver: DecisionAnswerResolver | None,
    max_illegal_outputs: int,
    max_nonblocking_tools: int,
    max_tool_attempts: int,
    tool_gateway: Any | None,
    turn_listener: TurnListener | None,
) -> RunTurnResult:
    await _record_incoming_item(item, engine=engine, turn_id=active_turn_id)

    replay_call = _replay_tool_call_from_item(item)
    if replay_call is None and decision_answer_resolver is not None:
        preflight_forced_reason = await _maybe_answer_pending_decision_from_user_message(
            item,
            decision_answer_resolver,
            router,
            policy_gate,
            engine=engine,
            turn_id=active_turn_id,
            turn_queue=turn_queue,
            accumulator=accumulator,
            tracer=tracer,
            turn_listener=turn_listener,
        )
        if preflight_forced_reason is not None:
            _emit_turn_event(
                turn_listener,
                {
                    "type": "turn_ended",
                    "outcome": "forced_end",
                    "reason": preflight_forced_reason,
                },
            )
            return RunTurnResult(
                turn_id=active_turn_id,
                case_id=item.case_id,
                outcome="forced_end",
                tool_calls=tuple(accumulator.tool_calls),
                tool_results=tuple(accumulator.tool_results),
                reducer_results=tuple(accumulator.reducer_results),
                replays_enqueued=tuple(accumulator.replays_enqueued),
                forced_reason=preflight_forced_reason,
            )
    illegal_outputs = 0
    nonblocking_tools = 0
    attempts = 0
    outcome = "finished"
    forced_reason: str | None = None
    # turn 内工具轨迹：observation 不落消息表，必须显式回灌 planner，
    # 否则每步规划都失忆（M9 路径 1 实测无限重复只读工具）
    turn_observations: list[str] = []
    if item.kind == "job_observation":
        event_payload = item.payload.get("event")
        if isinstance(event_payload, Mapping):
            event_name = event_payload.get("event", "JobEvent")
            job_kind = ""
            inner = event_payload.get("payload")
            if isinstance(inner, Mapping) and inner.get("kind"):
                job_kind = f" kind={inner['kind']}"
            turn_observations.append(
                f"后台任务事件：{event_name} job_id={item.payload.get('job_id')}{job_kind}，"
                "请依据该结果推进下一步。"
            )

    while True:
        if attempts >= max_tool_attempts:
            forced_reason = "hard_attempt_limit"
            _force_reply(
                engine=engine,
                state=loaded,
                turn_id=active_turn_id,
                message=(
                    "本回合工具调用达到 12 次上限，已停止继续执行；请缩小请求或补充更明确的目标。"
                ),
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
                turn_listener=turn_listener,
            )
            outcome = "forced_end"
            break

        loaded = _load_state(engine, item.case_id)
        context_bundle = _build_context(policy_gate, loaded, tuple(turn_observations))
        tracer.record(
            "context",
            {
                "token_counts": context_bundle.token_counts,
                "allowed_tools": [spec.name for spec in context_bundle.allowed_tools],
                "turn_item": {
                    "kind": item.kind,
                    "item_id": item.item_id,
                    "payload_keys": sorted(item.payload),
                },
            },
        )
        if replay_call is not None:
            step = PlannerStep(tool_call=replay_call)
            replay_call = None
        else:
            upcoming_message_id = _peek_next_message_id(active_turn_id, accumulator)
            step = await planner.plan(
                context_bundle,
                context_bundle.allowed_tools,
                on_delta=_delta_forwarder(turn_listener, upcoming_message_id),
            )
        attempts += 1
        tracer.record("action", step.model_dump())

        # narration 在工具执行前立刻落库：即使后续工具被拒/失败，叙述也已可见
        if step.content:
            message_kind = "narration" if step.tool_call is not None else "reply"
            message_id = _next_message_id(active_turn_id, accumulator)
            _persist_assistant_message(
                engine,
                case_id=loaded.case_state.case_id,
                message_id=message_id,
                content=step.content,
                kind=message_kind,
            )
            _emit_turn_event(
                turn_listener,
                {
                    "type": "message_completed",
                    "message_id": message_id,
                    "kind": message_kind,
                    "content": step.content,
                },
            )
            if message_kind == "narration":
                turn_observations.append(f"助手叙述: {step.content}")

        if step.tool_call is None:
            if not step.content:
                # 双空：既无 content 也无 tool_call，计入非法输出
                illegal_outputs += 1
                turn_observations.append(
                    "上一步输出为空（既无内容也无工具调用），请直接给出回复或发起一个工具调用。"
                )
                if illegal_outputs >= max_illegal_outputs:
                    forced_reason = "illegal_output_limit"
                    _force_reply(
                        engine=engine,
                        state=loaded,
                        turn_id=active_turn_id,
                        message=(
                            f"连续 {max_illegal_outputs} 次规划输出为空"
                            "（既无内容也无工具调用），已停止本回合。"
                        ),
                        reason=forced_reason,
                        accumulator=accumulator,
                        tracer=tracer,
                        turn_listener=turn_listener,
                    )
                    outcome = "forced_end"
                    break
                continue
            # 纯 content：即回复，回合结束
            end_event = TurnEnded(
                turn_id=active_turn_id,
                case_id=loaded.case_state.case_id,
                project_id=loaded.case_state.project_id,
                payload={"reason": "reply"},
            )
            reducer_result = _apply_events(
                (end_event,),
                engine=engine,
                base_version=loaded.case_state.state_version,
                actor="agent",
            )
            accumulator.reducer_results.append(reducer_result)
            tracer.record("events", _events_payload((end_event,), reducer_result))
            if reducer_result.status != "applied":
                forced_reason = f"reducer_{reducer_result.status}"
                _force_reply(
                    engine=engine,
                    state=loaded,
                    turn_id=active_turn_id,
                    message=f"回合收尾写入被拒绝，已停止本回合。{_reducer_diagnostic(reducer_result)}",
                    reason=forced_reason,
                    accumulator=accumulator,
                    tracer=tracer,
                    turn_listener=turn_listener,
                )
                outcome = "forced_end"
                break
            outcome = "finished"
            break

        tool_call = step.tool_call
        accumulator.tool_calls.append(tool_call)
        step_id = tool_call.tool_call_id or _tool_call_id(tool_call)
        _emit_turn_event(
            turn_listener,
            {"type": "tool_step_started", "step_id": step_id, "tool": tool_call.tool_name},
        )

        verdict = policy_gate.adjudicate(
            tool_call,
            PolicyContext(
                preconditions=context_bundle_input_preconditions(loaded),
                decisions=loaded.decisions,
                pending_decision=loaded.pending_decision,
                allowed_tools=tuple(context_bundle.allowed_tools),
            ),
        )
        tracer.record("gate", _verdict_payload(verdict))

        if verdict.status == "deny":
            _emit_tool_step_finished(turn_listener, step_id, tool_call.tool_name, "deny")
            illegal_outputs += 1
            synthetic_result = _synthetic_tool_result(tool_call, "failed", verdict.reason)
            accumulator.tool_results.append(synthetic_result)
            turn_observations.append(_turn_observation_entry(tool_call, synthetic_result))
            tracer.record("tool_result", _tool_result_payload(synthetic_result))
            reducer_result = _apply_events(
                verdict.events,
                engine=engine,
                base_version=loaded.case_state.state_version,
                actor="agent",
            )
            accumulator.reducer_results.append(reducer_result)
            tracer.record("events", _events_payload(verdict.events, reducer_result))
            if illegal_outputs >= max_illegal_outputs:
                forced_reason = "illegal_output_limit"
                _force_reply(
                    engine=engine,
                    state=loaded,
                    turn_id=active_turn_id,
                    message=(
                        f"连续 {max_illegal_outputs} 次工具调用没有通过安全或 "
                        f"schema 校验，已停止本回合。卡点：{verdict.reason}"
                    ),
                    reason=forced_reason,
                    accumulator=accumulator,
                    tracer=tracer,
                    turn_listener=turn_listener,
                )
                outcome = "forced_end"
                break
            continue

        illegal_outputs = 0
        if verdict.status == "ask":
            _emit_tool_step_finished(turn_listener, step_id, tool_call.tool_name, "ask")
            result = _synthetic_tool_result(tool_call, "requires_user", verdict.reason)
            accumulator.tool_results.append(result)
            tracer.record("tool_result", _tool_result_payload(result))
            reducer_result = _apply_events(
                verdict.events,
                engine=engine,
                base_version=loaded.case_state.state_version,
                actor="agent",
            )
            accumulator.reducer_results.append(reducer_result)
            tracer.record("events", _events_payload(verdict.events, reducer_result))
            outcome = "requires_user"
            break

        if verdict.status == "defer":
            result = _defer_tool_call(
                active_registry.require(tool_call.tool_name),
                tool_call,
                verdict.validated_arguments,
                state=loaded,
                turn_id=active_turn_id,
                engine=engine,
            )
            _emit_tool_step_finished(turn_listener, step_id, tool_call.tool_name, result.status)
            turn_observations.append(_turn_observation_entry(tool_call, result))
            accumulator.tool_results.append(result)
            tracer.record("tool_result", _tool_result_payload(result))
            reducer_result = _apply_events(
                result.events,
                engine=engine,
                base_version=loaded.case_state.state_version,
                actor="agent",
            )
            accumulator.reducer_results.append(reducer_result)
            tracer.record("events", _events_payload(result.events, reducer_result))
            if reducer_result.status != "applied":
                forced_reason = f"reducer_{reducer_result.status}"
                diagnostic = _reducer_diagnostic(reducer_result)
                _force_reply(
                    engine=engine,
                    state=loaded,
                    turn_id=active_turn_id,
                    message=f"长任务入队被拒绝，已停止本回合。{diagnostic}",
                    reason=forced_reason,
                    accumulator=accumulator,
                    tracer=tracer,
                    turn_listener=turn_listener,
                )
                outcome = "forced_end"
                break
            outcome = "running"
            break

        result = _execute_tool(
            router,
            tool_call,
            engine=engine,
            state=loaded,
            turn_id=active_turn_id,
            gateway=tool_gateway,
        )
        _emit_tool_step_finished(turn_listener, step_id, tool_call.tool_name, result.status)
        accumulator.tool_results.append(result)
        turn_observations.append(_turn_observation_entry(tool_call, result))
        tracer.record("tool_result", _tool_result_payload(result))
        _persist_tool_result_data(result, engine=engine)
        reducer_result = _apply_events(
            result.events,
            engine=engine,
            base_version=loaded.case_state.state_version,
            actor="agent",
        )
        accumulator.reducer_results.append(reducer_result)
        tracer.record("events", _events_payload(result.events, reducer_result))
        if reducer_result.status != "applied":
            forced_reason = f"reducer_{reducer_result.status}"
            diagnostic = _reducer_diagnostic(reducer_result)
            _force_reply(
                engine=engine,
                state=loaded,
                turn_id=active_turn_id,
                message=f"状态写入被拒绝，已停止本回合。{diagnostic}",
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
                turn_listener=turn_listener,
            )
            outcome = "forced_end"
            break
        enqueued = await _handle_followups(
            reducer_result,
            engine=engine,
            router=router,
            turn_queue=turn_queue,
            turn_id=active_turn_id,
            accumulator=accumulator,
            tracer=tracer,
        )
        accumulator.replays_enqueued.extend(enqueued)

        if result.status == "running":
            outcome = "running"
            break
        if result.status == "requires_user":
            outcome = "requires_user"
            break
        if token.cancel_requested:
            forced_reason = "stopped_by_user"
            _force_reply(
                engine=engine,
                state=_load_state(engine, item.case_id),
                turn_id=active_turn_id,
                message="已按停止请求结束本回合。",
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
                turn_listener=turn_listener,
            )
            outcome = "stopped"
            break
        nonblocking_tools += 1
        if nonblocking_tools >= max_nonblocking_tools:
            forced_reason = "nonblocking_tool_limit"
            _force_reply(
                engine=engine,
                state=_load_state(engine, item.case_id),
                turn_id=active_turn_id,
                message=(
                    f"本回合已连续完成 {max_nonblocking_tools} 个非阻塞步骤，"
                    "先暂停并汇报进度，等待下一次输入或观察结果。"
                ),
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
                turn_listener=turn_listener,
            )
            outcome = "forced_end"
            break

    _emit_turn_event(
        turn_listener,
        {"type": "turn_ended", "outcome": outcome, "reason": forced_reason},
    )
    return RunTurnResult(
        turn_id=active_turn_id,
        case_id=item.case_id,
        outcome=outcome,
        tool_calls=tuple(accumulator.tool_calls),
        tool_results=tuple(accumulator.tool_results),
        reducer_results=tuple(accumulator.reducer_results),
        replays_enqueued=tuple(accumulator.replays_enqueued),
        forced_reason=forced_reason,
    )


async def recover_approved_pending_tool_calls(
    *,
    engine: Engine,
    turn_queue: TurnQueue,
    created_at: str | None = None,
) -> int:
    """Scan approved outbox decisions and enqueue their pending tool replays once."""

    replay_items: list[tuple[str, str, dict[str, Any], str]] = []
    now = created_at or _now_iso()
    with begin_immediate(engine) as connection:
        rows = connection.execute(
            select(schema.decisions).where(
                schema.decisions.c.pending_tool_call_status == "approved"
            )
        ).all()
        repository = DecisionsRepository(connection)
        for row in rows:
            decision = _decision_from_row(dict(row._mapping))
            pending = next_replay(decision)
            target_case_id = decision.case_id or _case_id_for_pending_replay(
                connection,
                pending,
                decision.project_id,
            )
            if pending is None or target_case_id is None:
                continue
            replayed_tool_call_id = _replayed_tool_call_id(decision.decision_id)
            if not mark_replayed(
                repository,
                decision.decision_id,
                replayed_tool_call_id=replayed_tool_call_id,
                consumed_at=now,
            ):
                continue
            replay_items.append(
                (
                    target_case_id,
                    decision.decision_id,
                    pending.model_dump(mode="json"),
                    replayed_tool_call_id,
                )
            )
    for case_id, decision_id, pending_payload, replayed_tool_call_id in replay_items:
        await turn_queue.enqueue_ui_observation(
            case_id,
            observation_type="replay_pending_tool_call",
            payload={
                "decision_id": decision_id,
                "pending_tool_call": pending_payload,
                "replayed_tool_call_id": replayed_tool_call_id,
            },
            item_id=decision_id,
        )
    return len(replay_items)


def context_bundle_input_preconditions(loaded: _LoadedState) -> PreconditionContext:
    return PreconditionContext(
        case_state=loaded.case_state,
        project_state=loaded.project_state,
        project_artifacts=loaded.project_artifacts,
        project_audio_assets=loaded.project_audio_assets,
    )


def _build_context(
    policy_gate: PolicyGate,
    loaded: _LoadedState,
    turn_observations: tuple[str, ...] = (),
) -> ContextBundle:
    builder = ContextBuilder(policy_gate=policy_gate)
    return builder.build(
        ContextBuildInput(
            preconditions=context_bundle_input_preconditions(loaded),
            decisions=loaded.decisions,
            pending_decision=loaded.pending_decision,
            timeline=loaded.timeline,
            messages=loaded.messages,
            memory_summaries=loaded.memory_summaries,
            turn_observations=turn_observations,
        )
    )


def _finished_job_observation(
    engine: Engine,
    job_id: str,
    *,
    tool_name: str,
) -> tuple[Literal["succeeded", "failed"], str] | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.jobs.c.status, schema.jobs.c.result_json, schema.jobs.c.error_json).where(
                schema.jobs.c.job_id == job_id
            )
        ).first()
    if row is None:
        return None
    status = str(row._mapping["status"])
    if status == "succeeded":
        result_raw = row._mapping["result_json"]
        summary = ""
        if result_raw:
            parsed = load_json(str(result_raw))
            if isinstance(parsed, Mapping):
                summary = json.dumps(parsed, ensure_ascii=False)[:300]
        return (
            "succeeded",
            f"该后台任务已完成（job {job_id}），无需重复发起。"
            f"结果摘要：{summary or '无'}。请基于结果推进下一步。",
        )
    if status == "failed":
        error_raw = row._mapping["error_json"]
        detail = str(error_raw)[:200] if error_raw else "未知原因"
        return ("failed", f"该后台任务此前已失败（job {job_id}）：{detail}")
    return None


def _turn_observation_entry(tool_call: ToolCall, result: ToolResult) -> str:
    arguments = json.dumps(tool_call.arguments, ensure_ascii=False)
    if len(arguments) > 160:
        arguments = arguments[:160] + "…"
    observation = result.observation or ""
    if len(observation) > 400:
        observation = observation[:400] + "…"
    return f"{tool_call.tool_name}({arguments}) -> {result.status}: {observation}"


async def _maybe_answer_pending_decision_from_user_message(
    item: TurnQueueItem,
    resolver: DecisionAnswerResolver,
    router: ToolRouter,
    policy_gate: PolicyGate,
    *,
    engine: Engine,
    turn_id: str,
    turn_queue: TurnQueue | None,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
    turn_listener: TurnListener | None = None,
) -> str | None:
    if item.kind != "user_message":
        return None
    user_message = str(item.payload.get("content", ""))
    if user_message == "":
        return None
    loaded = _load_state(engine, item.case_id)
    if loaded.pending_decision is None:
        return None
    try:
        resolution = await resolver.resolve(
            case_state=loaded.case_state,
            decision=loaded.pending_decision,
            user_message=user_message,
        )
    except Exception as exc:
        event = CapabilityDegraded(
            degradation_id=_degradation_id(turn_id, loaded.pending_decision.decision_id),
            case_id=item.case_id,
            capability="llm.chat",
            provider_id=None,
            reason=f"decision answer resolver failed: {exc}",
            fallback="planner",
            payload={"decision_id": loaded.pending_decision.decision_id},
        )
        reducer_result = _apply_events((event,), engine=engine, base_version=None, actor="system")
        accumulator.reducer_results.append(reducer_result)
        tracer.record(
            "action",
            {
                "stage": "decision_answering",
                "decision_id": loaded.pending_decision.decision_id,
                "status": "degraded",
                "reason": str(exc),
            },
        )
        tracer.record("events", _events_payload((event,), reducer_result))
        return None
    if resolution.answer is None:
        tracer.record(
            "action",
            {
                "stage": "decision_answering",
                "decision_id": loaded.pending_decision.decision_id,
                "status": "unanswered",
            },
        )
        return None

    step = PlannerStep(
        tool_call=ToolCall(
            tool_name="decision.answer",
            arguments={
                "decision_id": loaded.pending_decision.decision_id,
                "answer": resolution.answer.model_dump(mode="json"),
            },
            tool_call_id=f"natural_answer_{turn_id}",
        )
    )
    tool_call = step.tool_call
    assert tool_call is not None
    accumulator.tool_calls.append(tool_call)
    context_bundle = _build_context(policy_gate, loaded)
    tracer.record(
        "context",
        {
            "token_counts": context_bundle.token_counts,
            "allowed_tools": [spec.name for spec in context_bundle.allowed_tools],
            "turn_item": {
                "kind": item.kind,
                "item_id": item.item_id,
                "payload_keys": sorted(item.payload),
            },
            "decision_answering": "natural_language",
        },
    )
    tracer.record("action", step.model_dump())
    verdict = policy_gate.adjudicate(
        tool_call,
        PolicyContext(
            preconditions=context_bundle_input_preconditions(loaded),
            decisions=loaded.decisions,
            pending_decision=loaded.pending_decision,
            allowed_tools=tuple(context_bundle.allowed_tools),
        ),
    )
    tracer.record("gate", _verdict_payload(verdict))
    if verdict.status != "allow":
        synthetic_result = _synthetic_tool_result(tool_call, "failed", verdict.reason)
        accumulator.tool_results.append(synthetic_result)
        tracer.record("tool_result", _tool_result_payload(synthetic_result))
        reducer_result = _apply_events(
            verdict.events,
            engine=engine,
            base_version=loaded.case_state.state_version,
            actor="agent",
        )
        accumulator.reducer_results.append(reducer_result)
        tracer.record("events", _events_payload(verdict.events, reducer_result))
        _force_reply(
            engine=engine,
            state=loaded,
            turn_id=turn_id,
            message=(f"自然语言回答没有通过决策归约校验，已停止本回合。卡点：{verdict.reason}"),
            reason="natural_language_decision_answer_denied",
            accumulator=accumulator,
            tracer=tracer,
            turn_listener=turn_listener,
        )
        return "natural_language_decision_answer_denied"

    result = _execute_tool(
        router,
        tool_call,
        engine=engine,
        state=loaded,
        turn_id=turn_id,
    )
    accumulator.tool_results.append(result)
    tracer.record("tool_result", _tool_result_payload(result))
    reducer_result = _apply_events(
        result.events,
        engine=engine,
        base_version=loaded.case_state.state_version,
        actor="user",
    )
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload(result.events, reducer_result))
    if reducer_result.status != "applied":
        _force_reply(
            engine=engine,
            state=loaded,
            turn_id=turn_id,
            message=(
                f"自然语言回答归约写入失败，已停止本回合。{_reducer_diagnostic(reducer_result)}"
            ),
            reason=f"reducer_{reducer_result.status}",
            accumulator=accumulator,
            tracer=tracer,
            turn_listener=turn_listener,
        )
        return f"reducer_{reducer_result.status}"
    enqueued = await _handle_followups(
        reducer_result,
        engine=engine,
        router=router,
        turn_queue=turn_queue,
        turn_id=turn_id,
        accumulator=accumulator,
        tracer=tracer,
    )
    accumulator.replays_enqueued.extend(enqueued)
    return None


def _load_state(engine: Engine, case_id: str) -> _LoadedState:
    with engine.connect() as connection:
        case_row = CasesRepository(connection).get(case_id)
        if case_row is None:
            raise ValueError(f"case not found: {case_id}")
        case_state = CaseState.model_validate(case_row)
        project_state = _load_project_state(connection, case_state.project_id)
        project_artifacts = _load_project_artifact_stats(connection, case_state)
        project_audio_assets = _load_project_audio_assets(connection, case_state)
        decisions = _load_decisions(connection)
        pending_decision = _find_pending_decision(case_state, decisions)
        messages = tuple(
            ContextMessage(
                role=str(row["role"]),
                content=str(row["content"]),
                created_at=str(row["created_at"]),
                case_id=str(row["case_id"]),
            )
            for row in MessagesRepository(connection).list_for_case(case_id)
        )
        timeline = _load_timeline(connection, case_state)
        memory_summaries = _load_memory_summaries(connection, case_state)
    return _LoadedState(
        case_state=case_state,
        project_state=project_state,
        project_artifacts=project_artifacts,
        project_audio_assets=project_audio_assets,
        decisions=decisions,
        pending_decision=pending_decision,
        messages=messages,
        timeline=timeline,
        memory_summaries=memory_summaries,
    )


def _load_project_state(connection: Connection, project_id: str) -> ProjectState | None:
    row = ProjectsRepository(connection).get(project_id)
    return None if row is None else ProjectState.model_validate(row)


def _load_project_artifact_stats(
    connection: Connection,
    case_state: CaseState,
) -> ProjectArtifactStats:
    disabled = set(case_state.disabled_asset_ids)
    asset_rows = connection.execute(
        select(schema.assets)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == case_state.project_id)
        .where(schema.project_asset_links.c.enabled.is_(True))
    ).all()
    usable_asset_ids: set[str] = set()
    asset_ids_with_audio: set[str] = set()
    voiceover_asset_ids: set[str] = set()
    for row in asset_rows:
        values = dict(row._mapping)
        asset_id = str(values["asset_id"])
        if asset_id in disabled or not bool(values.get("usable")):
            continue
        usable_asset_ids.add(asset_id)
        if _asset_has_audio(values):
            asset_ids_with_audio.add(asset_id)
        if str(values.get("kind")) == "audio":
            voiceover_asset_ids.add(asset_id)
    transcript_rows = connection.execute(select(schema.transcripts)).all()
    transcript_asset_ids: set[str] = set()
    transcript_with_vad_asset_ids: set[str] = set()
    transcript_ids: set[str] = set()
    transcript_ids_with_vad: set[str] = set()
    for row in transcript_rows:
        values = dict(row._mapping)
        transcript_id = str(values["transcript_id"])
        asset_id = str(values["asset_id"])
        if asset_id not in usable_asset_ids:
            continue
        transcript_ids.add(transcript_id)
        transcript_asset_ids.add(asset_id)
        vad_segments = load_json(str(values["vad_segments"]))
        if isinstance(vad_segments, list) and vad_segments:
            transcript_ids_with_vad.add(transcript_id)
            transcript_with_vad_asset_ids.add(asset_id)
    return ProjectArtifactStats(
        usable_asset_count=len(usable_asset_ids),
        usable_asset_ids=frozenset(usable_asset_ids),
        asset_ids_with_audio=frozenset(asset_ids_with_audio),
        transcript_asset_ids=frozenset(transcript_asset_ids),
        transcript_with_vad_asset_ids=frozenset(transcript_with_vad_asset_ids),
        transcript_ids=frozenset(transcript_ids),
        transcript_ids_with_vad=frozenset(transcript_ids_with_vad),
        voiceover_asset_ids=frozenset(voiceover_asset_ids),
        candidate_pack_valid=_candidate_pack_valid(connection, case_state),
    )


def _load_project_audio_assets(
    connection: Connection,
    case_state: CaseState,
) -> tuple[ProjectAudioAsset, ...]:
    disabled = set(case_state.disabled_asset_ids)
    rows = connection.execute(
        select(schema.assets.c.asset_id, schema.assets.c.filename)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == case_state.project_id)
        .where(schema.project_asset_links.c.enabled.is_(True))
        .where(schema.assets.c.kind == "audio")
        .where(schema.assets.c.usable.is_(True))
        .order_by(schema.assets.c.mtime.desc())
    ).all()
    return tuple(
        ProjectAudioAsset(
            asset_id=str(row._mapping["asset_id"]),
            filename=str(row._mapping["filename"] or row._mapping["asset_id"]),
        )
        for row in rows
        if str(row._mapping["asset_id"]) not in disabled
    )


def _asset_has_audio(asset: Mapping[str, Any]) -> bool:
    if str(asset.get("kind")) == "audio":
        return True
    raw_probe = asset.get("probe")
    if not isinstance(raw_probe, str) or not raw_probe:
        return False
    probe = load_json(raw_probe)
    return isinstance(probe, Mapping) and probe.get("has_audio") is True


def _candidate_pack_valid(connection: Connection, case_state: CaseState) -> bool:
    if case_state.candidate_pack_id is None:
        return True
    row = connection.execute(
        select(schema.candidate_packs.c.candidate_pack_id).where(
            schema.candidate_packs.c.candidate_pack_id == case_state.candidate_pack_id
        )
    ).first()
    return row is not None


def _load_decisions(connection: Connection) -> tuple[Decision, ...]:
    rows = connection.execute(select(schema.decisions)).all()
    return tuple(_decision_from_row(dict(row._mapping)) for row in rows)


def _decision_from_row(values: dict[str, Any]) -> Decision:
    decoded = dict(values)
    for key in ("options", "answer", "pending_tool_call"):
        raw_value = decoded.get(key)
        if isinstance(raw_value, str):
            decoded[key] = load_json(raw_value)
    return Decision.model_validate(decoded)


def _find_pending_decision(
    case_state: CaseState,
    decisions: Sequence[Decision],
) -> Decision | None:
    if case_state.pending_decision_id is None:
        return None
    for decision in decisions:
        if decision.decision_id == case_state.pending_decision_id:
            return decision
    return None


def _load_timeline(connection: Connection, case_state: CaseState) -> TimelineState | None:
    version = case_state.timeline_current_version
    if version is None:
        return None
    row = TimelineVersionsRepository(connection).get_by_case_version(case_state.case_id, version)
    if row is None:
        return None
    return TimelineState.model_validate(row["document_json"])


def _load_memory_summaries(
    connection: Connection,
    case_state: CaseState,
) -> tuple[str, ...]:
    hits = search_relevant_memories(
        connection,
        query=case_state.brief.goal,
        project_id=case_state.project_id,
        limit=5,
    )
    return tuple(
        _memory_summary_line(
            memory_id=hit.memory_id,
            scope=hit.scope,
            project_id=hit.project_id,
            content=hit.content,
        )
        for hit in hits
    )


def _memory_summary_line(
    *,
    memory_id: str,
    scope: str,
    project_id: str | None,
    content: str,
) -> str:
    owner = "user" if scope == "user" else f"project:{project_id}"
    summary = content if len(content) <= 200 else f"{content[:197]}..."
    return f"{owner} {memory_id}: {summary}"


async def _record_incoming_item(
    item: TurnQueueItem,
    *,
    engine: Engine,
    turn_id: str,
) -> None:
    if item.kind == "user_message":
        if item.payload.get("message_recorded") is True:
            return
        content = str(item.payload.get("content", ""))
        message_id = str(item.payload.get("message_id") or item.item_id or f"msg_{turn_id}_user")
        with begin_immediate(engine) as connection:
            MessagesRepository(connection).insert(
                {
                    "message_id": message_id,
                    "case_id": item.case_id,
                    "role": "user",
                    "kind": "user",
                    "content": content,
                    "created_at": item.enqueued_at,
                }
            )
    event = item.payload.get("event")
    if isinstance(event, Mapping):
        _apply_events((dict(event),), engine=engine, base_version=None, actor="job")


def _replay_tool_call_from_item(item: TurnQueueItem) -> ToolCall | None:
    if item.kind != "ui_observation":
        return None
    if item.payload.get("observation_type") != "replay_pending_tool_call":
        return None
    pending_payload = item.payload.get("pending_tool_call")
    if not isinstance(pending_payload, Mapping):
        return None
    pending = PendingToolCall.model_validate(pending_payload)
    return ToolCall(
        tool_name=pending.tool_name,
        arguments=pending.arguments,
        tool_call_id=str(
            item.payload.get("replayed_tool_call_id")
            or pending.original_tool_call_id
            or _replayed_tool_call_id(str(item.payload.get("decision_id", "unknown")))
        ),
        idempotency_key=pending.idempotency_key,
    )


def _execute_tool(
    router: ToolRouter,
    tool_call: ToolCall,
    *,
    engine: Engine,
    state: _LoadedState,
    turn_id: str,
    gateway: Any | None = None,
) -> ToolResult:
    with engine.connect() as connection:
        context = ToolExecutionContext(
            tool_call_id=tool_call.tool_call_id or _tool_call_id(tool_call),
            turn_id=turn_id,
            case_state=state.case_state,
            project_state=state.project_state,
            decisions=state.decisions,
            readonly_connection=connection,
            created_at=_now_iso(),
            metadata=_tool_context_metadata(engine, gateway),
        )
        try:
            return router.execute(tool_call, context)
        except Exception as exc:  # pragma: no cover - defensive harness boundary
            return ToolResult(
                tool_call_id=context.tool_call_id,
                tool_name=tool_call.tool_name,
                status="failed",
                observation=str(exc),
                error=ToolError(
                    error_code="tool_handler_exception",
                    message=str(exc),
                    retryable=False,
                    details={"exception_type": type(exc).__name__},
                ),
            )


def _defer_tool_call(
    registered: Any,
    tool_call: ToolCall,
    arguments: Mapping[str, Any],
    *,
    state: _LoadedState,
    turn_id: str,
    engine: Engine | None = None,
) -> ToolResult:
    spec = registered.spec
    kind = _job_kind_for_tool(spec.name, spec.namespace)
    job_arguments = dict(arguments)
    if spec.name == "audio.asr_original" and not job_arguments.get("asset_id"):
        asset_id = _asset_id_for_asr(state.case_state)
        if asset_id is not None:
            job_arguments["asset_id"] = asset_id
    idempotency_key = tool_call.idempotency_key or _job_idempotency_key(
        kind=kind,
        tool_name=spec.name,
        case_id=state.case_state.case_id,
        arguments=job_arguments,
    )
    job_id = _job_id(kind=kind, idempotency_key=idempotency_key)
    # 幂等短路：同参数 job 已有终态时直接报告结果，不再重复入队——
    # 否则 agent 只能拿到"job queued"并反复重试（M9 路径 1 实测）
    if engine is not None:
        finished = _finished_job_observation(engine, job_id, tool_name=spec.name)
        if finished is not None:
            return ToolResult(
                tool_call_id=tool_call.tool_call_id or _tool_call_id(tool_call),
                tool_name=spec.name,
                status=finished[0],
                observation=finished[1],
                data={"job_id": job_id, "job_kind": kind},
            )
    if spec.name == "asset.import_url":
        job_arguments.setdefault(
            "asset_id",
            _asset_id_for_url_import(state.case_state.project_id, job_arguments),
        )
        job_payload = {
            **job_arguments,
            "tool_name": spec.name,
            "tool_call_id": tool_call.tool_call_id or _tool_call_id(tool_call),
            "turn_id": turn_id,
        }
    else:
        job_payload = {
            "tool_name": spec.name,
            "arguments": job_arguments,
            "tool_call_id": tool_call.tool_call_id or _tool_call_id(tool_call),
            "turn_id": turn_id,
        }
    project_level_job = kind in {"proxy", "import_url"}
    event = JobEnqueued(
        job_id=job_id,
        project_id=state.case_state.project_id,
        case_id=None if project_level_job else state.case_state.case_id,
        payload={
            "kind": kind,
            "idempotency_key": idempotency_key,
            "asset_id": job_arguments.get("asset_id"),
            "job_payload": job_payload,
            "tool_name": spec.name,
            "tool_call_id": job_payload["tool_call_id"],
            "attempts": 0,
            "max_retries": 2,
        },
    )
    return ToolResult(
        tool_call_id=str(job_payload["tool_call_id"]),
        tool_name=spec.name,
        status="running",
        observation=f"job queued: {job_id}",
        data={"job_id": job_id, "job_kind": kind, "idempotency_key": idempotency_key},
        events=[event.model_dump(mode="json")],
    )


def _persist_tool_result_data(result: ToolResult, *, engine: Engine) -> None:
    row = result.data.get("message_row")
    if not isinstance(row, Mapping):
        return
    with begin_immediate(engine) as connection:
        MessagesRepository(connection).insert(dict(row))


def _apply_events(
    events: Sequence[DomainEventBase | Mapping[str, Any]],
    *,
    engine: Engine,
    base_version: int | None,
    actor: Actor,
) -> ReducerApplyResult:
    if not events:
        return ReducerApplyResult(status="applied")
    event_payloads = [
        event.model_dump(mode="json") if isinstance(event, DomainEventBase) else dict(event)
        for event in events
    ]
    return apply(
        event_payloads,
        engine=engine,
        base_version=base_version,
        actor=actor,
    )


async def _handle_followups(
    result: ReducerApplyResult,
    *,
    engine: Engine,
    router: ToolRouter,
    turn_queue: TurnQueue | None,
    turn_id: str,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
) -> list[str]:
    if result.status != "applied":
        return []
    enqueued: list[str] = []
    for followup in result.followups:
        if followup.kind == "replay_pending_tool_call":
            if turn_queue is None:
                continue
            replay = _consume_pending_replay(engine, followup.decision_id)
            if replay is None:
                continue
            case_id, pending, replayed_tool_call_id = replay
            await turn_queue.enqueue_ui_observation(
                case_id,
                observation_type="replay_pending_tool_call",
                payload={
                    "decision_id": followup.decision_id,
                    "pending_tool_call": pending.model_dump(mode="json"),
                    "replayed_tool_call_id": replayed_tool_call_id,
                },
                item_id=followup.decision_id,
            )
            enqueued.append(followup.decision_id)
        elif followup.kind == "enqueue_memory_save":
            _execute_memory_save_followup(
                followup,
                engine=engine,
                router=router,
                turn_id=turn_id,
                accumulator=accumulator,
                tracer=tracer,
            )
        elif followup.kind == "discard_memory_candidate":
            _discard_memory_candidate_followup(
                followup,
                engine=engine,
                accumulator=accumulator,
                tracer=tracer,
            )
    return enqueued


def _execute_memory_save_followup(
    followup: HarnessFollowup,
    *,
    engine: Engine,
    router: ToolRouter,
    turn_id: str,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
) -> None:
    """Execute memory.save directly after commit.

    PRD calls this an enqueue step, but memory.save is a millisecond DB event
    generator. Keeping it in-process avoids the render/annotation job table while
    preserving the reducer boundary: the handler emits MemorySaved, then the
    harness applies that event in a separate transaction.
    """

    candidate_id = followup.payload.get("candidate_id")
    scope = followup.payload.get("scope")
    case_id = followup.payload.get("case_id")
    if not isinstance(candidate_id, str) or scope not in {"user", "project"}:
        tracer.record(
            "action",
            {
                "stage": "followup",
                "kind": followup.kind,
                "decision_id": followup.decision_id,
                "status": "failed",
                "observation": "invalid memory save followup payload",
            },
        )
        return
    if not isinstance(case_id, str):
        tracer.record(
            "action",
            {
                "stage": "followup",
                "kind": followup.kind,
                "decision_id": followup.decision_id,
                "status": "failed",
                "observation": "memory save followup missing case_id",
            },
        )
        return
    tool_call = ToolCall(
        tool_name="memory.save",
        arguments={"candidate_id": candidate_id, "scope": scope},
        tool_call_id=f"followup_{followup.decision_id}_memory_save",
    )
    accumulator.tool_calls.append(tool_call)
    tracer.record(
        "action",
        {
            "stage": "followup",
            "kind": followup.kind,
            "tool_call": tool_call.model_dump(mode="json"),
        },
    )
    try:
        state = _load_state(engine, case_id)
        result = _execute_tool(router, tool_call, engine=engine, state=state, turn_id=turn_id)
    except Exception as exc:  # pragma: no cover - defensive post-commit boundary
        result = ToolResult(
            tool_call_id=tool_call.tool_call_id or "followup_memory_save",
            tool_name=tool_call.tool_name,
            status="failed",
            observation=f"memory.save followup failed: {exc}",
            error=ToolError(
                error_code="memory_save_followup_failed",
                message=str(exc),
                retryable=False,
                details={"exception_type": type(exc).__name__},
            ),
        )
    accumulator.tool_results.append(result)
    tracer.record("tool_result", _tool_result_payload(result))
    reducer_result = _apply_events(result.events, engine=engine, base_version=None, actor="system")
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload(result.events, reducer_result))
    if result.status != "succeeded" or reducer_result.status != "applied":
        tracer.record(
            "action",
            {
                "stage": "followup",
                "kind": followup.kind,
                "decision_id": followup.decision_id,
                "status": "failed",
                "observation": result.observation,
                "reducer_status": reducer_result.status,
            },
        )


def _discard_memory_candidate_followup(
    followup: HarnessFollowup,
    *,
    engine: Engine,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
) -> None:
    candidate_id = followup.payload.get("candidate_id")
    case_id = followup.payload.get("case_id")
    if not isinstance(candidate_id, str):
        tracer.record(
            "action",
            {
                "stage": "followup",
                "kind": followup.kind,
                "decision_id": followup.decision_id,
                "status": "failed",
                "observation": "invalid memory discard followup payload",
            },
        )
        return
    event = MemoryCandidateDiscarded(
        candidate_id=candidate_id,
        case_id=case_id if isinstance(case_id, str) else None,
        payload={"candidate_id": candidate_id},
    )
    result = ToolResult(
        tool_call_id=f"followup_{followup.decision_id}_memory_discard",
        tool_name="memory.discard_candidate",
        status="succeeded",
        observation="经验候选已丢弃。",
        data={"candidate_id": candidate_id},
        events=[event.model_dump(mode="json")],
    )
    accumulator.tool_results.append(result)
    tracer.record(
        "action",
        {
            "stage": "followup",
            "kind": followup.kind,
            "decision_id": followup.decision_id,
            "candidate_id": candidate_id,
        },
    )
    tracer.record("tool_result", _tool_result_payload(result))
    reducer_result = _apply_events(result.events, engine=engine, base_version=None, actor="system")
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload(result.events, reducer_result))


def _consume_pending_replay(
    engine: Engine,
    decision_id: str,
) -> tuple[str, PendingToolCall, str] | None:
    with begin_immediate(engine) as connection:
        row = DecisionsRepository(connection).get(decision_id)
        if row is None:
            return None
        decision = Decision.model_validate(row)
        pending = next_replay(decision)
        target_case_id = decision.case_id or _case_id_for_pending_replay(
            connection,
            pending,
            decision.project_id,
        )
        if pending is None or target_case_id is None:
            return None
        replayed_tool_call_id = _replayed_tool_call_id(decision.decision_id)
        if not mark_replayed(
            DecisionsRepository(connection),
            decision.decision_id,
            replayed_tool_call_id=replayed_tool_call_id,
        ):
            return None
        return target_case_id, pending, replayed_tool_call_id


def _case_id_for_pending_replay(
    connection: Connection,
    pending: PendingToolCall | None,
    project_id: str | None,
) -> str | None:
    if pending is not None:
        case_id = pending.arguments.get("case_id")
        if isinstance(case_id, str) and _case_exists_for_replay(connection, case_id, project_id):
            return case_id
    if project_id is None:
        return None
    row = connection.execute(
        select(schema.cases.c.case_id)
        .where(schema.cases.c.project_id == project_id)
        .where(schema.cases.c.status == "active")
        .order_by(schema.cases.c.case_id)
        .limit(1)
    ).first()
    if row is None:
        return None
    return str(row._mapping["case_id"])


def _case_exists_for_replay(
    connection: Connection,
    case_id: str,
    project_id: str | None,
) -> bool:
    statement = select(schema.cases.c.case_id).where(
        schema.cases.c.case_id == case_id,
        schema.cases.c.status == "active",
    )
    if project_id is not None:
        statement = statement.where(schema.cases.c.project_id == project_id)
    return connection.execute(statement).first() is not None


def _force_reply(
    *,
    engine: Engine,
    state: _LoadedState,
    turn_id: str,
    message: str,
    reason: str,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
    turn_listener: TurnListener | None = None,
) -> None:
    """Harness 兜底收尾：直接写 assistant reply 行 + TurnEnded，不经 router。"""

    step = PlannerStep(content=message)
    tracer.record("action", {**step.model_dump(), "forced": True, "reason": reason})
    message_id = _next_message_id(turn_id, accumulator)
    _persist_assistant_message(
        engine,
        case_id=state.case_state.case_id,
        message_id=message_id,
        content=message,
        kind="reply",
    )
    _emit_turn_event(
        turn_listener,
        {
            "type": "message_completed",
            "message_id": message_id,
            "kind": "reply",
            "content": message,
        },
    )
    end_event = TurnEnded(
        turn_id=turn_id,
        case_id=state.case_state.case_id,
        project_id=state.case_state.project_id,
        payload={"reason": reason},
    )
    reducer_result = _apply_events(
        (end_event,),
        engine=engine,
        base_version=state.case_state.state_version,
        actor="agent",
    )
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload((end_event,), reducer_result))


def _format_message_id(turn_id: str, seq: int) -> str:
    return f"msg_{turn_id.replace(':', '_')}_{seq}"


def _next_message_id(turn_id: str, accumulator: _RunAccumulator) -> str:
    accumulator.message_seq += 1
    return _format_message_id(turn_id, accumulator.message_seq)


def _peek_next_message_id(turn_id: str, accumulator: _RunAccumulator) -> str:
    """message_id the *next* persisted assistant message will use.

    Streaming deltas are emitted before ``plan()`` returns, so the loop must
    predict the id up front; it matches ``_next_message_id`` because persisting
    the content increments ``message_seq`` to exactly this value.
    """

    return _format_message_id(turn_id, accumulator.message_seq + 1)


def _persist_assistant_message(
    engine: Engine,
    *,
    case_id: str,
    message_id: str,
    content: str,
    kind: str,
) -> None:
    with begin_immediate(engine) as connection:
        MessagesRepository(connection).insert(
            {
                "message_id": message_id,
                "case_id": case_id,
                "role": "assistant",
                "kind": kind,
                "content": content,
                "created_at": _now_iso(),
            }
        )


def _synthetic_tool_result(
    tool_call: ToolCall,
    status: Literal["failed", "requires_user"],
    observation: str,
) -> ToolResult:
    return ToolResult(
        tool_call_id=tool_call.tool_call_id or _tool_call_id(tool_call),
        tool_name=tool_call.tool_name,
        status=status,
        observation=observation,
    )


def _verdict_payload(verdict: Verdict) -> dict[str, Any]:
    return {
        "status": verdict.status,
        "reason": verdict.reason,
        "tool_call": None
        if verdict.tool_call is None
        else verdict.tool_call.model_dump(mode="json"),
        "event_count": len(verdict.events),
        "validated_arguments": dict(verdict.validated_arguments),
    }


def _tool_result_payload(result: ToolResult) -> dict[str, Any]:
    return {
        "tool_call_id": result.tool_call_id,
        "tool_name": result.tool_name,
        "status": result.status,
        "observation": result.observation,
        "artifact_count": len(result.artifacts),
        "event_count": len(result.events),
        "error": None if result.error is None else result.error.model_dump(mode="json"),
        "data_keys": sorted(result.data),
    }


def _events_payload(
    events: Sequence[DomainEventBase | Mapping[str, Any]],
    reducer_result: ReducerApplyResult,
) -> dict[str, Any]:
    return {
        "events": [
            event.model_dump(mode="json") if isinstance(event, DomainEventBase) else dict(event)
            for event in events
        ],
        "reducer": {
            "status": reducer_result.status,
            "applied_events": [
                {
                    "event_id": applied.event_id,
                    "event_type": applied.event_type,
                    "state_version": applied.state_version,
                }
                for applied in reducer_result.applied_events
            ],
            "skipped_events": reducer_result.skipped_events,
            "followups": [
                {
                    "kind": followup.kind,
                    "decision_id": followup.decision_id,
                    "payload": followup.payload,
                }
                for followup in reducer_result.followups
            ],
        },
    }


def _reducer_diagnostic(result: ReducerApplyResult) -> str:
    if result.conflict is not None:
        return (
            f"version_conflict: expected={result.conflict.expected_base_version}, "
            f"actual={result.conflict.actual_state_version}, event={result.conflict.event_type}"
        )
    if result.validation_failed is not None:
        summary = "; ".join(
            f"{violation.code}: {violation.message}"
            for violation in result.validation_failed.violations
        )
        return f"validation_failed: {summary}"
    return result.status


def _tool_call_id(tool_call: ToolCall) -> str:
    return f"tc_{tool_call.tool_name.replace('.', '_')}_{abs(hash(str(tool_call.arguments)))}"


def _job_kind_for_tool(tool_name: str, namespace: str) -> str:
    explicit = {
        "annotation.enqueue": "annotation",
        "audio.asr_original": "asr",
        "asr.transcribe": "asr",
        "tts.speech": "tts",
        "render.preview": "render_preview",
        "render.final_mp4": "render_final",
        "proxy.generate": "proxy",
        "align.transcript": "align",
        "asset.import_url": "import_url",
        "noop": "noop",
    }
    if tool_name in explicit:
        return explicit[tool_name]
    if namespace in {"annotation", "asr", "tts", "proxy", "align"}:
        return namespace
    return tool_name.replace(".", "_")


def _asset_id_for_asr(case_state: CaseState) -> str | None:
    if case_state.audio_plan is not None and case_state.audio_plan.source_asset_ids:
        return case_state.audio_plan.source_asset_ids[0]
    if case_state.selected_asset_ids:
        return case_state.selected_asset_ids[0]
    return None


def _tool_context_metadata(engine: Engine, gateway: Any | None = None) -> dict[str, Any]:
    metadata: dict[str, Any] = {}
    database = engine.url.database
    if database is not None and database != ":memory:":
        metadata["workspace_path"] = str(Path(database).parent)
    if gateway is not None:
        # 工具经此调 LLM/VLM/embedding；不注入则全部降级（M9 实测）
        metadata["provider_gateway"] = gateway
    return metadata


def _job_idempotency_key(
    *,
    kind: str,
    tool_name: str,
    case_id: str,
    arguments: Mapping[str, Any],
) -> str:
    encoded = json.dumps(
        {
            "kind": kind,
            "tool_name": tool_name,
            "case_id": case_id,
            "arguments": arguments,
        },
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def _job_id(*, kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


def _asset_id_for_url_import(project_id: str, arguments: Mapping[str, Any]) -> str:
    existing = arguments.get("asset_id")
    if isinstance(existing, str) and existing:
        return existing
    url = arguments.get("url")
    digest = hashlib.sha256(f"{project_id}:{url}".encode()).hexdigest()
    return f"asset_{digest[:20]}"


def _replayed_tool_call_id(decision_id: str) -> str:
    return f"replay_{decision_id}"


def _degradation_id(turn_id: str, decision_id: str) -> str:
    digest = hashlib.sha256(f"{turn_id}:{decision_id}:decision_answering".encode()).hexdigest()
    return f"degraded_{digest[:20]}"


def _turn_id(item: TurnQueueItem) -> str:
    suffix = item.item_id or item.enqueued_at.replace(":", "_")
    return f"turn_{item.case_id}_{item.kind}_{suffix}"


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
