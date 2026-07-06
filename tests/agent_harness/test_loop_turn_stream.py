"""run_turn 向 TurnListener 发射的事件序列（快照/流式的唯一数据源）。"""

from __future__ import annotations

from collections.abc import Mapping
from pathlib import Path
from typing import Any

from pydantic import BaseModel, ConfigDict
from sqlalchemy.engine import Engine

from agent_harness.loop import ScriptedPlanner, run_turn
from agent_harness.turn_queue import TurnQueueItem
from contracts.tool import ToolSpec
from contracts.tool_result import ToolResult
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import CasesRepository, MessagesRepository
from storage.repositories.projects import ProjectsRepository
from tools import ToolExecutionContext, build_default_tool_registry
from tools.registry import ToolRegistry

NOW = "2026-07-06T00:00:00+00:00"


class _RecordingListener:
    def __init__(self) -> None:
        self.events: list[dict[str, Any]] = []

    def emit(self, event: Mapping[str, Any]) -> None:
        self.events.append(dict(event))

    def types(self) -> list[str]:
        return [event["type"] for event in self.events]

    def of_type(self, event_type: str) -> list[dict[str, Any]]:
        return [event for event in self.events if event["type"] == event_type]


def _prepare_workspace(tmp_path: Path) -> Engine:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
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
                "state_version": 0,
                "status": "active",
                "pending_decision_id": None,
                "running_jobs": [],
                "last_error": None,
                "brief": {"goal": "test", "confirmed_facts": []},
                "content_plan": None,
                "audio_plan": None,
                "cut_plan": None,
                "candidate_pack_id": None,
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
    return engine


async def test_narration_then_reply_emits_full_ordered_stream(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    listener = _RecordingListener()

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "content": "我先展示进度。",
                    "tool_call": {
                        "tool_name": "interaction.show_progress",
                        "arguments": {"title": "step 1"},
                    },
                },
                {"content": "进度已展示，本回合结束。"},
            ]
        ),
        turn_id="turn_stream",
        turn_listener=listener,
    )

    assert result.outcome == "finished"
    assert listener.types() == [
        "turn_started",
        "text_delta",
        "message_completed",
        "tool_step_started",
        "tool_step_finished",
        "text_delta",
        "message_completed",
        "turn_ended",
    ]

    started = listener.of_type("turn_started")[0]
    assert started["turn_id"] == "turn_stream"

    # 叙述：text_delta 与 message_completed 的 message_id 必须一致（前端据此自愈）。
    narration_delta = listener.of_type("text_delta")[0]
    narration_done = listener.of_type("message_completed")[0]
    assert narration_delta["message_id"] == narration_done["message_id"] == "msg_turn_stream_1"
    assert narration_delta["kind"] == "assistant"
    assert narration_delta["delta"] == "我先展示进度。"
    assert narration_done["kind"] == "narration"
    assert narration_done["content"] == "我先展示进度。"

    # 工具步：started/finished 共享 step_id，状态取自结果。
    step_started = listener.of_type("tool_step_started")[0]
    step_finished = listener.of_type("tool_step_finished")[0]
    assert step_started["tool"] == "interaction.show_progress"
    assert step_started["step_id"] == step_finished["step_id"]
    assert step_finished["status"] == "succeeded"

    # 回复：定型 reply，message_id 与 text_delta 对齐。
    reply_delta = listener.of_type("text_delta")[1]
    reply_done = listener.of_type("message_completed")[1]
    assert reply_delta["message_id"] == reply_done["message_id"] == "msg_turn_stream_2"
    assert reply_done["kind"] == "reply"
    assert reply_done["content"] == "进度已展示，本回合结束。"

    ended = listener.of_type("turn_ended")[0]
    assert ended["outcome"] == "finished"
    assert ended["reason"] is None


async def test_message_id_matches_persisted_row(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    listener = _RecordingListener()

    await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "hi"}),
        engine=engine,
        planner=ScriptedPlanner([{"content": "已完成。"}]),
        turn_id="turn_stream",
        turn_listener=listener,
    )

    with begin_immediate(engine) as connection:
        rows = MessagesRepository(connection).list_for_case("case_1")
    assistant_ids = [row["message_id"] for row in rows if row["role"] == "assistant"]
    completed_ids = [event["message_id"] for event in listener.of_type("message_completed")]
    assert assistant_ids == completed_ids == ["msg_turn_stream_1"]


async def test_denied_tool_emits_started_and_finished_with_deny_status(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    listener = _RecordingListener()

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "?"}),
        engine=engine,
        # 未注册工具 → policy deny；脚本随后耗尽 → 兜底回复收尾。
        planner=ScriptedPlanner([{"tool_name": "shell.exec", "arguments": {}}]),
        turn_id="turn_deny",
        turn_listener=listener,
    )

    assert result.outcome == "finished"
    started = listener.of_type("tool_step_started")[0]
    finished = listener.of_type("tool_step_finished")[0]
    assert started["tool"] == "shell.exec"
    assert started["step_id"] == finished["step_id"]
    assert finished["status"] == "deny"
    assert "turn_ended" in listener.types()


async def test_forced_reply_emits_message_completed(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    listener = _RecordingListener()

    from agent_harness.loop import PlannerStep

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "??"}),
        engine=engine,
        planner=ScriptedPlanner([PlannerStep(), PlannerStep()]),
        turn_id="turn_forced",
        max_illegal_outputs=2,
        turn_listener=listener,
    )

    assert result.outcome == "forced_end"
    completed = listener.of_type("message_completed")
    assert len(completed) == 1
    assert completed[0]["kind"] == "reply"
    assert "连续 2 次" in completed[0]["content"]
    ended = listener.of_type("turn_ended")[0]
    assert ended["outcome"] == "forced_end"
    assert ended["reason"] == "illegal_output_limit"


async def test_listener_exception_never_breaks_turn(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    class _BrokenListener:
        def emit(self, event: Mapping[str, Any]) -> None:
            raise RuntimeError("listener blew up")

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "hi"}),
        engine=engine,
        planner=ScriptedPlanner([{"content": "完成。"}]),
        turn_id="turn_safe",
        turn_listener=_BrokenListener(),
    )

    assert result.outcome == "finished"


# --- async 工具执行路径 + 进度通道 ------------------------------------------


class _EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


def _async_tool_spec(name: str) -> ToolSpec:
    return ToolSpec(
        name=name,
        namespace="x",
        version="1",
        input_model=_EmptyInput,
        result_model=None,
        handler_ref=f"tests.{name}",
        allowed_scopes=["case_agent_console"],
        requires_artifacts=[],
        requires_active_project=False,
        requires_active_case=False,
        side_effects=[],
        emits_events=[],
        description="async test tool",
    )


def _registry_with(name: str, handler: Any) -> ToolRegistry:
    registry = build_default_tool_registry()
    registry.register(_async_tool_spec(name), handler)
    return registry


async def test_async_tool_handler_is_awaited_and_result_flows(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    async def async_handler(input_model: _EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name="x.async_ok",
            status="succeeded",
            observation="async done",
        )

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {"tool_name": "x.async_ok", "arguments": {}},
                {"content": "完成。"},
            ]
        ),
        registry=_registry_with("x.async_ok", async_handler),
        turn_id="turn_async",
    )

    assert result.outcome == "finished"
    tool_result = result.tool_results[-1]
    assert tool_result.status == "succeeded"
    assert tool_result.observation == "async done"


async def test_async_tool_handler_progress_emits_subagent_progress(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    listener = _RecordingListener()

    async def async_handler(input_model: _EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model
        context.metadata["turn_progress"]({"note": "x"})
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name="x.async_progress",
            status="succeeded",
            observation="ok",
        )

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {"tool_name": "x.async_progress", "arguments": {}},
                {"content": "完成。"},
            ]
        ),
        registry=_registry_with("x.async_progress", async_handler),
        turn_id="turn_progress",
        turn_listener=listener,
    )

    assert result.outcome == "finished"
    progress = listener.of_type("subagent_progress")
    assert len(progress) == 1
    assert progress[0]["type"] == "subagent_progress"
    assert progress[0]["note"] == "x"


async def test_async_and_sync_handler_exceptions_are_equivalent(tmp_path: Path) -> None:
    async def _run(name: str, handler: Any) -> ToolResult:
        engine = _prepare_workspace(tmp_path / name)
        outcome = await run_turn(
            TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
            engine=engine,
            planner=ScriptedPlanner([{"tool_name": name, "arguments": {}}]),
            registry=_registry_with(name, handler),
            turn_id=f"turn_{name}",
        )
        return outcome.tool_results[-1]

    def sync_boom(input_model: _EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model, context
        raise RuntimeError("sync boom")

    async def async_boom(input_model: _EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model, context
        raise RuntimeError("async boom")

    sync_result = await _run("x.sync_boom", sync_boom)
    async_result = await _run("x.async_boom", async_boom)

    assert sync_result.status == async_result.status == "failed"
    assert sync_result.error is not None
    assert async_result.error is not None
    assert sync_result.error.error_code == async_result.error.error_code == "tool_handler_exception"
    assert async_result.error.details.get("exception_type") == "RuntimeError"


async def test_turn_progress_is_noop_without_listener(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    async def async_handler(input_model: _EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model
        # 无 turn_listener 时 turn_progress 必须是可调用的 no-op，绝不为 None、绝不抛错。
        callback = context.metadata["turn_progress"]
        assert callable(callback)
        callback({"note": "no listener"})
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name="x.async_noop",
            status="succeeded",
            observation="ok",
        )

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {"tool_name": "x.async_noop", "arguments": {}},
                {"content": "完成。"},
            ]
        ),
        registry=_registry_with("x.async_noop", async_handler),
        turn_id="turn_noop",
    )

    assert result.outcome == "finished"
    assert result.tool_results[-1].status == "succeeded"
