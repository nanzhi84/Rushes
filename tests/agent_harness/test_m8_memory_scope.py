from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from agent_harness.loop import ScriptedPlanner, _load_state, run_turn
from agent_harness.reducer import apply
from agent_harness.turn_queue import TurnQueueItem
from contracts.case import CaseState
from contracts.provider import ProviderResult
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import CasesRepository, DecisionsRepository, ProjectsRepository
from storage.repositories._json import dump_json
from storage.repositories.event_log import EventLogRepository
from tools import ToolExecutionContext
from tools.memory_tools import ask_scope, extract_from_case, save
from tools.specs import MemoryAskScopeInput, MemoryExtractFromCaseInput, MemorySaveInput

NOW = "2026-07-05T00:00:00+00:00"


class _GatewayResult:
    def __init__(self, content: str) -> None:
        self.result = ProviderResult(
            provider_id="mock_llm",
            capability="llm.chat",
            request_id="memory_extract_mock",
            model="mock",
            latency_ms=1,
            normalized_output={"content": content},
        )
        self.events: tuple[dict[str, Any], ...] = ()


class _Gateway:
    def __init__(self, content: str) -> None:
        self.content = content

    async def call(self, request: object, *, provider_id: str | None = None) -> _GatewayResult:
        del request, provider_id
        return _GatewayResult(self.content)


def _prepare_workspace(tmp_path: Path) -> Engine:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    with begin_immediate(engine) as connection:
        ProjectsRepository(connection).insert(_project("project_1", "Project A"))
        ProjectsRepository(connection).insert(_project("project_2", "Project B"))
        CasesRepository(connection).insert(_case("case_1", "project_1", goal="护肤口播"))
        CasesRepository(connection).insert(_case("case_2", "project_2", goal="护肤口播"))
        connection.execute(
            schema.objects.insert().values(
                hash="hash_export",
                rel_path="exports/case_1.mp4",
                size=10,
                created_at=NOW,
            )
        )
        connection.execute(
            schema.exports.insert().values(
                export_id="export_1",
                case_id="case_1",
                timeline_version=1,
                object_hash="hash_export",
                quality=dump_json({"quality": "high"}),
                created_at=NOW,
            )
        )
    return engine


def _project(project_id: str, name: str) -> dict[str, object]:
    return {
        "project_id": project_id,
        "name": name,
        "status": "active",
        "defaults": {"aspect_ratio": "9:16", "fps": 30},
        "created_at": NOW,
        "updated_at": NOW,
    }


def _case(case_id: str, project_id: str, *, goal: str) -> dict[str, object]:
    return {
        "case_id": case_id,
        "project_id": project_id,
        "name": case_id,
        "state_version": 0,
        "status": "active",
        "pending_decision_id": None,
        "running_jobs": [],
        "last_error": None,
        "brief": {"goal": goal, "confirmed_facts": []},
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
        "scratch_memory": {"tone": "前三秒直接给结论"},
    }


@contextmanager
def _context(
    engine: Engine,
    *,
    case_id: str = "case_1",
    tool_call_id: str = "tool_call_1",
    gateway: object | None = None,
) -> Iterator[ToolExecutionContext]:
    connection = engine.connect()
    try:
        case = CasesRepository(connection).get(case_id)
        assert case is not None
        metadata = {"provider_gateway": gateway} if gateway is not None else {}
        yield ToolExecutionContext(
            tool_call_id=tool_call_id,
            turn_id="turn_1",
            case_state=CaseState.model_validate(case),
            readonly_connection=connection,
            created_at=NOW,
            metadata=metadata,
        )
    finally:
        connection.close()


def _apply(
    engine: Engine,
    events: list[dict[str, Any]],
    *,
    base_version: int | None = None,
) -> None:
    result = apply(events, engine=engine, base_version=base_version, actor="agent")
    assert result.status == "applied"


def _candidate(engine: Engine, candidate_id: str) -> dict[str, Any]:
    with begin_immediate(engine) as connection:
        row = connection.execute(
            select(schema.memory_candidates).where(
                schema.memory_candidates.c.candidate_id == candidate_id
            )
        ).one()
    return dict(row._mapping)


def _memory_count(engine: Engine) -> int:
    with begin_immediate(engine) as connection:
        return int(
            connection.execute(select(func.count()).select_from(schema.memories)).scalar_one()
        )


def _event_types(engine: Engine) -> list[str]:
    with begin_immediate(engine) as connection:
        return [row.event_type for row in EventLogRepository(connection).read_after(0)]


async def test_memory_scope_save_and_skip_paths(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    gateway = _Gateway("项目内护肤口播：前三秒直接给结论，再接产品使用过程。")
    with _context(engine, gateway=gateway) as context:
        extracted = extract_from_case(
            MemoryExtractFromCaseInput(summary_hint="沉淀这次护肤口播经验"),
            context,
        )
    candidate_id = str(extracted.data["candidate_id"])
    _apply(engine, extracted.events)

    assert _candidate(engine, candidate_id)["status"] == "pending"
    assert "MemoryCandidateExtracted" in _event_types(engine)

    with _context(engine) as context:
        asked = ask_scope(MemoryAskScopeInput(candidate_id=candidate_id), context)
    _apply(engine, asked.events, base_version=0)
    decision_id = str(asked.data["decision_id"])
    with begin_immediate(engine) as connection:
        decision = DecisionsRepository(connection).get(decision_id)
    assert decision is not None
    assert decision["type"] == "memory_scope"
    assert decision["options"][0]["payload"]["candidate_id"] == candidate_id

    await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "project"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "tool_name": "decision.answer",
                    "arguments": {
                        "decision_id": decision_id,
                        "answer": {"option_id": "project", "answered_via": "button"},
                    },
                }
            ]
        ),
        turn_id="turn_memory_project",
    )

    saved_candidate = _candidate(engine, candidate_id)
    assert saved_candidate["status"] == "saved"
    assert saved_candidate["saved_memory_id"] is not None
    with begin_immediate(engine) as connection:
        memory = connection.execute(select(schema.memories)).one()
    assert memory._mapping["scope"] == "project"
    assert memory._mapping["project_id"] == "project_1"
    assert "MemorySaved" in _event_types(engine)

    with _context(engine, tool_call_id="dup_save") as context:
        duplicate = save(MemorySaveInput(candidate_id=candidate_id, scope="project"), context)
    assert duplicate.status == "failed"
    assert _memory_count(engine) == 1

    with _context(engine, tool_call_id="extract_skip") as context:
        skipped = extract_from_case(
            MemoryExtractFromCaseInput(summary_hint="跳过这条候选"),
            context,
        )
    skip_candidate_id = str(skipped.data["candidate_id"])
    _apply(engine, skipped.events)
    with _context(engine, tool_call_id="ask_skip") as context:
        skip_ask = ask_scope(MemoryAskScopeInput(candidate_id=skip_candidate_id), context)
    current_version = _load_state(engine, "case_1").case_state.state_version
    _apply(engine, skip_ask.events, base_version=current_version)
    skip_decision_id = str(skip_ask.data["decision_id"])

    await run_turn(
        TurnQueueItem(case_id="case_1", kind="user_message", payload={"content": "skip"}),
        engine=engine,
        planner=ScriptedPlanner(
            [
                {
                    "tool_name": "decision.answer",
                    "arguments": {
                        "decision_id": skip_decision_id,
                        "answer": {"option_id": "skip", "answered_via": "button"},
                    },
                }
            ]
        ),
        turn_id="turn_memory_skip",
    )

    assert _candidate(engine, skip_candidate_id)["status"] == "discarded"
    assert _memory_count(engine) == 1
    assert "MemoryCandidateDiscarded" in _event_types(engine)


def test_project_memory_does_not_cross_project(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.memories.insert().values(
                memory_id="mem_project_a",
                scope="project",
                project_id="project_1",
                content="护肤口播开头必须先说适用肤质。",
                tags="[]",
                created_from_case_id="case_1",
                created_at=NOW,
            )
        )

    loaded_b = _load_state(engine, "case_2")

    assert all("mem_project_a" not in item for item in loaded_b.memory_summaries)


def test_user_memory_injects_across_projects_by_relevance(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.memories.insert().values(
                memory_id="mem_user_1",
                scope="user",
                project_id=None,
                content="护肤口播用户偏好：开头直接给结论，不要铺垫太久。",
                tags="[]",
                created_from_case_id="case_1",
                created_at=NOW,
            )
        )

    loaded_a = _load_state(engine, "case_1")
    loaded_b = _load_state(engine, "case_2")

    assert any("mem_user_1" in item for item in loaded_a.memory_summaries)
    assert any("mem_user_1" in item for item in loaded_b.memory_summaries)


def test_extract_from_case_falls_back_without_gateway(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    with _context(engine) as context:
        result = extract_from_case(
            MemoryExtractFromCaseInput(summary_hint="没有 provider gateway 也要沉淀"),
            context,
        )

    assert result.status == "succeeded"
    assert result.data["content"]
    assert [event["event"] for event in result.events] == [
        "CapabilityDegraded",
        "MemoryCandidateExtracted",
    ]
