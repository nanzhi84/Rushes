"""One internal tool execution path shared by Agent, decision replay, and REST."""

from __future__ import annotations

import asyncio
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from pydantic import ValidationError
from sqlalchemy.engine import Engine

from contracts.decision import Decision
from contracts.draft import DraftState
from contracts.events import Actor, DomainEventBase
from contracts.tool_result import ToolError, ToolResult
from tools import ToolExecutionContext, ToolRegistry

from .policy_gate import PolicyContext, PolicyGate, ToolCall, Verdict
from .reducer import ReducerApplyResult, apply
from .tool_router import ToolRouter

ProgressCallback = Callable[[Mapping[str, Any]], None]


@dataclass(frozen=True, slots=True)
class InternalToolExecution:
    verdict: Verdict
    result: ToolResult | None = None
    reducer_result: ReducerApplyResult | None = None
    partial_reducer_results: tuple[ReducerApplyResult, ...] = ()


async def execute_internal_tool(
    tool_call: ToolCall,
    *,
    engine: Engine,
    registry: ToolRegistry,
    router: ToolRouter,
    policy_gate: PolicyGate,
    policy_context: PolicyContext,
    draft_state: DraftState,
    decisions: tuple[Decision, ...],
    turn_id: str,
    actor: Actor,
    base_version: int | None,
    gateway: Any | None = None,
    turn_progress: ProgressCallback | None = None,
    stop_token: Any | None = None,
    workspace_paths: Any | None = None,
    include_harness_only: bool = False,
) -> InternalToolExecution:
    """Gate, run, validate, persist rows, and apply events in one ordered function."""

    effective_context = policy_context
    if include_harness_only:
        effective_context = policy_context.model_copy(
            update={
                "allowed_tools": tuple(
                    policy_gate.compute_allowed_tools(
                        policy_context,
                        include_harness_only=True,
                    )
                )
            }
        )
    verdict = policy_gate.adjudicate(tool_call, effective_context)
    if verdict.status != "allow":
        return InternalToolExecution(verdict=verdict)

    partial_results: list[ReducerApplyResult] = []
    current_version = [base_version]
    metadata = _tool_metadata(
        engine,
        gateway=gateway,
        turn_progress=turn_progress,
        stop_token=stop_token,
        workspace_paths=workspace_paths,
    )
    metadata["partial_result_sink"] = _make_partial_result_sink(
        engine,
        base_version,
        actor,
        partial_results,
        current_version,
    )
    with engine.connect() as connection:
        context = ToolExecutionContext(
            tool_call_id=tool_call.tool_call_id or _tool_call_id(tool_call),
            turn_id=turn_id,
            draft_state=draft_state,
            decisions=decisions,
            readonly_connection=connection,
            created_at=_now_iso(),
            metadata=metadata,
        )
        try:
            outcome = router.execute(tool_call, context)
            result = outcome if isinstance(outcome, ToolResult) else await outcome
        except Exception as exc:  # defensive single boundary for every invoker
            result = ToolResult(
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

    result = _validate_result_model(result, registry)
    invalid_result = (
        result.status == "failed"
        and result.error is not None
        and result.error.error_code == "invalid_tool_result"
    )
    if not invalid_result:
        reducer_result = apply_tool_events(
            result.events,
            engine=engine,
            base_version=current_version[0],
            actor=actor,
            rows=result.data,
        )
    else:
        reducer_result = ReducerApplyResult(status="applied")
    return InternalToolExecution(
        verdict=verdict,
        result=result,
        reducer_result=reducer_result,
        partial_reducer_results=tuple(partial_results),
    )


def execute_internal_tool_sync(*args: Any, **kwargs: Any) -> InternalToolExecution:
    """Thread-friendly REST bridge; callers must not invoke it on a running event loop."""

    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(execute_internal_tool(*args, **kwargs))
    raise RuntimeError("execute_internal_tool_sync cannot run inside an active event loop")


def apply_tool_events(
    events: Sequence[DomainEventBase | Mapping[str, Any]],
    *,
    engine: Engine,
    base_version: int | None,
    actor: Actor,
    rows: Mapping[str, Any] | None = None,
) -> ReducerApplyResult:
    if not events and not rows:
        return ReducerApplyResult(status="applied")
    payloads = [
        event.model_dump(mode="json") if isinstance(event, DomainEventBase) else dict(event)
        for event in events
    ]
    return apply(
        payloads,
        engine=engine,
        base_version=base_version,
        actor=actor,
        result_rows=rows,
    )


def _make_partial_result_sink(
    engine: Engine,
    base_version: int | None,
    actor: Actor,
    sink_results: list[ReducerApplyResult],
    current_version: list[int | None] | None = None,
) -> Callable[[Mapping[str, Any], Sequence[DomainEventBase | Mapping[str, Any]]], None]:
    def _sink(
        rows: Mapping[str, Any],
        events: Sequence[DomainEventBase | Mapping[str, Any]],
    ) -> None:
        version_ref = current_version if current_version is not None else [base_version]
        result = apply_tool_events(
            events,
            engine=engine,
            base_version=version_ref[0],
            actor=actor,
            rows=rows,
        )
        sink_results.append(result)
        if result.status != "applied":
            event_names = ", ".join(
                event.event if isinstance(event, DomainEventBase) else str(event.get("event"))
                for event in events
            )
            raise RuntimeError(
                f"partial_result_sink 归约未落库：status={result.status} events=[{event_names}]"
            )
        versions = set(result.draft_state_versions.values())
        if len(versions) == 1:
            version_ref[0] = versions.pop()

    return _sink


def _validate_result_model(result: ToolResult, registry: ToolRegistry) -> ToolResult:
    if result.status == "failed":
        return result
    spec = registry.require(result.tool_name).spec
    if spec.result_model is None:
        return result
    try:
        spec.result_model.model_validate(result.data)
    except ValidationError as exc:
        return ToolResult(
            tool_call_id=result.tool_call_id,
            tool_name=result.tool_name,
            status="failed",
            observation="工具返回结果不符合 result_model 契约。",
            error=ToolError(
                error_code="invalid_tool_result",
                message="工具返回结果不符合 result_model 契约。",
                retryable=False,
                details={"validation_errors": exc.errors(include_url=False)},
            ),
        )
    return result


def _tool_metadata(
    engine: Engine,
    *,
    gateway: Any | None,
    turn_progress: ProgressCallback | None,
    stop_token: Any | None,
    workspace_paths: Any | None,
) -> dict[str, Any]:
    metadata: dict[str, Any] = {}
    database = engine.url.database
    if database is not None and database != ":memory:":
        metadata["workspace_path"] = str(Path(database).parent)
    if workspace_paths is not None:
        metadata["workspace_paths"] = workspace_paths
    if gateway is not None:
        metadata["provider_gateway"] = gateway
    metadata["turn_progress"] = turn_progress or (lambda _payload: None)
    if stop_token is not None:
        metadata["stop_token"] = stop_token
    return metadata


def _tool_call_id(tool_call: ToolCall) -> str:
    return f"tc_{tool_call.tool_name.replace('.', '_')}_{abs(hash(str(tool_call.arguments)))}"


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
