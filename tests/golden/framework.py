"""Golden replay framework for PRD §4.7."""

from __future__ import annotations

import re
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from pydantic import BaseModel, ConfigDict
from sqlalchemy.engine import Engine

from agent_harness.loop import LLMPlanner, PlannerStep, run_turn
from agent_harness.policy_gate import ToolCall
from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem
from contracts.case import CaseState
from contracts.provider import ProviderDescriptor
from contracts.tool import ToolSpec
from contracts.tool_result import ToolResult
from providers.capabilities import LLM_CHAT, ProviderRequest
from providers.gateway import ProviderGateway
from providers.mock import MockProvider
from providers.registry import ProviderRegistry
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import CasesRepository, EventLogRepository, MessagesRepository
from tools import ToolExecutionContext, ToolRegistry, build_default_tool_registry

NOW = "2026-07-04T00:00:00+00:00"


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


@dataclass(frozen=True, slots=True)
class ExpectedToolTrace:
    tool_name: str = "*"
    status: str = "*"


@dataclass(frozen=True, slots=True)
class GoldenAssertions:
    assert_final: Callable[[GoldenRunResult], None]


@dataclass(frozen=True, slots=True)
class GoldenCase:
    name: str
    build_workspace: Callable[[Engine], None]
    user_messages: Sequence[str]
    provider_script: Sequence[dict[str, Any]]
    expected_tool_trace: Sequence[ExpectedToolTrace]
    assertions: GoldenAssertions
    registry_factory: Callable[[], ToolRegistry] = build_default_tool_registry


@dataclass(frozen=True, slots=True)
class ToolTraceEntry:
    tool_name: str
    status: str
    observation: str


@dataclass(frozen=True, slots=True)
class GoldenRunResult:
    engine: Engine
    traces: tuple[ToolTraceEntry, ...]
    case_state: CaseState
    event_types: tuple[str, ...]
    messages: tuple[dict[str, object], ...]


class GatewayPlanner(LLMPlanner):
    def __init__(self, gateway: ProviderGateway) -> None:
        self._gateway = gateway

    async def plan(
        self,
        context: Any,
        tools: Sequence[ToolSpec],
        *,
        on_delta: Callable[[str], None] | None = None,
    ) -> PlannerStep:
        response = await self._gateway.call(
            ProviderRequest(
                capability=LLM_CHAT,
                payload={
                    "blocks": context.blocks,
                    "tools": [tool.name for tool in tools],
                },
            )
        )
        output = response.result.normalized_output
        raw_content = output.get("content")
        content = raw_content if isinstance(raw_content, str) and raw_content else None
        if on_delta is not None and content:
            on_delta(content)
        raw_tool_call = output.get("tool_call")
        if not isinstance(raw_tool_call, Mapping):
            return PlannerStep(content=content or "（无工具调用，结束本回合）")
        return PlannerStep(
            content=content,
            tool_call=ToolCall.from_input(_resolve_placeholders(dict(raw_tool_call), context)),
        )


class GoldenExecutor:
    async def run(self, case: GoldenCase, tmp_path: Path) -> GoldenRunResult:
        workspace = tmp_path / case.name
        engine = create_workspace_engine(workspace)
        with engine.begin() as connection:
            schema.create_all(connection)
        case.build_workspace(engine)
        provider = MockProvider(
            scripts={
                LLM_CHAT: list(case.provider_script),
            }
        )
        provider_registry = ProviderRegistry()
        provider_registry.register(
            ProviderDescriptor(
                provider_id="mock",
                display_name="Mock",
                version="1",
                capabilities=[LLM_CHAT],
                config_model=EmptyInput,
                client_ref="providers.mock.MockProvider",
                supports_json_schema=True,
            ),
            provider,
        )
        planner = GatewayPlanner(ProviderGateway(registry=provider_registry))
        tool_traces: list[ToolTraceEntry] = []

        async def runner(item: TurnQueueItem, token: StopToken) -> None:
            del token
            result = await run_turn(
                item,
                engine=engine,
                planner=planner,
                registry=case.registry_factory(),
                turn_id=f"golden_{case.name}_{item.item_id}",
            )
            tool_traces.extend(_trace_entries(result.tool_results))

        queue = TurnQueue(runner)
        for index, message in enumerate(case.user_messages):
            await queue.enqueue_user_message(
                "case_1",
                content=message,
                message_id=f"{case.name}_msg_{index}",
            )
            await queue.join_case("case_1")
        await queue.shutdown()

        run_result = GoldenRunResult(
            engine=engine,
            traces=tuple(tool_traces),
            case_state=_load_case(engine),
            event_types=_event_types(engine),
            messages=tuple(_messages(engine)),
        )
        _assert_trace(case.expected_tool_trace, run_result.traces)
        case.assertions.assert_final(run_result)
        return run_result


def base_workspace(engine: Engine, *, state_version: int = 0) -> None:
    from storage.repositories import CasesRepository
    from storage.repositories.projects import ProjectsRepository

    with begin_immediate(engine) as connection:
        ProjectsRepository(connection).insert(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "defaults": {"aspect_ratio": "9:16", "fps": 30},
                "created_at": NOW,
                "updated_at": NOW,
            }
        )
        CasesRepository(connection).insert(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "state_version": state_version,
                "status": "active",
                "pending_decision_id": None,
                "running_jobs": [],
                "last_error": None,
                "brief": {"goal": "test", "confirmed_facts": []},
                "content_plan": None,
                "audio_plan": None,
                "cut_plan": None,
                "timeline_current_version": None,
                "timeline_validated": False,
                "preview_current_id": None,
                "last_viewed_preview_id": None,
                "rough_cut_approved": False,
                "rough_cut_approved_version": None,
                "postprocess_plan": None,
                "export_current_id": None,
                "selected_asset_ids": [],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        )


def registry_with_timeline_tools() -> ToolRegistry:
    registry = build_default_tool_registry()
    registry.register(
        ToolSpec(
            name="test.create_timeline_stale",
            namespace="test",
            version="1",
            input_model=EmptyInput,
            result_model=None,
            handler_ref="tests.golden.stale",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["timeline"],
            emits_events=["TimelineVersionCreated"],
            description="Emit a stale strict timeline event.",
        ),
        _stale_timeline_handler,
    )
    registry.register(
        ToolSpec(
            name="test.create_timeline",
            namespace="test",
            version="1",
            input_model=EmptyInput,
            result_model=None,
            handler_ref="tests.golden.current",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["timeline"],
            emits_events=["TimelineVersionCreated"],
            description="Emit a current strict timeline event.",
        ),
        _current_timeline_handler,
    )
    return registry


def _stale_timeline_handler(input_model: EmptyInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    return _timeline_result(context, base_version=0, version=1)


def _current_timeline_handler(input_model: EmptyInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    return _timeline_result(context, base_version=None, version=1)


def _timeline_result(
    context: ToolExecutionContext,
    *,
    base_version: int | None,
    version: int,
) -> ToolResult:
    assert context.case_state is not None
    event: dict[str, Any] = {
        "event": "TimelineVersionCreated",
        "case_id": context.case_state.case_id,
        "timeline_version": version,
        "payload": {"timeline_version": version},
    }
    if base_version is not None:
        event["base_version"] = base_version
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="test.create_timeline",
        status="succeeded",
        observation="timeline created",
        events=[event],
    )


def _trace_entries(results: Sequence[ToolResult]) -> list[ToolTraceEntry]:
    return [
        ToolTraceEntry(
            tool_name=result.tool_name,
            status=result.status,
            observation=result.observation,
        )
        for result in results
    ]


def _assert_trace(expected: Sequence[ExpectedToolTrace], actual: Sequence[ToolTraceEntry]) -> None:
    assert len(actual) == len(expected)
    for expected_entry, actual_entry in zip(expected, actual, strict=True):
        if expected_entry.tool_name != "*":
            assert actual_entry.tool_name == expected_entry.tool_name
        if expected_entry.status != "*":
            assert actual_entry.status == expected_entry.status


def _load_case(engine: Engine) -> CaseState:
    with begin_immediate(engine) as connection:
        row = CasesRepository(connection).get("case_1")
    assert row is not None
    return CaseState.model_validate(row)


def _event_types(engine: Engine) -> tuple[str, ...]:
    with begin_immediate(engine) as connection:
        rows = EventLogRepository(connection).read_after(0)
    return tuple(row.event_type for row in rows)


def _messages(engine: Engine) -> list[dict[str, object]]:
    with begin_immediate(engine) as connection:
        return MessagesRepository(connection).list_for_case("case_1")


def _resolve_placeholders(tool_call: dict[str, Any], context: Any) -> dict[str, Any]:
    decision_id = _pending_decision_id(context)
    if decision_id is None:
        return tool_call
    arguments = tool_call.get("arguments")
    if isinstance(arguments, dict):
        tool_call["arguments"] = _replace_value(arguments, "__pending_decision__", decision_id)
    return tool_call


def _replace_value(value: Any, old: str, new: str) -> Any:
    if value == old:
        return new
    if isinstance(value, dict):
        return {key: _replace_value(child, old, new) for key, child in value.items()}
    if isinstance(value, list):
        return [_replace_value(child, old, new) for child in value]
    return value


def _pending_decision_id(context: Any) -> str | None:
    blocks = getattr(context, "blocks", {})
    block = blocks.get("pending_decision") if isinstance(blocks, dict) else None
    if not isinstance(block, str):
        return None
    match = re.search(r"^pending_decision:\s+(\S+)$", block, flags=re.MULTILINE)
    return None if match is None else match.group(1)
