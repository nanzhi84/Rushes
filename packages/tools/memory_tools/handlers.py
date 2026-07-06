"""Long-term memory tool handlers."""

from __future__ import annotations

import asyncio
import json
import re
import threading
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Literal, cast
from uuid import uuid4

from sqlalchemy import and_, or_, select
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.decision import Decision, DecisionOption
from contracts.events import (
    CapabilityDegraded,
    DecisionCreated,
    MemoryCandidateExtracted,
    MemorySaved,
)
from contracts.provider import ProviderResult
from contracts.tool_result import ToolError, ToolResult
from providers import LLM_CHAT, ProviderRequest
from storage import schema
from storage.repositories._json import load_json
from tools.context import ToolExecutionContext
from tools.specs import (
    MemoryAskScopeInput,
    MemoryExtractFromCaseInput,
    MemorySaveInput,
    MemorySearchRelevantInput,
)


@dataclass(frozen=True, slots=True)
class MemorySearchHit:
    memory_id: str
    scope: Literal["user", "project"]
    project_id: str | None
    content: str
    score: float


def extract_from_case(
    input_model: MemoryExtractFromCaseInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "memory.extract_from_case"
    case_state = context.case_state
    connection = context.readonly_connection
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "memory tools require DB access")

    source = _case_memory_source(connection, case_state, input_model.summary_hint)
    extraction = _extract_content_with_llm(source, context)
    content = extraction.content.strip() or _fallback_memory_text(source)
    suggested_scope = _suggest_scope(content, input_model.summary_hint)
    candidate_id = f"memcand_{uuid4().hex}"
    created_at = _now(context)
    event = MemoryCandidateExtracted(
        candidate_id=candidate_id,
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        payload={
            "case_id": case_state.case_id,
            "content": content,
            "suggested_scope": suggested_scope,
            "status": "pending",
            "created_at": created_at,
            "source": "llm" if extraction.used_llm else "fallback",
        },
    )
    events = [*extraction.events, event.model_dump(mode="json")]
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation="已提取一条待确认的经验候选。",
        data={
            "candidate_id": candidate_id,
            "suggested_scope": suggested_scope,
            "content": content,
        },
        events=events,
    )


def ask_scope(input_model: MemoryAskScopeInput, context: ToolExecutionContext) -> ToolResult:
    tool_name = "memory.ask_scope"
    case_state = context.case_state
    connection = context.readonly_connection
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "memory tools require DB access")
    candidate = _memory_candidate(connection, input_model.candidate_id)
    if candidate is None:
        return _failed(tool_name, context, "candidate_not_found", "memory candidate not found")
    if str(candidate["status"]) != "pending":
        return _failed(
            tool_name,
            context,
            "candidate_not_pending",
            "memory candidate is not pending",
            details={"status": candidate["status"]},
        )
    if str(candidate["case_id"]) != case_state.case_id:
        return _failed(
            tool_name,
            context,
            "candidate_case_mismatch",
            "memory candidate does not belong to active case",
        )

    decision = Decision(
        decision_id=f"decision_memory_scope_{input_model.candidate_id}",
        scope_type="case",
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        type="memory_scope",
        question="这条经验要保存到哪个作用域？",
        options=[
            DecisionOption(
                option_id="project",
                label="project 级",
                description="只在当前 Project 的 Case 中注入。",
                payload={"candidate_id": input_model.candidate_id, "scope": "project"},
            ),
            DecisionOption(
                option_id="user",
                label="user 级",
                description="可在任意 Project 中按相关性注入。",
                payload={"candidate_id": input_model.candidate_id, "scope": "user"},
            ),
            DecisionOption(
                option_id="skip",
                label="跳过",
                description="丢弃这条候选，不写入长期记忆。",
                payload={"candidate_id": input_model.candidate_id, "scope": "skip"},
            ),
        ],
        allow_free_text=False,
        status="pending",
        blocking=True,
        created_by_tool_call_id=context.tool_call_id,
    )
    event = DecisionCreated(
        decision_id=decision.decision_id,
        scope_type=decision.scope_type,
        project_id=decision.project_id,
        case_id=decision.case_id,
        payload={
            "decision": decision.model_dump(mode="json"),
            "type": decision.type,
            "question": decision.question,
            "options": [option.model_dump(mode="json") for option in decision.options],
            "blocking": decision.blocking,
            "candidate_id": input_model.candidate_id,
            "created_by_tool_call_id": context.tool_call_id,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="requires_user",
        observation=decision.question,
        data={"decision_id": decision.decision_id, "candidate_id": input_model.candidate_id},
        events=[event.model_dump(mode="json")],
    )


def save(input_model: MemorySaveInput, context: ToolExecutionContext) -> ToolResult:
    tool_name = "memory.save"
    connection = context.readonly_connection
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "memory.save requires DB access")
    candidate = _memory_candidate(connection, input_model.candidate_id)
    if candidate is None:
        return _failed(tool_name, context, "candidate_not_found", "memory candidate not found")
    if str(candidate["status"]) != "pending":
        return _failed(
            tool_name,
            context,
            "candidate_not_pending",
            "memory candidate is not pending",
            details={
                "status": candidate["status"],
                "saved_memory_id": candidate.get("saved_memory_id"),
            },
        )
    case_id = str(candidate["case_id"])
    project_id = _project_id_for_candidate(connection, case_id)
    if input_model.scope == "project" and project_id is None:
        return _failed(tool_name, context, "case_not_found", "candidate case not found")

    memory_id = f"mem_{uuid4().hex}"
    created_at = _now(context)
    event = MemorySaved(
        memory_id=memory_id,
        candidate_id=input_model.candidate_id,
        project_id=project_id if input_model.scope == "project" else None,
        payload={
            "candidate_id": input_model.candidate_id,
            "scope": input_model.scope,
            "project_id": project_id if input_model.scope == "project" else None,
            "content": str(candidate["content"]),
            "tags": [],
            "created_from_case_id": case_id,
            "created_at": created_at,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation="经验已保存。",
        data={
            "candidate_id": input_model.candidate_id,
            "memory_id": memory_id,
            "scope": input_model.scope,
        },
        events=[event.model_dump(mode="json")],
    )


def search_relevant(
    input_model: MemorySearchRelevantInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "memory.search_relevant"
    connection = context.readonly_connection
    if connection is None:
        return _failed(tool_name, context, "missing_connection", "memory tools require DB access")
    project_id = _active_project_id(context)
    hits = search_relevant_memories(
        connection,
        query=input_model.query,
        project_id=project_id,
        scope_filter=input_model.scope_filter,
        limit=input_model.limit,
    )
    payload = [
        {
            "memory_id": hit.memory_id,
            "scope": hit.scope,
            "project_id": hit.project_id,
            "summary": _truncate(hit.content, 200),
            "score": hit.score,
        }
        for hit in hits
    ]
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=f"找到 {len(payload)} 条相关经验。",
        data={"memories": payload},
    )


def search_relevant_memories(
    connection: Connection,
    *,
    query: str,
    project_id: str | None,
    scope_filter: Literal["user", "project"] | None = None,
    limit: int = 5,
) -> tuple[MemorySearchHit, ...]:
    """Return scoped memory hits using a lightweight in-process ranker.

    M8 keeps the storage change narrow: scoped SQL filtering prevents project
    leakage, then a small row set is scored in process instead of adding an FTS
    migration for memories.
    """

    statement = select(schema.memories)
    if scope_filter == "project":
        if project_id is None:
            return ()
        statement = statement.where(
            schema.memories.c.scope == "project",
            schema.memories.c.project_id == project_id,
        )
    elif scope_filter == "user":
        statement = statement.where(schema.memories.c.scope == "user")
    else:
        project_id_clause = (
            schema.memories.c.project_id == project_id
            if project_id is not None
            else schema.memories.c.project_id.is_(None)
        )
        project_clause = and_(schema.memories.c.scope == "project", project_id_clause)
        statement = statement.where(
            or_(
                schema.memories.c.scope == "user",
                project_clause,
            )
        )
    rows = connection.execute(statement.order_by(schema.memories.c.created_at.desc())).all()
    tokens = _query_tokens(query)
    hits: list[MemorySearchHit] = []
    for index, row in enumerate(rows):
        values = dict(row._mapping)
        content = str(values["content"])
        score = _memory_score(content, tokens, index)
        if score <= 0 and tokens:
            continue
        hits.append(
            MemorySearchHit(
                memory_id=str(values["memory_id"]),
                scope=cast(Literal["user", "project"], values["scope"]),
                project_id=values["project_id"],
                content=content,
                score=score,
            )
        )
    limited_hits = sorted(hits, key=lambda hit: hit.score, reverse=True)[: max(1, min(limit, 5))]
    return tuple(limited_hits)


@dataclass(frozen=True, slots=True)
class _ExtractionResult:
    content: str
    used_llm: bool
    events: tuple[dict[str, Any], ...] = ()


def _case_memory_source(
    connection: Connection,
    case_state: CaseState,
    summary_hint: str | None,
) -> dict[str, Any]:
    decisions = []
    decision_rows = connection.execute(
        select(schema.decisions)
        .where(schema.decisions.c.case_id == case_state.case_id)
        .where(schema.decisions.c.status == "answered")
        .order_by(schema.decisions.c.decision_id)
    ).all()
    for row in decision_rows:
        values = dict(row._mapping)
        decisions.append(
            {
                "type": values["type"],
                "question": values["question"],
                "answer": load_json(values["answer"]) if values.get("answer") else None,
            }
        )
    export_rows = connection.execute(
        select(schema.exports)
        .where(schema.exports.c.case_id == case_state.case_id)
        .order_by(schema.exports.c.created_at.desc())
        .limit(5)
    ).all()
    exports = [
        {
            "export_id": row._mapping["export_id"],
            "timeline_version": row._mapping["timeline_version"],
            "quality": load_json(row._mapping["quality"]),
            "created_at": row._mapping["created_at"],
        }
        for row in export_rows
    ]
    return {
        "summary_hint": summary_hint,
        "case_id": case_state.case_id,
        "project_id": case_state.project_id,
        "brief": case_state.brief.model_dump(mode="json"),
        "cut_plan": None
        if case_state.cut_plan is None
        else case_state.cut_plan.model_dump(mode="json", by_alias=True),
        "decisions_answered": decisions,
        "exports": exports,
        "scratch_memory": case_state.scratch_memory,
    }


def _extract_content_with_llm(
    source: dict[str, Any],
    context: ToolExecutionContext,
) -> _ExtractionResult:
    gateway = context.metadata.get("provider_gateway") or context.metadata.get("llm_gateway")
    if gateway is None or not hasattr(gateway, "call"):
        return _ExtractionResult(
            content=_fallback_memory_text(source),
            used_llm=False,
            events=(
                CapabilityDegraded(
                    degradation_id=f"degraded_{context.tool_call_id}_memory_llm",
                    project_id=source["project_id"],
                    case_id=source["case_id"],
                    capability="llm.chat",
                    provider_id=None,
                    reason="llm gateway is not configured",
                    fallback="rule_based_memory_template",
                    payload={"tool_call_id": context.tool_call_id},
                ).model_dump(mode="json"),
            ),
        )
    request = ProviderRequest(
        capability=LLM_CHAT,
        request_id=f"memory_extract_{context.tool_call_id}",
        case_id=str(source["case_id"]),
        payload={
            "messages": [
                {
                    "role": "system",
                    "content": (
                        "你是视频剪辑项目的经验提炼器。只输出一段可复用经验，"
                        "不超过 120 字，不要编造输入里没有的信息。"
                    ),
                },
                {
                    "role": "user",
                    "content": json.dumps(source, ensure_ascii=False, sort_keys=True),
                },
            ]
        },
    )
    try:
        gateway_result = _run_async_sync(gateway.call(request))
    except Exception as exc:
        return _ExtractionResult(
            content=_fallback_memory_text(source),
            used_llm=False,
            events=(
                CapabilityDegraded(
                    degradation_id=f"degraded_{context.tool_call_id}_memory_llm",
                    project_id=source["project_id"],
                    case_id=source["case_id"],
                    capability="llm.chat",
                    provider_id=None,
                    reason=f"llm call failed: {exc}",
                    fallback="rule_based_memory_template",
                    payload={"tool_call_id": context.tool_call_id},
                ).model_dump(mode="json"),
            ),
        )
    events = tuple(dict(event) for event in getattr(gateway_result, "events", ()) or ())
    result = cast(ProviderResult, gateway_result.result)
    if result.error is not None:
        return _ExtractionResult(
            content=_fallback_memory_text(source),
            used_llm=False,
            events=(
                *events,
                CapabilityDegraded(
                    degradation_id=f"degraded_{context.tool_call_id}_memory_llm",
                    project_id=source["project_id"],
                    case_id=source["case_id"],
                    capability="llm.chat",
                    provider_id=result.provider_id,
                    reason=f"{result.error.error_code}: {result.error.message}",
                    fallback="rule_based_memory_template",
                    payload={"tool_call_id": context.tool_call_id},
                ).model_dump(mode="json"),
            ),
        )
    content = _content_from_llm_output(result.normalized_output)
    return _ExtractionResult(content=content, used_llm=True, events=events)


def _fallback_memory_text(source: Mapping[str, Any]) -> str:
    brief = source.get("brief")
    goal = ""
    if isinstance(brief, Mapping):
        goal = str(brief.get("goal") or "")
    hint = source.get("summary_hint")
    scratch = source.get("scratch_memory")
    scratch_text = ""
    if isinstance(scratch, Mapping) and scratch:
        scratch_text = "；".join(f"{key}: {value}" for key, value in list(scratch.items())[:3])
    parts = [part for part in (str(hint or ""), goal, scratch_text) if part]
    if parts:
        return "本 Case 经验：" + "；".join(parts)[:220]
    return f"Case {source.get('case_id')} 已完成一次导出，可复用其剪辑决策与修改轨迹。"


def _content_from_llm_output(output: Mapping[str, Any]) -> str:
    for key in ("memory", "memory_candidate", "content", "text"):
        value = output.get(key)
        if isinstance(value, str) and value.strip():
            return _content_from_text(value)
    tool_call = output.get("tool_call")
    if isinstance(tool_call, Mapping):
        parsed = _arguments_from_tool_call(tool_call)
        if parsed:
            return _content_from_llm_output(parsed)
    tool_calls = output.get("tool_calls")
    if isinstance(tool_calls, Sequence) and not isinstance(tool_calls, str | bytes):
        for item in tool_calls:
            if isinstance(item, Mapping):
                parsed = _arguments_from_tool_call(item)
                if parsed:
                    return _content_from_llm_output(parsed)
    return ""


def _content_from_text(value: str) -> str:
    text = value.strip()
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        return text
    if isinstance(parsed, Mapping):
        for key in ("memory", "memory_candidate", "content", "text"):
            inner = parsed.get(key)
            if isinstance(inner, str) and inner.strip():
                return inner.strip()
    return text


def _arguments_from_tool_call(value: Mapping[str, Any]) -> dict[str, Any]:
    raw: Any = value.get("arguments")
    function = value.get("function")
    if raw is None and isinstance(function, Mapping):
        raw = function.get("arguments")
    if isinstance(raw, Mapping):
        return dict(raw)
    if isinstance(raw, str):
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            return {}
        return dict(parsed) if isinstance(parsed, Mapping) else {}
    return {}


def _suggest_scope(content: str, summary_hint: str | None) -> Literal["user", "project"]:
    text = f"{summary_hint or ''}\n{content}".lower()
    user_markers = ("个人", "偏好", "我", "用户", "每次", "以后都", "全局")
    return "user" if any(marker in text for marker in user_markers) else "project"


def _memory_candidate(connection: Connection, candidate_id: str) -> dict[str, Any] | None:
    row = connection.execute(
        select(schema.memory_candidates).where(
            schema.memory_candidates.c.candidate_id == candidate_id
        )
    ).first()
    return None if row is None else dict(row._mapping)


def _project_id_for_candidate(connection: Connection, case_id: str) -> str | None:
    row = connection.execute(
        select(schema.cases.c.project_id).where(schema.cases.c.case_id == case_id)
    ).first()
    return None if row is None else str(row._mapping["project_id"])


def _active_project_id(context: ToolExecutionContext) -> str | None:
    if context.case_state is not None:
        return context.case_state.project_id
    if context.project_state is not None:
        return context.project_state.project_id
    return None


def _query_tokens(query: str) -> tuple[str, ...]:
    return tuple(token for token in re.findall(r"[\w\u4e00-\u9fff]+", query.lower()) if token)


def _memory_score(content: str, tokens: Sequence[str], index: int) -> float:
    if not tokens:
        return 1.0 / (index + 1)
    lowered = content.lower()
    score = 0.0
    for token in tokens:
        if token in lowered:
            score += 2.0 if len(token) > 1 else 0.5
    return score + 1.0 / ((index + 1) * 100)


def _truncate(value: str, limit: int) -> str:
    if len(value) <= limit:
        return value
    return value[: limit - 1] + "…"


def _failed(
    tool_name: str,
    context: ToolExecutionContext,
    error_code: str,
    message: str,
    *,
    details: dict[str, Any] | None = None,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="failed",
        observation=message,
        error=ToolError(
            error_code=error_code,
            message=message,
            retryable=False,
            details=details or {},
        ),
    )


def _now(context: ToolExecutionContext) -> str:
    return context.created_at or datetime.now(UTC).isoformat()


def _run_async_sync(awaitable: Any) -> Any:
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return asyncio.run(awaitable)

    result: dict[str, Any] = {}

    def runner() -> None:
        try:
            result["value"] = asyncio.run(awaitable)
        except BaseException as exc:  # pragma: no cover - defensive thread bridge
            result["error"] = exc

    thread = threading.Thread(target=runner, daemon=True)
    thread.start()
    thread.join()
    if "error" in result:
        raise result["error"]
    return result["value"]
