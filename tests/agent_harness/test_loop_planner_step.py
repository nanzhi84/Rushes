"""PlannerStep 协议核心语义：content 即散文，工具调用与叙述并行。"""

from pathlib import Path

from sqlalchemy.engine import Engine

from agent_harness.loop import PlannerStep, ScriptedPlanner, run_turn
from agent_harness.turn_queue import TurnQueueItem
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import CasesRepository, MessagesRepository
from storage.repositories.event_log import EventLogRepository
from storage.repositories.projects import ProjectsRepository

NOW = "2026-07-06T00:00:00+00:00"


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


def _assistant_messages(engine: Engine) -> list[dict[str, object]]:
    with begin_immediate(engine) as connection:
        rows = MessagesRepository(connection).list_for_case("case_1")
    return [row for row in rows if row["role"] == "assistant"]


def _turn_ended_reasons(engine: Engine) -> list[str]:
    with begin_immediate(engine) as connection:
        rows = EventLogRepository(connection).read_after(0)
    reasons: list[str] = []
    for row in rows:
        if row.event_type != "TurnEnded":
            continue
        payload = row.payload_json.get("payload")
        if isinstance(payload, dict):
            reasons.append(str(payload.get("reason")))
    return reasons


async def test_pure_content_ends_turn_and_persists_reply_row(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "hi"}),
        engine=engine,
        planner=ScriptedPlanner([{"content": "已完成剪辑目标确认。"}]),
        turn_id="turn_pure",
    )

    assert result.outcome == "finished"
    assert result.tool_calls == ()
    replies = _assistant_messages(engine)
    assert len(replies) == 1
    assert replies[0]["kind"] == "reply"
    assert replies[0]["content"] == "已完成剪辑目标确认。"
    assert replies[0]["message_id"] == "msg_turn_pure_1"
    assert _turn_ended_reasons(engine) == ["reply"]


async def test_content_with_tool_call_persists_narration_and_continues(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "content": "我先展示一下当前进度。",
                    "tool_call": {
                        "tool_name": "interaction.show_progress",
                        "arguments": {"title": "step 1"},
                    },
                },
                {"content": "进度已展示，本回合结束。"},
            ]
        ),
        turn_id="turn_narration",
    )

    assert result.outcome == "finished"
    assert [call.tool_name for call in result.tool_calls] == ["interaction.show_progress"]
    assert result.tool_results[0].status == "succeeded"
    messages = _assistant_messages(engine)
    assert [(row["kind"], row["content"]) for row in messages] == [
        ("narration", "我先展示一下当前进度。"),
        ("reply", "进度已展示，本回合结束。"),
    ]
    assert messages[0]["message_id"] == "msg_turn_narration_1"
    assert messages[1]["message_id"] == "msg_turn_narration_2"
    assert _turn_ended_reasons(engine) == ["reply"]


async def test_empty_steps_reach_illegal_output_limit_and_force_reply(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "??"}),
        engine=engine,
        planner=ScriptedPlanner([PlannerStep(), PlannerStep()]),
        turn_id="turn_empty",
        max_illegal_outputs=2,
    )

    assert result.outcome == "forced_end"
    assert result.forced_reason == "illegal_output_limit"
    replies = _assistant_messages(engine)
    assert len(replies) == 1
    assert replies[0]["kind"] == "reply"
    assert "连续 2 次" in str(replies[0]["content"])
    assert _turn_ended_reasons(engine) == ["illegal_output_limit"]


async def test_hard_attempt_limit_forces_reply_row(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    calls = [
        {"tool_name": "interaction.show_progress", "arguments": {"title": f"step {index}"}}
        for index in range(12)
    ]

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "loop"}),
        engine=engine,
        planner=ScriptedPlanner(calls),
        turn_id="turn_hard_limit",
        max_nonblocking_tools=99,
    )

    assert result.outcome == "forced_end"
    assert result.forced_reason == "hard_attempt_limit"
    assert len(result.tool_results) == 12
    replies = _assistant_messages(engine)
    assert len(replies) == 1
    assert replies[0]["kind"] == "reply"
    assert "12 次上限" in str(replies[0]["content"])
    assert _turn_ended_reasons(engine) == ["hard_attempt_limit"]
