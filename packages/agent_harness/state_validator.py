"""StateValidator invariants from PRD §4.6."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.draft import DraftState
from contracts.timeline import TimelineState
from storage import schema
from storage.repositories._json import load_json

TimelineInvariantHook = Callable[[Connection, DraftState, TimelineState], Sequence[str]]


@dataclass(frozen=True, slots=True)
class ValidationViolation:
    code: str
    message: str
    draft_id: str | None = None
    event_type: str | None = None


@dataclass(frozen=True, slots=True)
class ValidationFailed:
    violations: tuple[ValidationViolation, ...]


def validate_before_commit(
    connection: Connection,
    *,
    draft_states: Mapping[str, DraftState],
    timeline_invariant_hook: TimelineInvariantHook | None = None,
) -> ValidationFailed | None:
    violations: list[ValidationViolation] = []
    for draft_state in draft_states.values():
        violations.extend(_validate_draft_references(connection, draft_state))
        violations.extend(
            _validate_timeline_structure(connection, draft_state, timeline_invariant_hook)
        )
    if violations:
        return ValidationFailed(violations=tuple(violations))
    return None


def _validate_draft_references(
    connection: Connection,
    draft_state: DraftState,
) -> list[ValidationViolation]:
    violations: list[ValidationViolation] = []
    if draft_state.timeline_current_version is not None and not _timeline_exists(
        connection,
        draft_state.draft_id,
        draft_state.timeline_current_version,
    ):
        violations.append(
            ValidationViolation(
                code="missing_timeline_current_version",
                message="timeline_current_version does not exist for this draft",
                draft_id=draft_state.draft_id,
            )
        )
    if draft_state.preview_current_id is not None and not _preview_belongs_to_draft(
        connection,
        draft_state.preview_current_id,
        draft_state.draft_id,
    ):
        violations.append(
            ValidationViolation(
                code="invalid_preview_current_id",
                message="preview_current_id does not belong to this draft",
                draft_id=draft_state.draft_id,
            )
        )
    if draft_state.pending_decision_id is not None:
        decision = _decision_row(connection, draft_state.pending_decision_id)
        if decision is None or decision.get("draft_id") != draft_state.draft_id:
            violations.append(
                ValidationViolation(
                    code="invalid_pending_decision_id",
                    message="pending_decision_id does not belong to this draft",
                    draft_id=draft_state.draft_id,
                )
            )
        elif decision.get("status") != "pending":
            violations.append(
                ValidationViolation(
                    code="pending_decision_not_pending",
                    message="pending_decision_id must point to a pending decision",
                    draft_id=draft_state.draft_id,
                )
            )
    return violations


def _validate_timeline_structure(
    connection: Connection,
    draft_state: DraftState,
    timeline_invariant_hook: TimelineInvariantHook | None,
) -> list[ValidationViolation]:
    if draft_state.timeline_current_version is None:
        return []
    row = connection.execute(
        select(schema.timeline_versions).where(
            schema.timeline_versions.c.draft_id == draft_state.draft_id,
            schema.timeline_versions.c.version == draft_state.timeline_current_version,
        )
    ).first()
    if row is None:
        return []
    values = dict(row._mapping)
    document = load_json(values["document_json"])
    try:
        timeline = TimelineState.model_validate(document)
    except Exception as exc:
        return [
            ValidationViolation(
                code="invalid_timeline_document",
                message=f"timeline document cannot be parsed: {exc}",
                draft_id=draft_state.draft_id,
            )
        ]
    violations: list[ValidationViolation] = []
    if (
        timeline.draft_id != draft_state.draft_id
        or timeline.version != draft_state.timeline_current_version
    ):
        violations.append(
            ValidationViolation(
                code="timeline_identity_mismatch",
                message=(
                    "timeline document draft_id/version must match the timeline row and DraftState"
                ),
                draft_id=draft_state.draft_id,
            )
        )
    if timeline_invariant_hook is not None:
        for message in timeline_invariant_hook(connection, draft_state, timeline):
            violations.append(
                ValidationViolation(
                    code="timeline_frame_invariant_failed",
                    message=message,
                    draft_id=draft_state.draft_id,
                )
            )
    return violations


def _timeline_exists(connection: Connection, draft_id: str, version: int) -> bool:
    row = connection.execute(
        select(schema.timeline_versions.c.timeline_id).where(
            schema.timeline_versions.c.draft_id == draft_id,
            schema.timeline_versions.c.version == version,
        )
    ).first()
    return row is not None


def _preview_belongs_to_draft(connection: Connection, preview_id: str, draft_id: str) -> bool:
    row = connection.execute(
        select(schema.previews.c.preview_id).where(
            schema.previews.c.preview_id == preview_id,
            schema.previews.c.draft_id == draft_id,
        )
    ).first()
    return row is not None


def _decision_row(connection: Connection, decision_id: str) -> dict[str, Any] | None:
    row = connection.execute(
        select(schema.decisions).where(schema.decisions.c.decision_id == decision_id)
    ).first()
    if row is None:
        return None
    return dict(row._mapping)
