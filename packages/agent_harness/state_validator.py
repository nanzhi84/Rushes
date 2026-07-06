"""StateValidator invariants from PRD §4.6."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.events import DomainEventBase
from contracts.timeline import TimelineState
from storage import schema
from storage.repositories._json import load_json

TimelineInvariantHook = Callable[[Connection, CaseState, TimelineState], Sequence[str]]


@dataclass(frozen=True, slots=True)
class ValidationViolation:
    code: str
    message: str
    case_id: str | None = None
    event_type: str | None = None


@dataclass(frozen=True, slots=True)
class ValidationFailed:
    violations: tuple[ValidationViolation, ...]


CASE_STATE_EVENT_NAMES = frozenset(
    {
        "CaseCreated",
        "CaseRenamed",
        "CaseCopied",
        "CaseMoved",
        "CaseClosed",
        "CaseTrashed",
        "CaseAssetScopeChanged",
        "DecisionCreated",
        "DecisionAnswered",
        "DecisionCancelled",
        "BriefUpdated",
        "ContentPlanUpdated",
        "AudioPlanUpdated",
        "CutPlanUpdated",
        "PostprocessPlanUpdated",
        "CandidatePackCreated",
        "TimelineVersionCreated",
        "TimelineVersionRestored",
        "TimelineValidated",
        "TimelineValidationFailed",
        "PreviewRendered",
        "PreviewViewed",
        "ExportCompleted",
        "JobEnqueued",
        "JobProgress",
        "JobSucceeded",
        "JobFailed",
        "JobCancelled",
        "TurnEnded",
        "CapabilityDegraded",
    }
)
PROJECT_ASSET_EVENT_NAMES = frozenset(
    {
        "AssetImported",
        "AssetProbed",
        "ProxyGenerated",
        "AnnotationCompleted",
        "AnnotationFailed",
        "AssetInvalidated",
        "AssetIndexReady",
        "AssetIndexFailed",
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingCompleted",
        "MaterialUnderstandingFailed",
        "AssetLinked",
        "AssetUnlinked",
    }
)


def validate_before_commit(
    connection: Connection,
    *,
    case_states: Mapping[str, CaseState],
    events: Sequence[DomainEventBase],
    timeline_invariant_hook: TimelineInvariantHook | None = None,
) -> ValidationFailed | None:
    violations: list[ValidationViolation] = []
    for case_state in case_states.values():
        violations.extend(_validate_case_references(connection, case_state))
        violations.extend(
            _validate_timeline_structure(connection, case_state, timeline_invariant_hook)
        )
        violations.extend(_validate_asset_scope(connection, case_state))
    violations.extend(_validate_event_scope(events))
    if violations:
        return ValidationFailed(violations=tuple(violations))
    return None


def _validate_case_references(
    connection: Connection,
    case_state: CaseState,
) -> list[ValidationViolation]:
    violations: list[ValidationViolation] = []
    if case_state.timeline_current_version is not None and not _timeline_exists(
        connection,
        case_state.case_id,
        case_state.timeline_current_version,
    ):
        violations.append(
            ValidationViolation(
                code="missing_timeline_current_version",
                message="timeline_current_version does not exist for this case",
                case_id=case_state.case_id,
            )
        )
    if case_state.preview_current_id is not None and not _preview_belongs_to_case(
        connection,
        case_state.preview_current_id,
        case_state.case_id,
    ):
        violations.append(
            ValidationViolation(
                code="invalid_preview_current_id",
                message="preview_current_id does not belong to this case",
                case_id=case_state.case_id,
            )
        )
    if case_state.candidate_pack_id is not None and not _candidate_pack_belongs_to_case(
        connection,
        case_state.candidate_pack_id,
        case_state.case_id,
    ):
        violations.append(
            ValidationViolation(
                code="invalid_candidate_pack_id",
                message="candidate_pack_id does not belong to this case",
                case_id=case_state.case_id,
            )
        )
    if case_state.pending_decision_id is not None:
        decision = _decision_row(connection, case_state.pending_decision_id)
        if decision is None or decision.get("case_id") != case_state.case_id:
            violations.append(
                ValidationViolation(
                    code="invalid_pending_decision_id",
                    message="pending_decision_id does not belong to this case",
                    case_id=case_state.case_id,
                )
            )
        elif decision.get("status") != "pending":
            violations.append(
                ValidationViolation(
                    code="pending_decision_not_pending",
                    message="pending_decision_id must point to a pending decision",
                    case_id=case_state.case_id,
                )
            )
    return violations


def _validate_timeline_structure(
    connection: Connection,
    case_state: CaseState,
    timeline_invariant_hook: TimelineInvariantHook | None,
) -> list[ValidationViolation]:
    if case_state.timeline_current_version is None:
        return []
    row = connection.execute(
        select(schema.timeline_versions).where(
            schema.timeline_versions.c.case_id == case_state.case_id,
            schema.timeline_versions.c.version == case_state.timeline_current_version,
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
                case_id=case_state.case_id,
            )
        ]
    violations: list[ValidationViolation] = []
    if (
        timeline.case_id != case_state.case_id
        or timeline.version != case_state.timeline_current_version
    ):
        violations.append(
            ValidationViolation(
                code="timeline_identity_mismatch",
                message=(
                    "timeline document case_id/version must match the timeline row and CaseState"
                ),
                case_id=case_state.case_id,
            )
        )
    if timeline_invariant_hook is not None:
        for message in timeline_invariant_hook(connection, case_state, timeline):
            violations.append(
                ValidationViolation(
                    code="timeline_frame_invariant_failed",
                    message=message,
                    case_id=case_state.case_id,
                )
            )
    return violations


def _validate_asset_scope(
    connection: Connection,
    case_state: CaseState,
) -> list[ValidationViolation]:
    asset_ids = set(case_state.selected_asset_ids) | set(case_state.disabled_asset_ids)
    if not asset_ids:
        return []
    linked_ids = set(
        connection.execute(
            select(schema.project_asset_links.c.asset_id).where(
                schema.project_asset_links.c.project_id == case_state.project_id,
                schema.project_asset_links.c.asset_id.in_(asset_ids),
            )
        ).scalars()
    )
    missing = sorted(asset_ids - linked_ids)
    if not missing:
        return []
    return [
        ValidationViolation(
            code="case_assets_not_linked_to_project",
            message="selected/disabled asset ids must be linked to the case project: "
            + ", ".join(missing),
            case_id=case_state.case_id,
        )
    ]


def _validate_event_scope(events: Sequence[DomainEventBase]) -> list[ValidationViolation]:
    violations: list[ValidationViolation] = []
    for event in events:
        if event.event in PROJECT_ASSET_EVENT_NAMES and event.case_id is not None:
            violations.append(
                ValidationViolation(
                    code="project_asset_event_has_case_scope",
                    message="project asset events must not carry or modify case scope",
                    case_id=event.case_id,
                    event_type=event.event,
                )
            )
        if event.event in CASE_STATE_EVENT_NAMES and event.event in {
            "AssetLinked",
            "AssetUnlinked",
        }:
            violations.append(
                ValidationViolation(
                    code="case_event_modified_project_asset_pool",
                    message="case-scoped events must not modify the project asset pool",
                    case_id=event.case_id,
                    event_type=event.event,
                )
            )
    return violations


def _timeline_exists(connection: Connection, case_id: str, version: int) -> bool:
    row = connection.execute(
        select(schema.timeline_versions.c.timeline_id).where(
            schema.timeline_versions.c.case_id == case_id,
            schema.timeline_versions.c.version == version,
        )
    ).first()
    return row is not None


def _preview_belongs_to_case(connection: Connection, preview_id: str, case_id: str) -> bool:
    row = connection.execute(
        select(schema.previews.c.preview_id).where(
            schema.previews.c.preview_id == preview_id,
            schema.previews.c.case_id == case_id,
        )
    ).first()
    return row is not None


def _candidate_pack_belongs_to_case(
    connection: Connection,
    candidate_pack_id: str,
    case_id: str,
) -> bool:
    row = connection.execute(
        select(schema.candidate_packs.c.candidate_pack_id).where(
            schema.candidate_packs.c.candidate_pack_id == candidate_pack_id,
            schema.candidate_packs.c.case_id == case_id,
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
