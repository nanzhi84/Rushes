"""memory 工具守卫分支与 search_relevant 的作用域过滤。"""

from __future__ import annotations

from pathlib import Path

from sqlalchemy.engine import Engine

from contracts.case import CaseState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.memory_tools import handlers
from tools.specs import (
    MemoryAskScopeInput,
    MemoryExtractFromCaseInput,
    MemorySaveInput,
    MemorySearchRelevantInput,
)
from tools.timeline_tools import handlers as _  # noqa: F401  # 确保包导入顺序稳定

NOW = "2026-07-06T00:00:00+00:00"


def _engine(tmp_path: Path) -> Engine:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        for project_id in ("project_1", "project_2"):
            connection.execute(
                schema.projects.insert().values(
                    project_id=project_id,
                    name=project_id,
                    status="active",
                    defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                    created_at=NOW,
                    updated_at=NOW,
                )
            )
        connection.execute(
            schema.cases.insert().values(
                case_id="case_1",
                project_id="project_1",
                name="Case",
                state_version=0,
                status="active",
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
    return engine


def _case_state() -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": {"mode": "silent"},
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _context(connection=None, *, case_state=None) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=case_state,
        readonly_connection=connection,
        created_at=NOW,
    )


def _seed_memory(
    connection,
    memory_id: str,
    *,
    scope: str,
    project_id: str | None,
    content: str,
) -> None:
    connection.execute(
        schema.memories.insert().values(
            memory_id=memory_id,
            scope=scope,
            project_id=project_id,
            content=content,
            tags="[]",
            created_from_case_id=None,
            created_at=NOW,
        )
    )


def test_memory_handlers_guard_missing_case_and_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.connect() as connection:
        no_case = handlers.extract_from_case(MemoryExtractFromCaseInput(), _context(connection))
        assert no_case.error is not None
        assert no_case.error.error_code == "missing_case"
        no_case_ask = handlers.ask_scope(
            MemoryAskScopeInput(candidate_id="memcand_x"), _context(connection)
        )
        assert no_case_ask.error is not None
        assert no_case_ask.error.error_code == "missing_case"

    state = _case_state()
    no_conn = handlers.extract_from_case(MemoryExtractFromCaseInput(), _context(case_state=state))
    assert no_conn.error is not None
    assert no_conn.error.error_code == "missing_connection"
    no_conn_ask = handlers.ask_scope(
        MemoryAskScopeInput(candidate_id="memcand_x"), _context(case_state=state)
    )
    assert no_conn_ask.error is not None
    assert no_conn_ask.error.error_code == "missing_connection"
    no_conn_save = handlers.save(
        MemorySaveInput(candidate_id="memcand_x", scope="user"), _context(case_state=state)
    )
    assert no_conn_save.error is not None
    assert no_conn_save.error.error_code == "missing_connection"
    no_conn_search = handlers.search_relevant(
        MemorySearchRelevantInput(query="q"), _context(case_state=state)
    )
    assert no_conn_search.error is not None
    assert no_conn_search.error.error_code == "missing_connection"


def test_ask_scope_and_save_reject_missing_or_settled_candidates(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    state = _case_state()
    with engine.begin() as connection:
        context = _context(connection, case_state=state)
        not_found = handlers.ask_scope(MemoryAskScopeInput(candidate_id="memcand_ghost"), context)
        assert not_found.error is not None
        assert not_found.error.error_code == "candidate_not_found"
        save_not_found = handlers.save(
            MemorySaveInput(candidate_id="memcand_ghost", scope="user"), context
        )
        assert save_not_found.error is not None
        assert save_not_found.error.error_code == "candidate_not_found"

        connection.execute(
            schema.memory_candidates.insert().values(
                candidate_id="memcand_done",
                case_id="case_1",
                content="已保存过的候选",
                suggested_scope="project",
                status="saved",
                created_at=NOW,
            )
        )
        settled_ask = handlers.ask_scope(MemoryAskScopeInput(candidate_id="memcand_done"), context)
        assert settled_ask.error is not None
        settled_save = handlers.save(
            MemorySaveInput(candidate_id="memcand_done", scope="project"), context
        )
        assert settled_save.error is not None
        assert settled_save.error.error_code == "candidate_not_pending"


def test_search_relevant_scopes_and_truncates(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    state = _case_state()
    long_text = "口播剪辑经验：" + "节奏优先，" * 60
    with engine.begin() as connection:
        _seed_memory(
            connection, "mem_user", scope="user", project_id=None, content="用户级经验：口播节奏"
        )
        _seed_memory(
            connection, "mem_p1", scope="project", project_id="project_1", content=long_text
        )
        _seed_memory(
            connection,
            "mem_p2",
            scope="project",
            project_id="project_2",
            content="口播 其他项目经验",
        )
        context = _context(connection, case_state=state)

        default = handlers.search_relevant(MemorySearchRelevantInput(query="口播"), context)
        ids = {item["memory_id"] for item in default.data["memories"]}
        assert "mem_p2" not in ids  # project memory 不跨 project
        assert {"mem_user", "mem_p1"} <= ids
        p1_hit = next(item for item in default.data["memories"] if item["memory_id"] == "mem_p1")
        assert len(p1_hit["summary"]) <= 201  # 截断（含省略号）
        assert "找到" in default.observation

        user_only = handlers.search_relevant(
            MemorySearchRelevantInput(query="口播", scope_filter="user"), context
        )
        assert {item["memory_id"] for item in user_only.data["memories"]} == {"mem_user"}

        project_only = handlers.search_relevant(
            MemorySearchRelevantInput(query="口播", scope_filter="project"), context
        )
        assert {item["memory_id"] for item in project_only.data["memories"]} == {"mem_p1"}

        # scope_filter=project 但无 project 上下文 → 空结果
        no_project = handlers.search_relevant(
            MemorySearchRelevantInput(query="口播", scope_filter="project"),
            _context(connection),
        )
        assert no_project.data["memories"] == []


def test_memory_save_followup_rejects_invalid_payload() -> None:
    """防御分支：payload 缺字段/类型错时记 trace 并直接返回，不触达 DB。"""
    from typing import Any, cast

    from agent_harness.loop import _execute_memory_save_followup
    from domain.decision_effects import HarnessFollowup

    class _Recorder:
        def __init__(self) -> None:
            self.payloads: list[dict[str, Any]] = []

        def record(self, kind: str, payload: dict[str, Any]) -> int:
            self.payloads.append(payload)
            return len(self.payloads)

    for payload in (
        {"candidate_id": 123, "scope": "user", "case_id": "case_1"},
        {"candidate_id": "memcand_1", "scope": "invalid", "case_id": "case_1"},
        {"candidate_id": "memcand_1", "scope": "user", "case_id": 42},
    ):
        recorder = _Recorder()
        _execute_memory_save_followup(
            HarnessFollowup(kind="enqueue_memory_save", decision_id="dec_1", payload=payload),
            engine=cast(Any, None),
            router=cast(Any, None),
            turn_id="turn_1",
            accumulator=cast(Any, None),
            tracer=cast(Any, recorder),
        )
        assert recorder.payloads
        assert recorder.payloads[-1]["status"] == "failed"


def test_llm_output_parsing_fallbacks() -> None:
    """_content_from_llm_output 的 tool_call/tool_calls/JSON 文本回退链。"""
    from tools.memory_tools.handlers import (
        _arguments_from_tool_call,
        _content_from_llm_output,
        _content_from_text,
    )

    assert _content_from_llm_output({"memory": "直接字符串"}) == "直接字符串"
    assert (
        _content_from_llm_output({"tool_call": {"arguments": {"content": "来自 tool_call"}}})
        == "来自 tool_call"
    )
    assert (
        _content_from_llm_output(
            {"tool_calls": [{"function": {"arguments": '{"memory": "来自 tool_calls"}'}}]}
        )
        == "来自 tool_calls"
    )
    assert _content_from_llm_output({"unrelated": 1}) == ""

    assert _content_from_text('{"content": "JSON 包裹"}') == "JSON 包裹"
    assert _content_from_text("普通文本") == "普通文本"
    assert _content_from_text('{"other": 1}') == '{"other": 1}'

    assert _arguments_from_tool_call({"arguments": "not json"}) == {}
    assert _arguments_from_tool_call({"arguments": '["list"]'}) == {}
    assert _arguments_from_tool_call({}) == {}
