from pathlib import Path

from pydantic import BaseModel, ConfigDict
from sqlalchemy import select, update
from sqlalchemy.engine import Engine

from agent_harness.loop import (
    ScriptedPlanner,
    recover_approved_pending_tool_calls,
    run_turn,
)
from agent_harness.policy_gate import PolicyContext, PolicyGate
from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem
from contracts.case import CaseState
from contracts.decision import Decision
from contracts.tool import ToolSpec
from contracts.tool_result import ToolResult
from domain.preconditions import PreconditionContext
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import (
    CasesRepository,
    DecisionsRepository,
    JobsRepository,
    MessagesRepository,
)
from storage.repositories._json import load_json
from storage.repositories.event_log import EventLogRepository
from storage.repositories.projects import ProjectsRepository
from tools import PATCH_OP_REGISTRY, ToolExecutionContext, build_default_tool_registry

NOW = "2026-07-04T00:00:00+00:00"


class EmptyInput(BaseModel):
    model_config = ConfigDict(extra="forbid")


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
        CasesRepository(connection).insert(_case_values())
    return engine


def _case_values(**overrides: object) -> dict[str, object]:
    values: dict[str, object] = {
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
    values.update(overrides)
    return values


def _load_case(engine: Engine) -> CaseState:
    with begin_immediate(engine) as connection:
        row = CasesRepository(connection).get("case_1")
    assert row is not None
    return CaseState.model_validate(row)


def _event_types(engine: Engine) -> list[str]:
    with begin_immediate(engine) as connection:
        return [row.event_type for row in EventLogRepository(connection).read_after(0)]


def _messages(engine: Engine) -> list[dict[str, object]]:
    with begin_immediate(engine) as connection:
        return MessagesRepository(connection).list_for_case("case_1")


async def test_unregistered_tool_denies_policy_refusal_and_state_unchanged(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(
            case_id="case_1",
            kind="user_message",
            payload={"content": "run shell"},
            item_id="msg_1",
        ),
        engine=engine,
        planner=ScriptedPlanner([{"tool_name": "shell.exec", "arguments": {}}]),
        turn_id="turn_unregistered",
    )

    assert result.tool_results[0].status == "failed"
    assert "PolicyRefusal" in _event_types(engine)
    assert _load_case(engine).state_version == 0


async def test_three_illegal_outputs_force_respond(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "bad"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {"tool_name": "shell.exec", "arguments": {}},
                {"tool_name": "shell.exec", "arguments": {}},
                {"tool_name": "shell.exec", "arguments": {}},
            ]
        ),
        turn_id="turn_illegal",
    )

    assert result.forced_reason == "illegal_output_limit"
    assert result.tool_results[-1].tool_name == "respond"
    assert any("连续 3 次" in str(message["content"]) for message in _messages(engine))


async def test_five_nonblocking_tools_force_progress_response(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    calls = [
        {
            "tool_name": "interaction.show_progress",
            "arguments": {"title": f"step {index}"},
        }
        for index in range(5)
    ]

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "go"}),
        engine=engine,
        planner=ScriptedPlanner(calls),
        turn_id="turn_nonblocking",
    )

    assert result.forced_reason == "nonblocking_tool_limit"
    assert result.tool_results[-1].tool_name == "respond"
    assert any(
        "连续完成 5 个非阻塞步骤" in str(message["content"]) for message in _messages(engine)
    )


async def test_twelve_attempt_hard_limit_forces_diagnostic(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    calls = [
        {
            "tool_name": "interaction.show_progress",
            "arguments": {"title": f"step {index}"},
        }
        for index in range(12)
    ]

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "loop"}),
        engine=engine,
        planner=ScriptedPlanner(calls),
        turn_id="turn_hard_limit",
        max_nonblocking_tools=99,
    )

    assert result.forced_reason == "hard_attempt_limit"
    assert result.tool_results[-1].tool_name == "respond"
    assert any("12 次上限" in str(message["content"]) for message in _messages(engine))


async def test_running_result_ends_turn_until_observation(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    registry = build_default_tool_registry()

    def running_handler(input_model: EmptyInput, context: ToolExecutionContext) -> ToolResult:
        del input_model
        return ToolResult(
            tool_call_id=context.tool_call_id,
            tool_name="x.long",
            status="running",
            observation="job queued",
        )

    registry.register(
        ToolSpec(
            name="x.long",
            namespace="x",
            version="1",
            input_model=EmptyInput,
            result_model=None,
            handler_ref="tests.long",
            allowed_scopes=["case_agent_console"],
            requires_artifacts=[],
            requires_active_project=False,
            requires_active_case=False,
            side_effects=["job"],
            emits_events=[],
            is_long_running=True,
            description="long",
        ),
        running_handler,
    )

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "long"}),
        engine=engine,
        planner=ScriptedPlanner([{"tool_name": "x.long", "arguments": {}}]),
        registry=registry,
        turn_id="turn_running",
    )

    assert result.outcome == "running"
    assert result.tool_results[-1].status == "running"
    job_id = str(result.tool_results[-1].data["job_id"])
    with begin_immediate(engine) as connection:
        job = JobsRepository(connection).get(job_id)
    assert job is not None
    assert job["kind"] == "x_long"
    assert job["status"] == "pending"
    assert "JobEnqueued" in _event_types(engine)
    assert _load_case(engine).running_jobs[0].job_id == job_id


async def test_stop_token_ends_after_current_tool(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    token = StopToken(cancel_requested=True)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "stop"}),
        engine=engine,
        planner=ScriptedPlanner(
            [{"tool_name": "interaction.show_progress", "arguments": {"title": "done"}}]
        ),
        stop_token=token,
        turn_id="turn_stop",
    )

    assert result.outcome == "stopped"
    assert result.tool_results[-1].tool_name == "finish_turn"


async def test_decision_answer_reduces_pending_and_restores_allowed_tools(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_pending_generic_decision(engine)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "yes"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "tool_name": "decision.answer",
                    "arguments": {
                        "decision_id": "decision_1",
                        "answer": {
                            "option_id": "yes",
                            "answered_via": "button",
                            "payload": {
                                "reduce_target": "scratch_memory",
                                "value": "confirmed",
                            },
                        },
                    },
                }
            ]
        ),
        turn_id="turn_answer",
    )

    case_state = _load_case(engine)
    assert result.outcome == "finished"
    assert case_state.pending_decision_id is None
    assert case_state.scratch_memory["decision_1"] == "confirmed"

    registry = build_default_tool_registry()
    policy_gate = PolicyGate(
        tool_specs=registry.specs_by_name(),
        patch_op_specs=PATCH_OP_REGISTRY.as_mapping(),
    )
    allowed = policy_gate.compute_allowed_tools(
        PolicyContext(
            preconditions=PreconditionContext(case_state=case_state),
            decisions=(),
        )
    )
    assert "interaction.confirm_action" in {spec.name for spec in allowed}


async def test_ask_user_creates_blocking_decision_and_narrows_tools(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)

    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "ask"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "tool_name": "interaction.ask_user",
                    "arguments": {
                        "question": "需要确认什么？",
                        "type": "generic",
                        "reduce_target": "scratch_memory",
                    },
                }
            ]
        ),
        turn_id="turn_ask",
    )

    case_state = _load_case(engine)
    decision = _load_decision(engine, case_state.pending_decision_id)
    registry = build_default_tool_registry()
    policy_gate = PolicyGate(
        tool_specs=registry.specs_by_name(),
        patch_op_specs=PATCH_OP_REGISTRY.as_mapping(),
    )
    allowed = policy_gate.compute_allowed_tools(
        PolicyContext(
            preconditions=PreconditionContext(case_state=case_state),
            decisions=(decision,),
        )
    )
    names = {spec.name for spec in allowed}

    assert result.outcome == "requires_user"
    assert decision.blocking
    assert "decision.answer" in names
    assert "respond" in names
    assert "interaction.confirm_action" not in names


async def test_approved_pending_tool_call_recovery_enqueues_once(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_approved_replay_decision(engine)
    handled: list[str] = []

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        handled.append(str(item.payload["decision_id"]))
        await run_turn(
            item,
            engine=engine,
            planner=ScriptedPlanner([]),
            turn_id=f"turn_replay_{item.item_id}",
        )

    queue = TurnQueue(runner)
    first = await recover_approved_pending_tool_calls(
        engine=engine,
        turn_queue=queue,
        created_at=NOW,
    )
    second = await recover_approved_pending_tool_calls(
        engine=engine,
        turn_queue=queue,
        created_at=NOW,
    )
    await queue.join_all()
    await queue.shutdown()

    decision = _load_decision(engine, "decision_replay")
    assert first == 1
    assert second == 0
    assert handled == ["decision_replay"]
    assert decision.pending_tool_call_status == "replayed"


async def test_decision_answer_followup_enqueues_pending_tool_call_once(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_pending_replay_decision(engine)
    handled: list[str] = []

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        handled.append(str(item.payload["decision_id"]))

    queue = TurnQueue(runner)
    result = await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "approve"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "tool_name": "decision.answer",
                    "arguments": {
                        "decision_id": "decision_pending_replay",
                        "answer": {
                            "option_id": "approve",
                            "answered_via": "button",
                            "payload": {"approved": True},
                        },
                    },
                }
            ]
        ),
        turn_queue=queue,
        turn_id="turn_followup",
    )
    await queue.join_all()
    await queue.shutdown()

    decision = _load_decision(engine, "decision_pending_replay")
    assert result.replays_enqueued == ("decision_pending_replay",)
    assert handled == ["decision_pending_replay"]
    assert decision.pending_tool_call_status == "replayed"
    assert _load_case(engine).pending_decision_id is None


async def test_agent_trace_records_five_kinds_for_turn(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)

    await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "hi"}),
        engine=engine,
        planner=ScriptedPlanner([{"tool_name": "respond", "arguments": {"message": "hello"}}]),
        turn_id="turn_trace",
    )

    with begin_immediate(engine) as connection:
        rows = connection.execute(
            select(schema.agent_traces.c.kind)
            .where(schema.agent_traces.c.turn_id == "turn_trace")
            .order_by(schema.agent_traces.c.seq)
        ).all()
    assert [row._mapping["kind"] for row in rows] == [
        "context",
        "action",
        "gate",
        "tool_result",
        "events",
    ]


def _insert_pending_generic_decision(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        DecisionsRepository(connection).insert(
            {
                "decision_id": "decision_1",
                "scope_type": "case",
                "project_id": "project_1",
                "case_id": "case_1",
                "type": "generic",
                "question": "Confirm?",
                "options": [],
                "status": "pending",
                "answer": None,
                "pending_tool_call": None,
                "pending_tool_call_status": None,
                "consumed_at": None,
                "replayed_tool_call_id": None,
                "blocking": True,
                "created_by_tool_call_id": None,
            }
        )
        connection.execute(
            update(schema.cases)
            .where(schema.cases.c.case_id == "case_1")
            .values(pending_decision_id="decision_1")
        )


def _insert_approved_replay_decision(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        DecisionsRepository(connection).insert(
            {
                "decision_id": "decision_replay",
                "scope_type": "case",
                "project_id": "project_1",
                "case_id": "case_1",
                "type": "export",
                "question": "Replay?",
                "options": [],
                "status": "answered",
                "answer": {"option_id": "approve", "answered_via": "button"},
                "pending_tool_call": {
                    "tool_name": "interaction.show_progress",
                    "arguments": {"title": "replayed"},
                    "idempotency_key": "idem",
                    "argument_fingerprint": "fp",
                    "original_tool_call_id": "tc_original",
                },
                "pending_tool_call_status": "approved",
                "consumed_at": None,
                "replayed_tool_call_id": None,
                "blocking": False,
                "created_by_tool_call_id": None,
            }
        )


def _insert_pending_replay_decision(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        DecisionsRepository(connection).insert(
            {
                "decision_id": "decision_pending_replay",
                "scope_type": "case",
                "project_id": "project_1",
                "case_id": "case_1",
                "type": "export",
                "question": "Export?",
                "options": [],
                "status": "pending",
                "answer": None,
                "pending_tool_call": {
                    "tool_name": "interaction.show_progress",
                    "arguments": {"title": "approved replay"},
                    "idempotency_key": "idem_pending",
                    "argument_fingerprint": "fp_pending",
                    "original_tool_call_id": "tc_original_pending",
                },
                "pending_tool_call_status": "pending",
                "consumed_at": None,
                "replayed_tool_call_id": None,
                "blocking": True,
                "created_by_tool_call_id": None,
            }
        )
        connection.execute(
            update(schema.cases)
            .where(schema.cases.c.case_id == "case_1")
            .values(pending_decision_id="decision_pending_replay")
        )


def _load_decision(engine: Engine, decision_id: str | None) -> Decision:
    assert decision_id is not None
    with begin_immediate(engine) as connection:
        row = DecisionsRepository(connection).get(decision_id)
    assert row is not None
    return Decision.model_validate(row)


def test_message_content_is_decoded_as_text(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    with begin_immediate(engine) as connection:
        MessagesRepository(connection).insert(
            {
                "message_id": "msg_raw",
                "case_id": "case_1",
                "role": "user",
                "content": "hello",
                "created_at": NOW,
            }
        )
        row = connection.execute(
            select(schema.messages.c.content).where(schema.messages.c.message_id == "msg_raw")
        ).scalar_one()

    assert load_json(row) == "hello"
