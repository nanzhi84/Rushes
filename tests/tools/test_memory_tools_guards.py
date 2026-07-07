"""memory 工具守卫分支与 search_relevant 检索（单级草稿模型：记忆固定 user 域）。"""

from __future__ import annotations

from pathlib import Path

from sqlalchemy.engine import Engine

from contracts.draft import DraftState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.memory_tools import handlers
from tools.specs import (
    MemoryAskScopeInput,
    MemoryExtractFromDraftInput,
    MemorySaveInput,
    MemorySearchRelevantInput,
)
from tools.timeline_tools import handlers as _  # noqa: F401  # 确保包导入顺序稳定

NOW = "2026-07-06T00:00:00+00:00"


def _engine(tmp_path: Path) -> Engine:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _draft_state() -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": {"mode": "silent"},
        }
    )


def _context(connection=None, *, draft_state=None) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=draft_state,
        readonly_connection=connection,
        created_at=NOW,
    )


def _seed_memory(
    connection,
    memory_id: str,
    *,
    content: str,
) -> None:
    connection.execute(
        schema.memories.insert().values(
            memory_id=memory_id,
            scope="user",
            content=content,
            tags="[]",
            created_from_draft_id=None,
            created_at=NOW,
        )
    )


def test_memory_handlers_guard_missing_draft_and_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.connect() as connection:
        no_draft = handlers.extract_from_draft(MemoryExtractFromDraftInput(), _context(connection))
        assert no_draft.error is not None
        assert no_draft.error.error_code == "missing_draft"
        no_draft_ask = handlers.ask_scope(
            MemoryAskScopeInput(candidate_id="memcand_x"), _context(connection)
        )
        assert no_draft_ask.error is not None
        assert no_draft_ask.error.error_code == "missing_draft"

    state = _draft_state()
    no_conn = handlers.extract_from_draft(
        MemoryExtractFromDraftInput(), _context(draft_state=state)
    )
    assert no_conn.error is not None
    assert no_conn.error.error_code == "missing_connection"
    no_conn_ask = handlers.ask_scope(
        MemoryAskScopeInput(candidate_id="memcand_x"), _context(draft_state=state)
    )
    assert no_conn_ask.error is not None
    assert no_conn_ask.error.error_code == "missing_connection"
    no_conn_save = handlers.save(
        MemorySaveInput(candidate_id="memcand_x"), _context(draft_state=state)
    )
    assert no_conn_save.error is not None
    assert no_conn_save.error.error_code == "missing_connection"
    no_conn_search = handlers.search_relevant(
        MemorySearchRelevantInput(query="q"), _context(draft_state=state)
    )
    assert no_conn_search.error is not None
    assert no_conn_search.error.error_code == "missing_connection"


def test_ask_scope_and_save_reject_missing_or_settled_candidates(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    state = _draft_state()
    with engine.begin() as connection:
        context = _context(connection, draft_state=state)
        not_found = handlers.ask_scope(MemoryAskScopeInput(candidate_id="memcand_ghost"), context)
        assert not_found.error is not None
        assert not_found.error.error_code == "candidate_not_found"
        save_not_found = handlers.save(MemorySaveInput(candidate_id="memcand_ghost"), context)
        assert save_not_found.error is not None
        assert save_not_found.error.error_code == "candidate_not_found"

        connection.execute(
            schema.memory_candidates.insert().values(
                candidate_id="memcand_done",
                draft_id="draft_1",
                content="已保存过的候选",
                suggested_scope="user",
                status="saved",
                created_at=NOW,
            )
        )
        settled_ask = handlers.ask_scope(MemoryAskScopeInput(candidate_id="memcand_done"), context)
        assert settled_ask.error is not None
        assert settled_ask.error.error_code == "candidate_not_pending"
        settled_save = handlers.save(MemorySaveInput(candidate_id="memcand_done"), context)
        assert settled_save.error is not None
        assert settled_save.error.error_code == "candidate_not_pending"


def test_search_relevant_finds_user_memories_and_truncates(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    state = _draft_state()
    long_text = "口播剪辑经验：" + "节奏优先，" * 60
    with engine.begin() as connection:
        _seed_memory(connection, "mem_user", content="用户级经验：口播节奏")
        _seed_memory(connection, "mem_long", content=long_text)
        context = _context(connection, draft_state=state)

        found = handlers.search_relevant(MemorySearchRelevantInput(query="口播"), context)
        ids = {item["memory_id"] for item in found.data["memories"]}
        assert {"mem_user", "mem_long"} <= ids
        assert all(item["scope"] == "user" for item in found.data["memories"])
        long_hit = next(item for item in found.data["memories"] if item["memory_id"] == "mem_long")
        assert len(long_hit["summary"]) <= 201  # 截断（含省略号）
        assert "找到" in found.observation


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
