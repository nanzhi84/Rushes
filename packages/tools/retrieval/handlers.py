"""Candidate retrieval tool handler."""

from __future__ import annotations

import asyncio
import hashlib
import threading
from collections.abc import Mapping, Sequence
from datetime import UTC, datetime
from typing import Any, cast

from sqlalchemy import select

from contracts.candidate import CandidatePack
from contracts.events import CandidatePackCreated, CapabilityDegraded
from contracts.provider import ProviderResult
from contracts.tool_result import ToolArtifact, ToolError, ToolResult
from indexing import build_candidate_pack
from providers import EMBEDDING_TEXT, ProviderRequest
from storage import schema
from storage.repositories._json import dump_json
from tools.context import ToolExecutionContext
from tools.specs import RetrievalSearchCandidatesInput


def _search_observation(pack: CandidatePack) -> str:
    # LLM 只读 observation：候选数必须报告，0 候选时给出可执行指引，
    # 否则模型会反复重搜（M9 风景混剪实测）
    total = sum(len(slot.candidates) for slot in pack.slots)
    if total == 0:
        return (
            f"候选包 {pack.candidate_pack_id} 已创建，但所有 slot 的候选为 0。"
            "常见原因：素材尚未完成标注（annotation.enqueue 后等待完成）"
            "或 slot brief 与素材内容不匹配（content.revise_plan 修订）。"
            "不要原样重试本工具。"
        )
    lines: list[str] = []
    for slot in pack.slots:
        lines.append(f"{slot.slot_id}（{len(slot.candidates)} 个候选）：")
        for candidate in slot.candidates[:3]:
            summary = candidate.summary_line[:60]
            lines.append(f"  - candidate_id={candidate.candidate_id} | {summary}")
    detail = "\n".join(lines)
    return (
        f"候选包 {pack.candidate_pack_id} 已创建，共 {total} 个候选。\n{detail}\n"
        "下一步调用 timeline.plan_from_candidates，selections 里的 candidate_id "
        "必须使用上面列出的原文 id，不要自行构造。"
    )


def search_candidates(
    input_model: RetrievalSearchCandidatesInput,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed("missing_case", "active case required", context)
    if context.readonly_connection is None:
        return _failed("missing_connection", "repository access required", context)
    if case_state.audio_plan is None:
        return _failed("audio_plan_missing", "audio_plan must be confirmed", context)
    if case_state.cut_plan is None:
        return _failed("cut_plan_missing", "cut_plan is required", context)
    if not _usable_asset_exists(context):
        return _failed(
            "no_usable_asset",
            "at least one usable non-disabled asset is required",
            context,
        )

    embedding = _query_vectors(input_model, context)
    pack = build_candidate_pack(
        context.readonly_connection,
        case_state,
        case_state.cut_plan,
        embedding.vectors,
        generated_at=_created_at(context),
    )
    _persist_candidate_pack(context, pack)
    created = CandidatePackCreated(
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        candidate_pack_id=pack.candidate_pack_id,
        payload={
            "candidate_pack_id": pack.candidate_pack_id,
            "candidate_pack": pack.model_dump(mode="json"),
            "slots": [slot.model_dump(mode="json") for slot in pack.slots],
            "created_at": pack.snapshot.generated_at,
        },
    )
    events = [*embedding.events, created.model_dump(mode="json")]
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="retrieval.search_candidates",
        status="succeeded",
        observation=_search_observation(pack),
        data={
            "case_id": case_state.case_id,
            "candidate_pack_id": pack.candidate_pack_id,
            "candidate_pack": pack.model_dump(mode="json"),
            "degraded": embedding.degraded,
        },
        artifacts=[
            ToolArtifact(
                artifact_id=pack.candidate_pack_id,
                kind="candidate_pack",
            )
        ],
        events=events,
    )


class _EmbeddingResult:
    def __init__(
        self,
        *,
        vectors: Mapping[str, Sequence[float]],
        events: Sequence[dict[str, Any]],
        degraded: bool,
    ) -> None:
        self.vectors = vectors
        self.events = tuple(events)
        self.degraded = degraded


def _query_vectors(
    input_model: RetrievalSearchCandidatesInput,
    context: ToolExecutionContext,
) -> _EmbeddingResult:
    case_state = context.case_state
    if case_state is None or case_state.cut_plan is None:
        return _EmbeddingResult(vectors={}, events=(), degraded=False)
    gateway = context.metadata.get("embedding_gateway") or context.metadata.get("provider_gateway")
    if gateway is None or not hasattr(gateway, "call"):
        return _degraded_embedding(context, "embedding gateway is not configured")
    vectors: dict[str, Sequence[float]] = {}
    events: list[dict[str, Any]] = []
    for slot in case_state.cut_plan.slots:
        request = ProviderRequest(
            capability=EMBEDDING_TEXT,
            request_id=f"retrieval_{context.tool_call_id}_{slot.slot_id}",
            model=input_model.embedding_model,
            case_id=case_state.case_id,
            payload={"input": slot.brief, "retrieval_sentence": slot.brief},
        )
        try:
            gateway_result = _run_async_sync(
                gateway.call(request, provider_id=input_model.provider_id)
            )
        except Exception as exc:
            return _degraded_embedding(
                context,
                f"embedding call failed: {exc}",
                prior_events=events,
            )
        events.extend(dict(event) for event in getattr(gateway_result, "events", ()) or ())
        result = cast(ProviderResult, gateway_result.result)
        if result.error is not None:
            return _degraded_embedding(
                context,
                f"{result.error.error_code}: {result.error.message}",
                prior_events=events,
            )
        vector = _embedding_vector(result.normalized_output)
        if vector is None:
            return _degraded_embedding(
                context,
                "embedding result did not contain a vector",
                prior_events=events,
            )
        vectors[slot.slot_id] = vector
    return _EmbeddingResult(vectors=vectors, events=events, degraded=False)


def _degraded_embedding(
    context: ToolExecutionContext,
    reason: str,
    *,
    prior_events: Sequence[dict[str, Any]] = (),
) -> _EmbeddingResult:
    case_state = context.case_state
    event = CapabilityDegraded(
        degradation_id=f"degraded_{context.tool_call_id}_{_digest(reason)}",
        project_id=None if case_state is None else case_state.project_id,
        case_id=None if case_state is None else case_state.case_id,
        capability=EMBEDDING_TEXT,
        provider_id=None,
        reason=reason,
        fallback="keyword_only",
        payload={"tool_call_id": context.tool_call_id},
    )
    return _EmbeddingResult(
        vectors={},
        events=(*prior_events, event.model_dump(mode="json")),
        degraded=True,
    )


def _embedding_vector(output: Mapping[str, Any]) -> list[float] | None:
    direct = output.get("embedding") or output.get("vector")
    if isinstance(direct, Sequence) and not isinstance(direct, str | bytes | bytearray):
        return [float(item) for item in direct]
    data = output.get("data")
    if isinstance(data, Sequence) and not isinstance(data, str | bytes | bytearray) and data:
        first = data[0]
        if isinstance(first, Mapping):
            embedding = first.get("embedding")
            if isinstance(embedding, Sequence) and not isinstance(
                embedding,
                str | bytes | bytearray,
            ):
                return [float(item) for item in embedding]
    return None


def _persist_candidate_pack(context: ToolExecutionContext, pack: CandidatePack) -> None:
    assert context.readonly_connection is not None
    exists = context.readonly_connection.execute(
        select(schema.candidate_packs.c.candidate_pack_id).where(
            schema.candidate_packs.c.candidate_pack_id == pack.candidate_pack_id
        )
    ).first()
    if exists is not None:
        return
    context.readonly_connection.execute(
        schema.candidate_packs.insert().values(
            candidate_pack_id=pack.candidate_pack_id,
            case_id=pack.case_id,
            slots=dump_json([slot.model_dump(mode="json") for slot in pack.slots]),
            created_at=pack.snapshot.generated_at,
        )
    )


def _usable_asset_exists(context: ToolExecutionContext) -> bool:
    case_state = context.case_state
    if case_state is None or context.readonly_connection is None:
        return False
    statement = (
        select(schema.assets.c.asset_id)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == case_state.project_id)
        .where(schema.project_asset_links.c.enabled.is_(True))
        .where(schema.assets.c.usable.is_(True))
    )
    disabled = set(case_state.disabled_asset_ids)
    if disabled:
        statement = statement.where(schema.assets.c.asset_id.not_in(disabled))
    return context.readonly_connection.execute(statement.limit(1)).first() is not None


def _created_at(context: ToolExecutionContext) -> str:
    return context.created_at or datetime.now(UTC).isoformat()


def _digest(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()[:12]


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


def _failed(error_code: str, message: str, context: ToolExecutionContext) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="retrieval.search_candidates",
        status="failed",
        observation=message,
        error=ToolError(error_code=error_code, message=message, retryable=False),
    )
