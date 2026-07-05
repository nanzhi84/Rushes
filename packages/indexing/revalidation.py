"""CandidatePack staleness and candidate-level revalidation."""

from __future__ import annotations

from collections.abc import Mapping
from contextlib import nullcontext
from dataclasses import dataclass
from typing import Any

from sqlalchemy.engine import Connection, Engine

from contracts.candidate import Candidate, CandidatePack
from contracts.case import CaseState

from .candidate_pack import compute_scope_snapshot


@dataclass(frozen=True, slots=True)
class RemovedCandidate:
    slot_id: str
    candidate_id: str
    asset_id: str
    clip_id: str
    reason: str


@dataclass(frozen=True, slots=True)
class RevalidationResult:
    valid_candidates: dict[str, tuple[Candidate, ...]]
    removed: tuple[RemovedCandidate, ...]
    scope_changed: bool
    stale_annotations: tuple[str, ...]


def revalidate_pack(
    engine: Engine | Connection,
    case_state: CaseState,
    pack: CandidatePack,
) -> RevalidationResult:
    with _connection_context(engine) as connection:
        scope = compute_scope_snapshot(connection, case_state)
    scope_changed = scope.asset_scope_hash != pack.snapshot.asset_scope_hash
    stale_assets: set[str] = set()
    valid: dict[str, tuple[Candidate, ...]] = {}
    removed: list[RemovedCandidate] = []
    for slot in pack.slots:
        slot_valid: list[Candidate] = []
        for candidate in slot.candidates:
            reason = _candidate_invalid_reason(
                candidate,
                current_versions=scope.annotation_versions,
                pack_versions=pack.snapshot.annotation_versions,
                current_clip_ids=set(scope.clip_rows),
            )
            if reason is None:
                slot_valid.append(candidate)
                continue
            if reason == "stale_annotation":
                stale_assets.add(candidate.asset_id)
            removed.append(
                RemovedCandidate(
                    slot_id=slot.slot_id,
                    candidate_id=candidate.candidate_id,
                    asset_id=candidate.asset_id,
                    clip_id=candidate.clip_id,
                    reason=reason,
                )
            )
        valid[slot.slot_id] = tuple(slot_valid)
    return RevalidationResult(
        valid_candidates=valid,
        removed=tuple(removed),
        scope_changed=scope_changed,
        stale_annotations=tuple(sorted(stale_assets)),
    )


def _candidate_invalid_reason(
    candidate: Candidate,
    *,
    current_versions: Mapping[str, str],
    pack_versions: dict[str, str],
    current_clip_ids: set[str],
) -> str | None:
    if candidate.clip_id not in current_clip_ids:
        return "asset_or_clip_not_in_scope"
    current_version = current_versions.get(candidate.asset_id)
    pack_version = pack_versions.get(candidate.asset_id)
    if current_version is None or pack_version is None or current_version != pack_version:
        return "stale_annotation"
    return None


def _connection_context(engine: Engine | Connection) -> Any:
    if isinstance(engine, Connection):
        return nullcontext(engine)
    return engine.connect()
