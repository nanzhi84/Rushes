"""Single-turn harness loop."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, Protocol

from sqlalchemy import select
from sqlalchemy.engine import Connection, Engine

from contracts.case import CaseState
from contracts.decision import Decision, PendingToolCall
from contracts.events import Actor, DomainEventBase
from contracts.project import ProjectState
from contracts.timeline import TimelineState
from contracts.tool import PatchOpSpec, ToolSpec
from contracts.tool_result import ToolError, ToolResult
from domain.preconditions import PreconditionContext
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

from .context_builder import ContextBuilder, ContextBuildInput, ContextBundle, ContextMessage
from .policy_gate import PolicyContext, PolicyGate, ToolCall, Verdict, mark_replayed, next_replay
from .reducer import ReducerApplyResult, apply
from .tool_router import ToolRouter
from .trace import NullTraceRecorder, TraceRecorder
from .turn_queue import StopToken, TurnQueue, TurnQueueItem

DEFAULT_MAX_ILLEGAL_OUTPUTS = 3
DEFAULT_MAX_NONBLOCKING_TOOLS = 5
DEFAULT_MAX_TOOL_ATTEMPTS = 12


class LLMPlanner(Protocol):
    async def plan(
        self,
        context: ContextBundle,
        tools: Sequence[ToolSpec],
    ) -> ToolCall:
        """Return exactly one native tool call."""


class ScriptedPlanner:
    """Deterministic planner used by loop tests."""

    def __init__(self, calls: Sequence[ToolCall | Mapping[str, Any]]) -> None:
        self._calls = [ToolCall.from_input(call) for call in calls]
        self._index = 0

    async def plan(self, context: ContextBundle, tools: Sequence[ToolSpec]) -> ToolCall:
        del context, tools
        if self._index >= len(self._calls):
            return ToolCall(
                tool_name="finish_turn",
                arguments={"reason": "script exhausted"},
                tool_call_id=f"scripted_{self._index}",
            )
        call = self._calls[self._index]
        self._index += 1
        if call.tool_call_id is not None:
            return call
        return call.model_copy(update={"tool_call_id": f"scripted_{self._index}"})

    @property
    def calls_remaining(self) -> int:
        return max(0, len(self._calls) - self._index)


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
    decisions: tuple[Decision, ...]
    pending_decision: Decision | None
    messages: tuple[ContextMessage, ...]
    timeline: TimelineState | None


@dataclass(slots=True)
class _RunAccumulator:
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_results: list[ToolResult] = field(default_factory=list)
    reducer_results: list[ReducerApplyResult] = field(default_factory=list)
    replays_enqueued: list[str] = field(default_factory=list)


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
    max_illegal_outputs: int = DEFAULT_MAX_ILLEGAL_OUTPUTS,
    max_nonblocking_tools: int = DEFAULT_MAX_NONBLOCKING_TOOLS,
    max_tool_attempts: int = DEFAULT_MAX_TOOL_ATTEMPTS,
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
    await _record_incoming_item(item, engine=engine, turn_id=active_turn_id)

    replay_call = _replay_tool_call_from_item(item)
    illegal_outputs = 0
    nonblocking_tools = 0
    attempts = 0
    outcome = "finished"
    forced_reason: str | None = None

    while True:
        if attempts >= max_tool_attempts:
            forced_reason = "hard_attempt_limit"
            result = _force_respond(
                router,
                engine=engine,
                state=loaded,
                turn_id=active_turn_id,
                message=(
                    "本回合工具调用达到 12 次上限，已停止继续执行；请缩小请求或补充更明确的目标。"
                ),
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
            )
            accumulator.tool_results.append(result)
            outcome = "forced_end"
            break

        loaded = _load_state(engine, item.case_id)
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
            },
        )
        tool_call = replay_call or await planner.plan(context_bundle, context_bundle.allowed_tools)
        replay_call = None
        attempts += 1
        accumulator.tool_calls.append(tool_call)
        tracer.record("action", {"tool_call": tool_call.model_dump(mode="json")})

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
            illegal_outputs += 1
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
            if illegal_outputs >= max_illegal_outputs:
                forced_reason = "illegal_output_limit"
                result = _force_respond(
                    router,
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
                )
                accumulator.tool_results.append(result)
                outcome = "forced_end"
                break
            continue

        illegal_outputs = 0
        if verdict.status == "ask":
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

        result = _execute_tool(
            router,
            tool_call,
            engine=engine,
            state=loaded,
            turn_id=active_turn_id,
        )
        accumulator.tool_results.append(result)
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
            forced = _force_respond(
                router,
                engine=engine,
                state=loaded,
                turn_id=active_turn_id,
                message=f"状态写入被拒绝，已停止本回合。{diagnostic}",
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
            )
            accumulator.tool_results.append(forced)
            outcome = "forced_end"
            break
        enqueued = await _handle_followups(
            reducer_result,
            engine=engine,
            turn_queue=turn_queue,
        )
        accumulator.replays_enqueued.extend(enqueued)

        if result.status == "running":
            outcome = "running"
            break
        if result.status == "requires_user":
            outcome = "requires_user"
            break
        if result.tool_name in {"respond", "refuse", "finish_turn"}:
            outcome = "finished"
            break
        if token.cancel_requested:
            forced_reason = "stopped_by_user"
            forced = _force_finish(
                router,
                engine=engine,
                state=_load_state(engine, item.case_id),
                turn_id=active_turn_id,
                reason=forced_reason,
                accumulator=accumulator,
                tracer=tracer,
            )
            accumulator.tool_results.append(forced)
            outcome = "stopped"
            break
        nonblocking_tools += 1
        if nonblocking_tools >= max_nonblocking_tools:
            forced_reason = "nonblocking_tool_limit"
            forced = _force_respond(
                router,
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
            )
            accumulator.tool_results.append(forced)
            outcome = "forced_end"
            break

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
    """Scan approved outbox decisions and enqueue case-level replays once."""

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
            if pending is None or decision.case_id is None:
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
                    decision.case_id,
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
    )


def _build_context(policy_gate: PolicyGate, loaded: _LoadedState) -> ContextBundle:
    builder = ContextBuilder(policy_gate=policy_gate)
    return builder.build(
        ContextBuildInput(
            preconditions=context_bundle_input_preconditions(loaded),
            decisions=loaded.decisions,
            pending_decision=loaded.pending_decision,
            timeline=loaded.timeline,
            messages=loaded.messages,
        )
    )


def _load_state(engine: Engine, case_id: str) -> _LoadedState:
    with engine.connect() as connection:
        case_row = CasesRepository(connection).get(case_id)
        if case_row is None:
            raise ValueError(f"case not found: {case_id}")
        case_state = CaseState.model_validate(case_row)
        project_state = _load_project_state(connection, case_state.project_id)
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
    return _LoadedState(
        case_state=case_state,
        project_state=project_state,
        decisions=decisions,
        pending_decision=pending_decision,
        messages=messages,
        timeline=timeline,
    )


def _load_project_state(connection: Connection, project_id: str) -> ProjectState | None:
    row = ProjectsRepository(connection).get(project_id)
    return None if row is None else ProjectState.model_validate(row)


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


async def _record_incoming_item(
    item: TurnQueueItem,
    *,
    engine: Engine,
    turn_id: str,
) -> None:
    if item.kind == "user_message":
        content = str(item.payload.get("content", ""))
        message_id = str(item.payload.get("message_id") or item.item_id or f"msg_{turn_id}_user")
        with begin_immediate(engine) as connection:
            MessagesRepository(connection).insert(
                {
                    "message_id": message_id,
                    "case_id": item.case_id,
                    "role": "user",
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
) -> ToolResult:
    with engine.connect() as connection:
        context = ToolExecutionContext(
            tool_call_id=tool_call.tool_call_id or _tool_call_id(tool_call),
            turn_id=turn_id,
            case_state=state.case_state,
            project_state=state.project_state,
            decisions=state.decisions,
            readonly_connection=connection,
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
    turn_queue: TurnQueue | None,
) -> list[str]:
    if result.status != "applied" or turn_queue is None:
        return []
    enqueued: list[str] = []
    for followup in result.followups:
        if followup.kind != "replay_pending_tool_call":
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
    return enqueued


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
        if pending is None or decision.case_id is None:
            return None
        replayed_tool_call_id = _replayed_tool_call_id(decision.decision_id)
        if not mark_replayed(
            DecisionsRepository(connection),
            decision.decision_id,
            replayed_tool_call_id=replayed_tool_call_id,
        ):
            return None
        return decision.case_id, pending, replayed_tool_call_id


def _force_respond(
    router: ToolRouter,
    *,
    engine: Engine,
    state: _LoadedState,
    turn_id: str,
    message: str,
    reason: str,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
) -> ToolResult:
    call = ToolCall(
        tool_name="respond",
        arguments={"message": message},
        tool_call_id=f"{turn_id}_{reason}",
    )
    accumulator.tool_calls.append(call)
    tracer.record("action", {"tool_call": call.model_dump(mode="json"), "forced": True})
    tracer.record("gate", {"status": "allow", "reason": reason, "forced": True})
    result = _execute_tool(router, call, engine=engine, state=state, turn_id=turn_id)
    tracer.record("tool_result", _tool_result_payload(result))
    _persist_tool_result_data(result, engine=engine)
    reducer_result = _apply_events(
        result.events,
        engine=engine,
        base_version=state.case_state.state_version,
        actor="agent",
    )
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload(result.events, reducer_result))
    return result


def _force_finish(
    router: ToolRouter,
    *,
    engine: Engine,
    state: _LoadedState,
    turn_id: str,
    reason: str,
    accumulator: _RunAccumulator,
    tracer: TraceRecorder | NullTraceRecorder,
) -> ToolResult:
    call = ToolCall(
        tool_name="finish_turn",
        arguments={"reason": reason},
        tool_call_id=f"{turn_id}_{reason}",
    )
    accumulator.tool_calls.append(call)
    tracer.record("action", {"tool_call": call.model_dump(mode="json"), "forced": True})
    tracer.record("gate", {"status": "allow", "reason": reason, "forced": True})
    result = _execute_tool(router, call, engine=engine, state=state, turn_id=turn_id)
    tracer.record("tool_result", _tool_result_payload(result))
    reducer_result = _apply_events(
        result.events,
        engine=engine,
        base_version=state.case_state.state_version,
        actor="agent",
    )
    accumulator.reducer_results.append(reducer_result)
    tracer.record("events", _events_payload(result.events, reducer_result))
    return result


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


def _replayed_tool_call_id(decision_id: str) -> str:
    return f"replay_{decision_id}"


def _turn_id(item: TurnQueueItem) -> str:
    suffix = item.item_id or item.enqueued_at.replace(":", "_")
    return f"turn_{item.case_id}_{item.kind}_{suffix}"


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
