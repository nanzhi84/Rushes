"""CandidatePack construction from keyword/vector rankings and structured filters."""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from contextlib import nullcontext
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from sqlalchemy import and_, select
from sqlalchemy.engine import Connection, Engine

from contracts.candidate import (
    Candidate,
    CandidatePack,
    CandidatePackSnapshot,
    CandidateScore,
    CandidateSlot,
)
from contracts.case import CaseState, CutPlan
from storage import schema
from storage.repositories._json import load_json

from .keyword import search_bm25
from .rrf import fuse_rankings
from .types import RankedClip
from .vector import VectorSearchAdapter, search_cosine

DEFAULT_SEARCH_LIMIT = 64
DEFAULT_SLOT_LIMIT = 8
DEFAULT_RRF_K = 60
DEFAULT_MIN_QUALITY_SCORE = 0.5
DEFAULT_FPS = 30.0


@dataclass(frozen=True, slots=True)
class ClipRow:
    clip_id: str
    annotation_id: str
    asset_id: str
    start_frame: int
    end_frame: int
    role: str
    summary: str
    quality_score: float | None
    clip_usable: bool
    asset_usable: bool
    probe: Mapping[str, Any] | None
    annotation_updated_at: str
    annotation_document_json: str


@dataclass(frozen=True, slots=True)
class ScopeSnapshot:
    asset_ids: frozenset[str]
    clip_rows: Mapping[str, ClipRow]
    annotation_versions: Mapping[str, str]
    asset_scope_hash: str


def build_candidate_pack(
    engine: Engine | Connection,
    case_state: CaseState,
    cut_plan: CutPlan,
    query_vectors: Mapping[str, Sequence[float]] | None = None,
    *,
    vector_adapter: VectorSearchAdapter | None = None,
    search_limit: int = DEFAULT_SEARCH_LIMIT,
    slot_limit: int = DEFAULT_SLOT_LIMIT,
    rrf_k: int = DEFAULT_RRF_K,
    min_quality_score: float = DEFAULT_MIN_QUALITY_SCORE,
    generated_at: str | None = None,
) -> CandidatePack:
    """Build a strict CandidatePack for the current case/cut-plan scope."""

    with _connection_context(engine) as connection:
        scope = compute_scope_snapshot(connection, case_state)
        slots: list[CandidateSlot] = []
        allowed_clip_ids = set(scope.clip_rows)
        for slot_index, slot in enumerate(cut_plan.slots):
            keyword_ranks = search_bm25(
                connection,
                slot.brief,
                limit=search_limit,
                clip_ids=allowed_clip_ids,
            )
            vector_ranks: list[RankedClip] = []
            vector = (query_vectors or {}).get(slot.slot_id)
            if vector is not None:
                vector_ranks = search_cosine(
                    connection,
                    vector,
                    limit=search_limit,
                    clip_ids=allowed_clip_ids,
                    adapter=vector_adapter,
                )
            fused = fuse_rankings(keyword_ranks, vector_ranks, k=rrf_k)
            candidates: list[Candidate] = []
            role_filter = _role_filter(slot.brief)
            for fused_item in fused:
                row = scope.clip_rows.get(fused_item.clip_id)
                if row is None:
                    continue
                if not _passes_structured_filters(
                    row,
                    target_duration_sec=slot.target_duration_sec,
                    role_filter=role_filter,
                    min_quality_score=min_quality_score,
                ):
                    continue
                candidates.append(
                    Candidate(
                        candidate_id=f"cand_{slot_index + 1}_{len(candidates) + 1}_{row.clip_id}",
                        asset_id=row.asset_id,
                        clip_id=row.clip_id,
                        summary_line=_summary_line(row),
                        score=CandidateScore(
                            bm25_rank=fused_item.bm25_rank or 0,
                            vector_rank=fused_item.vector_rank or 0,
                            rrf=fused_item.rrf,
                        ),
                    )
                )
                if len(candidates) >= slot_limit:
                    break
            slots.append(
                CandidateSlot(
                    slot_id=slot.slot_id,
                    slot_brief=slot.brief,
                    target_duration_sec=slot.target_duration_sec,
                    candidates=candidates,
                )
            )
    timestamp = generated_at or datetime.now(UTC).isoformat()
    return CandidatePack(
        candidate_pack_id=f"cand_{uuid4().hex}",
        case_id=case_state.case_id,
        query_context={
            "cut_plan_ref": f"case:{case_state.case_id}:state:{case_state.state_version}:cut_plan",
            "audio_mode": (
                None if case_state.audio_plan is None else str(case_state.audio_plan.mode)
            ),
        },
        snapshot=CandidatePackSnapshot(
            generated_at=timestamp,
            asset_scope_hash=scope.asset_scope_hash,
            annotation_versions=dict(scope.annotation_versions),
        ),
        slots=slots,
    )


def compute_scope_snapshot(connection: Connection, case_state: CaseState) -> ScopeSnapshot:
    rows = _eligible_clip_rows(connection, case_state)
    clip_rows = {row.clip_id: row for row in rows}
    annotation_versions = _eligible_asset_versions(connection, case_state)
    asset_ids = frozenset(annotation_versions)
    return ScopeSnapshot(
        asset_ids=asset_ids,
        clip_rows=clip_rows,
        annotation_versions=annotation_versions,
        asset_scope_hash=_asset_scope_hash(
            asset_ids,
            selected_asset_ids=case_state.selected_asset_ids,
            disabled_asset_ids=case_state.disabled_asset_ids,
        ),
    )


def _eligible_clip_rows(connection: Connection, case_state: CaseState) -> list[ClipRow]:
    statement = (
        select(
            schema.annotation_clip_projection.c.clip_id,
            schema.annotation_clip_projection.c.annotation_id,
            schema.annotation_clip_projection.c.asset_id,
            schema.annotation_clip_projection.c.start_frame,
            schema.annotation_clip_projection.c.end_frame,
            schema.annotation_clip_projection.c.role,
            schema.annotation_clip_projection.c.summary,
            schema.annotation_clip_projection.c.quality_score,
            schema.annotation_clip_projection.c.usable.label("clip_usable"),
            schema.assets.c.usable.label("asset_usable"),
            schema.assets.c.probe,
            schema.annotations_table.c.updated_at.label("annotation_updated_at"),
            schema.annotations_table.c.document_json.label("annotation_document_json"),
        )
        .select_from(
            schema.annotation_clip_projection.join(
                schema.assets,
                schema.assets.c.asset_id == schema.annotation_clip_projection.c.asset_id,
            )
            .join(
                schema.project_asset_links,
                and_(
                    schema.project_asset_links.c.asset_id
                    == schema.annotation_clip_projection.c.asset_id,
                    schema.project_asset_links.c.project_id == case_state.project_id,
                ),
            )
            .join(
                schema.annotations_table,
                schema.annotations_table.c.annotation_id
                == schema.annotation_clip_projection.c.annotation_id,
            )
        )
        .where(schema.project_asset_links.c.enabled.is_(True))
        .where(schema.assets.c.usable.is_(True))
        .where(schema.assets.c.annotation_status == "completed")
        .where(schema.assets.c.index_status == "ready")
        .where(schema.annotations_table.c.status == "completed")
        .where(schema.annotation_clip_projection.c.usable.is_(True))
        .order_by(
            schema.annotation_clip_projection.c.asset_id,
            schema.annotation_clip_projection.c.start_frame,
            schema.annotation_clip_projection.c.clip_id,
        )
    )
    disabled = set(case_state.disabled_asset_ids)
    if disabled:
        statement = statement.where(schema.annotation_clip_projection.c.asset_id.not_in(disabled))
    selected = set(case_state.selected_asset_ids)
    if selected:
        statement = statement.where(schema.annotation_clip_projection.c.asset_id.in_(selected))
    result: list[ClipRow] = []
    for row in connection.execute(statement).all():
        values = row._mapping
        result.append(
            ClipRow(
                clip_id=str(values["clip_id"]),
                annotation_id=str(values["annotation_id"]),
                asset_id=str(values["asset_id"]),
                start_frame=int(values["start_frame"]),
                end_frame=int(values["end_frame"]),
                role=str(values["role"]),
                summary=str(values["summary"]),
                quality_score=(
                    None if values["quality_score"] is None else float(values["quality_score"])
                ),
                clip_usable=bool(values["clip_usable"]),
                asset_usable=bool(values["asset_usable"]),
                probe=_probe_payload(values["probe"]),
                annotation_updated_at=str(values["annotation_updated_at"]),
                annotation_document_json=str(values["annotation_document_json"]),
            )
        )
    return result


def _eligible_asset_versions(connection: Connection, case_state: CaseState) -> dict[str, str]:
    statement = (
        select(
            schema.assets.c.asset_id,
            schema.annotations_table.c.annotation_id,
            schema.annotations_table.c.updated_at,
        )
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                and_(
                    schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
                    schema.project_asset_links.c.project_id == case_state.project_id,
                ),
            ).join(
                schema.annotations_table,
                schema.annotations_table.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.enabled.is_(True))
        .where(schema.assets.c.usable.is_(True))
        .where(schema.assets.c.annotation_status == "completed")
        .where(schema.assets.c.index_status == "ready")
        .where(schema.annotations_table.c.status == "completed")
        .order_by(schema.assets.c.asset_id, schema.annotations_table.c.updated_at.desc())
    )
    disabled = set(case_state.disabled_asset_ids)
    if disabled:
        statement = statement.where(schema.assets.c.asset_id.not_in(disabled))
    selected = set(case_state.selected_asset_ids)
    if selected:
        statement = statement.where(schema.assets.c.asset_id.in_(selected))
    versions: dict[str, str] = {}
    for row in connection.execute(statement).all():
        asset_id = str(row._mapping["asset_id"])
        if asset_id in versions:
            continue
        versions[asset_id] = f"{row._mapping['annotation_id']}@{row._mapping['updated_at']}"
    return versions


def _passes_structured_filters(
    row: ClipRow,
    *,
    target_duration_sec: tuple[float, float],
    role_filter: frozenset[str] | None,
    min_quality_score: float,
) -> bool:
    if not row.clip_usable or not row.asset_usable:
        return False
    if role_filter is not None and row.role not in role_filter:
        return False
    if row.quality_score is not None and row.quality_score < min_quality_score:
        return False
    clip_duration = _clip_duration_sec(row)
    min_duration, max_duration = target_duration_sec
    if clip_duration < min_duration or clip_duration > max_duration:
        return False
    return not _hard_event_overlaps_clip(row)


def _role_filter(brief: str) -> frozenset[str] | None:
    lowered = brief.lower()
    a_roll_terms = ("a-roll", "aroll", "口播", "主讲", "采访", "人物讲话", "原声")
    b_roll_terms = (
        "b-roll",
        "broll",
        "空镜",
        "特写",
        "产品",
        "画面",
        "补充镜头",
        "素材",
    )
    if any(term in lowered for term in a_roll_terms):
        return frozenset({"a_roll_candidate"})
    if any(term in lowered for term in b_roll_terms):
        return frozenset({"b_roll_candidate", "image_candidate"})
    return None


def _clip_duration_sec(row: ClipRow) -> float:
    fps = DEFAULT_FPS
    if row.probe is not None:
        raw_fps = row.probe.get("fps")
        if isinstance(raw_fps, int | float) and raw_fps > 0:
            fps = float(raw_fps)
    return max(0.0, (row.end_frame - row.start_frame) / fps)


def _hard_event_overlaps_clip(row: ClipRow) -> bool:
    raw = load_json(row.annotation_document_json)
    if not isinstance(raw, dict):
        return False
    events = raw.get("quality_events")
    if not isinstance(events, list):
        return False
    for event in events:
        if not isinstance(event, dict) or event.get("severity") != "hard":
            continue
        start = event.get("start_frame")
        end = event.get("end_frame")
        if not isinstance(start, int) or not isinstance(end, int):
            continue
        if max(row.start_frame, start) < min(row.end_frame, end):
            return True
    return False


def _summary_line(row: ClipRow) -> str:
    quality = "未知" if row.quality_score is None else f"{row.quality_score:.2f}"
    return f"{row.summary}，{_clip_duration_sec(row):.1f}s 可用，质量 {quality}"


def _asset_scope_hash(
    asset_ids: frozenset[str],
    *,
    selected_asset_ids: Sequence[str],
    disabled_asset_ids: Sequence[str],
) -> str:
    payload = {
        "asset_ids": sorted(asset_ids),
        "selected_asset_ids": sorted(set(selected_asset_ids)),
        "disabled_asset_ids": sorted(set(disabled_asset_ids)),
    }
    raw = json.dumps(payload, ensure_ascii=False, separators=(",", ":"), sort_keys=True)
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _probe_payload(value: Any) -> Mapping[str, Any] | None:
    if value is None:
        return None
    parsed = load_json(str(value))
    return parsed if isinstance(parsed, dict) else None


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
