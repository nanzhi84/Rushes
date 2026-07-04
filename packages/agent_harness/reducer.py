"""Reducer single write path from PRD §4.5."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, cast

from sqlalchemy import delete, select, update
from sqlalchemy.engine import Connection, Engine

from contracts.case import CaseState, RunningJobRef
from contracts.decision import Decision, DecisionAnswer
from contracts.events import (
    Actor,
    DecisionEventBase,
    DomainEventBase,
    VersionMode,
)
from contracts.timeline import TimelineState
from domain.decision_effects import (
    HarnessFollowup,
    pending_tool_call_status_after_answer,
    reduce_decision_answer,
    validate_decision_registered,
)
from events.event_log import append_domain_event, validate_domain_event
from storage import schema
from storage.db import begin_immediate
from storage.repositories import CasesRepository, DecisionsRepository, EventLogRepository
from storage.repositories._json import dump_json, load_json

from .state_validator import TimelineInvariantHook, ValidationFailed, validate_before_commit


@dataclass(frozen=True, slots=True)
class VersionConflict:
    case_id: str
    expected_base_version: int | None
    actual_state_version: int
    event_type: str


@dataclass(frozen=True, slots=True)
class AppliedEvent:
    event_id: int
    event_type: str
    state_version: int | None


@dataclass(frozen=True, slots=True)
class ReducerApplyResult:
    status: Literal["applied", "version_conflict", "validation_failed"]
    applied_events: tuple[AppliedEvent, ...] = ()
    followups: tuple[HarnessFollowup, ...] = ()
    case_state_versions: Mapping[str, int] = field(default_factory=dict)
    conflict: VersionConflict | None = None
    validation_failed: ValidationFailed | None = None
    skipped_events: int = 0


class _AbortReducer(Exception):
    def __init__(self, result: ReducerApplyResult) -> None:
        super().__init__(result.status)
        self.result = result


class _ReducerContext:
    def __init__(
        self,
        connection: Connection,
        *,
        actor: Actor,
        created_at: str,
        base_version: int | None,
    ) -> None:
        self.connection = connection
        self.actor = actor
        self.created_at = created_at
        self.base_version = base_version
        self.case_states: dict[str, CaseState] = {}
        self.original_case_versions: dict[str, int] = {}
        self.touched_case_ids: set[str] = set()
        self.events_to_log: list[DomainEventBase] = []
        self.followups: list[HarnessFollowup] = []
        self.skipped_events = 0

    def load_case(self, case_id: str) -> CaseState:
        existing = self.case_states.get(case_id)
        if existing is not None:
            return existing
        row = CasesRepository(self.connection).get(case_id)
        if row is None:
            raise ValueError(f"case not found: {case_id}")
        state = CaseState.model_validate(row)
        self.case_states[case_id] = state
        self.original_case_versions[case_id] = state.state_version
        return state

    def set_case_state(self, case_state: CaseState, *, touch: bool = True) -> None:
        self.case_states[case_state.case_id] = case_state
        if touch:
            self.touched_case_ids.add(case_state.case_id)
            self.original_case_versions.setdefault(case_state.case_id, case_state.state_version)

    def patch_case_state(self, case_id: str, patch: Mapping[str, Any]) -> CaseState:
        state = self.load_case(case_id)
        data = state.model_dump(mode="json")
        data.update(dict(patch))
        updated = CaseState.model_validate(data)
        self.set_case_state(updated)
        return updated


def apply(
    events: Sequence[DomainEventBase | dict[str, Any]],
    *,
    engine: Engine,
    base_version: int | None,
    actor: Actor,
    created_at: str | None = None,
    timeline_invariant_hook: TimelineInvariantHook | None = None,
) -> ReducerApplyResult:
    """Apply a batch of DomainEvents in one BEGIN IMMEDIATE transaction."""

    timestamp = created_at or _now_iso()
    parsed_events = tuple(
        _normalize_event(event, actor=actor, base_version=base_version) for event in events
    )
    try:
        with begin_immediate(engine) as connection:
            context = _ReducerContext(
                connection,
                actor=actor,
                created_at=timestamp,
                base_version=base_version,
            )
            _preflight_strict_versions(context, parsed_events)
            for event in parsed_events:
                if _is_duplicate_merge_event(connection, event):
                    context.skipped_events += 1
                    continue
                _apply_event(context, event)

            validation = validate_before_commit(
                connection,
                case_states=context.case_states,
                events=context.events_to_log,
                timeline_invariant_hook=timeline_invariant_hook,
            )
            if validation is not None:
                raise _AbortReducer(
                    ReducerApplyResult(status="validation_failed", validation_failed=validation)
                )

            _persist_touched_case_states(context)
            applied_events = _append_events(context)
            return ReducerApplyResult(
                status="applied",
                applied_events=tuple(applied_events),
                followups=tuple(context.followups),
                case_state_versions={
                    case_id: state.state_version for case_id, state in context.case_states.items()
                },
                skipped_events=context.skipped_events,
            )
    except _AbortReducer as abort:
        return abort.result


def _normalize_event(
    event: DomainEventBase | dict[str, Any],
    *,
    actor: Actor,
    base_version: int | None,
) -> DomainEventBase:
    parsed = validate_domain_event(event)
    mode = _event_version_mode(parsed)
    updates: dict[str, Any] = {"actor": actor}
    if mode == "strict" and parsed.base_version is None:
        updates["base_version"] = base_version
    if mode == "merge":
        updates["base_version"] = None
    normalized = parsed.model_copy(update=updates)
    return validate_domain_event(normalized)


def _preflight_strict_versions(
    context: _ReducerContext,
    events: Sequence[DomainEventBase],
) -> None:
    for event in events:
        if _event_version_mode(event) != "strict":
            continue
        case_id = _event_case_id(event)
        if case_id is None:
            raise ValueError(f"strict event requires case_id: {event.event}")
        case_state = context.load_case(case_id)
        expected = context.base_version if context.base_version is not None else event.base_version
        if expected != case_state.state_version:
            raise _AbortReducer(
                ReducerApplyResult(
                    status="version_conflict",
                    conflict=VersionConflict(
                        case_id=case_id,
                        expected_base_version=expected,
                        actual_state_version=case_state.state_version,
                        event_type=event.event,
                    ),
                )
            )


def _apply_event(context: _ReducerContext, event: DomainEventBase) -> None:
    name = event.event
    event_log_index = len(context.events_to_log)
    if name == "ProjectCreated":
        _apply_project_created(context, event)
    elif name in {"ProjectRenamed", "ProjectTrashed", "ProjectCopied"}:
        _apply_project_update(context, event)
    elif name == "CaseCreated":
        _apply_case_created(context, event)
    elif name in {"CaseRenamed", "CaseCopied", "CaseMoved", "CaseClosed", "CaseTrashed"}:
        _apply_case_update(context, event)
    elif name in {
        "AssetImported",
        "AssetProbed",
        "ProxyGenerated",
        "AnnotationCompleted",
        "AnnotationFailed",
        "AssetInvalidated",
    }:
        _apply_asset_event(context, event)
    elif name == "AssetLinked":
        _apply_asset_linked(context, event)
    elif name == "AssetUnlinked":
        _apply_asset_unlinked(context, event)
    elif name == "CaseAssetScopeChanged":
        _apply_case_asset_scope_changed(context, event)
    elif name == "DecisionCreated":
        _apply_decision_created(context, cast(DecisionEventBase, event))
    elif name == "DecisionAnswered":
        _apply_decision_answered(context, cast(DecisionEventBase, event))
    elif name == "DecisionCancelled":
        _apply_decision_cancelled(context, cast(DecisionEventBase, event))
    elif name in {
        "BriefUpdated",
        "ContentPlanUpdated",
        "AudioPlanUpdated",
        "CutPlanUpdated",
        "PostprocessPlanUpdated",
    }:
        _apply_plan_updated(context, event)
    elif name == "CandidatePackCreated":
        _apply_candidate_pack_created(context, event)
    elif name == "TimelineVersionCreated":
        _apply_timeline_version_created(context, event)
    elif name == "TimelineVersionRestored":
        _apply_timeline_version_restored(context, event)
    elif name in {"TimelineValidated", "TimelineValidationFailed"}:
        _apply_timeline_validation_event(context, event)
    elif name == "PreviewRendered":
        _apply_preview_rendered(context, event)
    elif name == "PreviewViewed":
        _apply_preview_viewed(context, event)
    elif name == "ExportCompleted":
        _apply_export_completed(context, event)
    elif name == "MemoryCandidateExtracted":
        _apply_memory_candidate_extracted(context, event)
    elif name == "MemoryCandidateDiscarded":
        _apply_memory_candidate_discarded(context, event)
    elif name == "MemorySaved":
        _apply_memory_saved(context, event)
    elif name in {"JobEnqueued", "JobProgress", "JobSucceeded", "JobFailed", "JobCancelled"}:
        _apply_job_event(context, event)
    elif name in {
        "PolicyRefusal",
        "ProviderCallRecorded",
        "ContextCompacted",
        "TurnEnded",
        "CapabilityDegraded",
        "SecurityRefusal",
    }:
        pass
    else:
        raise ValueError(f"unhandled domain event: {name}")
    context.events_to_log.insert(event_log_index, event)


def _apply_project_created(context: _ReducerContext, event: DomainEventBase) -> None:
    project_id = _required_attr(event, "project_id")
    if _row_exists(context.connection, schema.projects, "project_id", project_id):
        return
    context.connection.execute(
        schema.projects.insert().values(
            project_id=project_id,
            name=str(
                getattr(event, "name", None) or event.payload.get("name") or "Untitled Project"
            ),
            status=str(event.payload.get("status", "active")),
            defaults=dump_json(event.payload.get("defaults", {})),
            created_at=str(event.payload.get("created_at", context.created_at)),
            updated_at=str(event.payload.get("updated_at", context.created_at)),
        )
    )


def _apply_project_update(context: _ReducerContext, event: DomainEventBase) -> None:
    project_id = _required_attr(event, "project_id")
    if not _row_exists(context.connection, schema.projects, "project_id", project_id):
        _apply_project_created(context, event)
    values: dict[str, Any] = {"updated_at": context.created_at}
    if event.event in {"ProjectRenamed", "ProjectCopied"}:
        values["name"] = str(
            getattr(event, "name", None) or event.payload.get("name") or "Copied Project"
        )
    if event.event == "ProjectTrashed":
        values["status"] = "trashed"
    context.connection.execute(
        update(schema.projects).where(schema.projects.c.project_id == project_id).values(**values)
    )


def _apply_case_created(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    if _row_exists(context.connection, schema.cases, "case_id", case_id):
        context.load_case(case_id)
        return
    project_id = _required_attr(event, "project_id")
    case_state = CaseState.model_validate(
        {
            "case_id": case_id,
            "project_id": project_id,
            "name": event.payload.get("name", "Untitled Case"),
            "state_version": int(event.payload.get("state_version", 0)),
            "status": event.payload.get("status", "active"),
            "pending_decision_id": None,
            "running_jobs": [],
            "last_error": None,
            "brief": event.payload.get("brief", {"goal": ""}),
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
    )
    context.connection.execute(schema.cases.insert().values(**_case_insert_values(case_state)))
    context.case_states[case_id] = case_state
    context.original_case_versions[case_id] = case_state.state_version


def _apply_case_update(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    state = context.load_case(case_id)
    patch: dict[str, Any] = {}
    if event.event in {"CaseRenamed", "CaseCopied"}:
        patch["name"] = str(getattr(event, "name", None) or event.payload.get("name") or state.name)
    if event.event == "CaseMoved":
        patch["project_id"] = (
            getattr(event, "target_project_id", None)
            or event.payload.get("target_project_id")
            or state.project_id
        )
    if event.event == "CaseClosed":
        patch["status"] = "closed"
    if event.event == "CaseTrashed":
        patch["status"] = "trashed"
    if patch:
        context.patch_case_state(case_id, patch)


def _apply_asset_event(context: _ReducerContext, event: DomainEventBase) -> None:
    asset_id = _required_attr(event, "asset_id")
    payload = event.payload
    object_hash = payload.get("object_hash")
    proxy_object_hash = payload.get("proxy_object_hash")
    if isinstance(object_hash, str):
        _ensure_object(context, object_hash)
    if isinstance(proxy_object_hash, str):
        _ensure_object(context, proxy_object_hash)
    if not _row_exists(context.connection, schema.assets, "asset_id", asset_id):
        values = _asset_insert_values(asset_id, payload)
        context.connection.execute(schema.assets.insert().values(**values))
    updates = _asset_update_values_for_event(event)
    if updates:
        context.connection.execute(
            update(schema.assets).where(schema.assets.c.asset_id == asset_id).values(**updates)
        )


def _apply_asset_linked(context: _ReducerContext, event: DomainEventBase) -> None:
    project_id = _required_attr(event, "project_id")
    asset_id = _required_attr(event, "asset_id")
    if not _row_exists_pair(
        context.connection,
        schema.project_asset_links,
        {"project_id": project_id, "asset_id": asset_id},
    ):
        context.connection.execute(
            schema.project_asset_links.insert().values(
                project_id=project_id,
                asset_id=asset_id,
                enabled=bool(event.payload.get("enabled", True)),
                linked_at=str(event.payload.get("linked_at", context.created_at)),
                note=str(event.payload.get("note", "")),
            )
        )
    else:
        context.connection.execute(
            update(schema.project_asset_links)
            .where(schema.project_asset_links.c.project_id == project_id)
            .where(schema.project_asset_links.c.asset_id == asset_id)
            .values(enabled=bool(event.payload.get("enabled", True)))
        )


def _apply_asset_unlinked(context: _ReducerContext, event: DomainEventBase) -> None:
    project_id = _required_attr(event, "project_id")
    asset_id = _required_attr(event, "asset_id")
    context.connection.execute(
        delete(schema.project_asset_links)
        .where(schema.project_asset_links.c.project_id == project_id)
        .where(schema.project_asset_links.c.asset_id == asset_id)
    )


def _apply_case_asset_scope_changed(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    patch: dict[str, Any] = {}
    if "selected_asset_ids" in event.payload:
        patch["selected_asset_ids"] = event.payload["selected_asset_ids"]
    if "disabled_asset_ids" in event.payload:
        patch["disabled_asset_ids"] = event.payload["disabled_asset_ids"]
    context.patch_case_state(case_id, patch)


def _apply_decision_created(context: _ReducerContext, event: DecisionEventBase) -> None:
    decision = _decision_from_created_event(context, event)
    validate_decision_registered(decision)
    DecisionsRepository(context.connection).insert(_decision_insert_values(decision))
    if decision.scope_type == "case" and decision.blocking and decision.case_id is not None:
        context.patch_case_state(decision.case_id, {"pending_decision_id": decision.decision_id})


def _apply_decision_answered(context: _ReducerContext, event: DecisionEventBase) -> None:
    decision_row = DecisionsRepository(context.connection).get(event.decision_id)
    if decision_row is None:
        raise ValueError(f"decision not found: {event.decision_id}")
    decision = Decision.model_validate(decision_row)
    answer = _answer_from_event(event)
    pending_status = pending_tool_call_status_after_answer(decision.pending_tool_call, answer)
    update_values: dict[str, Any] = {
        "status": "answered",
        "answer": dump_json(answer.model_dump(mode="json")),
    }
    if pending_status is not None:
        update_values["pending_tool_call_status"] = pending_status
    context.connection.execute(
        update(schema.decisions)
        .where(schema.decisions.c.decision_id == decision.decision_id)
        .values(**update_values)
    )
    if decision.scope_type != "case" or decision.case_id is None:
        return
    state = context.load_case(decision.case_id)
    effect = reduce_decision_answer(state, decision, answer)
    if effect.state_patch:
        state = context.patch_case_state(decision.case_id, effect.state_patch)
    if state.pending_decision_id == decision.decision_id:
        context.patch_case_state(decision.case_id, {"pending_decision_id": None})
    for followup_event in effect.followup_events:
        normalized = followup_event.model_copy(
            update={"actor": context.actor, "base_version": None}
        )
        context.events_to_log.append(validate_domain_event(normalized))
    context.followups.extend(effect.followups)


def _apply_decision_cancelled(context: _ReducerContext, event: DecisionEventBase) -> None:
    decision_row = DecisionsRepository(context.connection).get(event.decision_id)
    if decision_row is None:
        raise ValueError(f"decision not found: {event.decision_id}")
    decision = Decision.model_validate(decision_row)
    context.connection.execute(
        update(schema.decisions)
        .where(schema.decisions.c.decision_id == decision.decision_id)
        .values(status="cancelled", pending_tool_call_status="discarded")
    )
    if decision.scope_type == "case" and decision.case_id is not None:
        state = context.load_case(decision.case_id)
        if state.pending_decision_id == decision.decision_id:
            context.patch_case_state(decision.case_id, {"pending_decision_id": None})


def _apply_plan_updated(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    payload_key_by_event = {
        "BriefUpdated": "brief",
        "ContentPlanUpdated": "content_plan",
        "AudioPlanUpdated": "audio_plan",
        "CutPlanUpdated": "cut_plan",
        "PostprocessPlanUpdated": "postprocess_plan",
    }
    key = payload_key_by_event[event.event]
    if key not in event.payload:
        return
    context.patch_case_state(case_id, {key: event.payload[key]})


def _apply_candidate_pack_created(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    candidate_pack_id = getattr(event, "candidate_pack_id", None) or event.payload.get(
        "candidate_pack_id"
    )
    if not isinstance(candidate_pack_id, str):
        raise ValueError("CandidatePackCreated requires candidate_pack_id")
    if not _row_exists(
        context.connection,
        schema.candidate_packs,
        "candidate_pack_id",
        candidate_pack_id,
    ):
        pack = event.payload.get("candidate_pack", {})
        slots = event.payload.get("slots", pack.get("slots", []) if isinstance(pack, dict) else [])
        context.connection.execute(
            schema.candidate_packs.insert().values(
                candidate_pack_id=candidate_pack_id,
                case_id=case_id,
                slots=dump_json(slots),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    context.patch_case_state(case_id, {"candidate_pack_id": candidate_pack_id})


def _apply_timeline_version_created(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError("TimelineVersionCreated requires timeline_version")
    document = _timeline_document_from_event(event, case_id, version)
    if not _timeline_exists(context.connection, case_id, version):
        context.connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id=str(event.payload.get("timeline_id", f"{case_id}:v{version}")),
                case_id=case_id,
                version=version,
                parent_version=getattr(event, "parent_version", None),
                created_by_patch_id=getattr(event, "patch_id", None),
                document_json=dump_json(document),
                validation_report=(
                    None
                    if event.payload.get("validation_report") is None
                    else dump_json(event.payload["validation_report"])
                ),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    patch: dict[str, Any] = {
        "timeline_current_version": version,
        "timeline_validated": False,
    }
    if _timeline_creation_resets_rough_cut(event):
        patch["rough_cut_approved"] = False
    context.patch_case_state(case_id, patch)


def _apply_timeline_version_restored(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError("TimelineVersionRestored requires timeline_version")
    state = context.load_case(case_id)
    context.patch_case_state(
        case_id,
        {
            "timeline_current_version": version,
            "timeline_validated": False,
            "rough_cut_approved": version == state.rough_cut_approved_version,
        },
    )


def _apply_timeline_validation_event(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError(f"{event.event} requires timeline_version")
    valid = event.event == "TimelineValidated"
    report = event.payload.get("validation_report", {"valid": valid, "checks": []})
    context.connection.execute(
        update(schema.timeline_versions)
        .where(schema.timeline_versions.c.case_id == case_id)
        .where(schema.timeline_versions.c.version == version)
        .values(validation_report=dump_json(report))
    )
    state = context.load_case(case_id)
    if state.timeline_current_version == version:
        context.patch_case_state(case_id, {"timeline_validated": valid})


def _apply_preview_rendered(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    preview_id = _required_attr(event, "artifact_id")
    timeline_version = _required_int_attr(event, "timeline_version")
    object_hash = str(event.payload.get("object_hash", preview_id))
    _ensure_object(context, object_hash)
    if not _row_exists(context.connection, schema.previews, "preview_id", preview_id):
        context.connection.execute(
            schema.previews.insert().values(
                preview_id=preview_id,
                case_id=case_id,
                timeline_version=timeline_version,
                object_hash=object_hash,
                quality=dump_json(event.payload.get("quality", {})),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    state = context.load_case(case_id)
    if state.timeline_current_version == timeline_version:
        context.patch_case_state(case_id, {"preview_current_id": preview_id})


def _apply_preview_viewed(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    preview_id = _required_attr(event, "preview_id")
    if _preview_belongs_to_case(context.connection, preview_id, case_id):
        context.patch_case_state(case_id, {"last_viewed_preview_id": preview_id})


def _apply_export_completed(context: _ReducerContext, event: DomainEventBase) -> None:
    case_id = _required_attr(event, "case_id")
    export_id = _required_attr(event, "artifact_id")
    timeline_version = _required_int_attr(event, "timeline_version")
    object_hash = str(event.payload.get("object_hash", export_id))
    _ensure_object(context, object_hash)
    if not _row_exists(context.connection, schema.exports, "export_id", export_id):
        context.connection.execute(
            schema.exports.insert().values(
                export_id=export_id,
                case_id=case_id,
                timeline_version=timeline_version,
                object_hash=object_hash,
                quality=dump_json(event.payload.get("quality", {})),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    state = context.load_case(case_id)
    if state.timeline_current_version == timeline_version:
        context.patch_case_state(case_id, {"export_current_id": export_id})


def _apply_memory_candidate_extracted(context: _ReducerContext, event: DomainEventBase) -> None:
    candidate_id = _required_attr(event, "candidate_id")
    if _row_exists(context.connection, schema.memory_candidates, "candidate_id", candidate_id):
        return
    context.connection.execute(
        schema.memory_candidates.insert().values(
            candidate_id=candidate_id,
            case_id=event.case_id or str(event.payload.get("case_id", "")),
            content=str(event.payload.get("content", "")),
            suggested_scope=str(event.payload.get("suggested_scope", "user")),
            status=str(event.payload.get("status", "pending")),
            saved_memory_id=None,
            created_at=str(event.payload.get("created_at", context.created_at)),
        )
    )


def _apply_memory_candidate_discarded(context: _ReducerContext, event: DomainEventBase) -> None:
    candidate_id = _required_attr(event, "candidate_id")
    if _row_exists(context.connection, schema.memory_candidates, "candidate_id", candidate_id):
        context.connection.execute(
            update(schema.memory_candidates)
            .where(schema.memory_candidates.c.candidate_id == candidate_id)
            .values(status="discarded")
        )


def _apply_memory_saved(context: _ReducerContext, event: DomainEventBase) -> None:
    memory_id = _required_attr(event, "memory_id")
    candidate_id = getattr(event, "candidate_id", None) or event.payload.get("candidate_id")
    if not _row_exists(context.connection, schema.memories, "memory_id", memory_id):
        context.connection.execute(
            schema.memories.insert().values(
                memory_id=memory_id,
                scope=str(event.payload.get("scope", "user")),
                project_id=event.payload.get("project_id"),
                content=str(event.payload.get("content", "")),
                tags=dump_json(event.payload.get("tags", [])),
                created_from_case_id=event.payload.get("created_from_case_id"),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    if isinstance(candidate_id, str):
        context.connection.execute(
            update(schema.memory_candidates)
            .where(schema.memory_candidates.c.candidate_id == candidate_id)
            .values(status="saved", saved_memory_id=memory_id)
        )


def _apply_job_event(context: _ReducerContext, event: DomainEventBase) -> None:
    job_id = _required_attr(event, "job_id")
    requested_by_case_id = getattr(event, "requested_by_case_id", None)
    if not _row_exists(context.connection, schema.jobs, "job_id", job_id):
        _insert_job(context, event)
    status = _job_status_for_event(event)
    values: dict[str, Any] = {"status": status}
    progress = getattr(event, "progress", None)
    if progress is not None:
        values["progress"] = progress
    if status in {"succeeded", "failed", "cancelled"}:
        values["finished_at"] = str(event.payload.get("finished_at", context.created_at))
    context.connection.execute(
        update(schema.jobs).where(schema.jobs.c.job_id == job_id).values(**values)
    )
    if isinstance(requested_by_case_id, str):
        _update_running_jobs(context, requested_by_case_id, event, status)


def _insert_job(context: _ReducerContext, event: DomainEventBase) -> None:
    job_id = _required_attr(event, "job_id")
    kind = str(event.payload.get("kind", "unknown"))
    context.connection.execute(
        schema.jobs.insert().values(
            job_id=job_id,
            kind=kind,
            status=_job_status_for_event(event),
            project_id=event.project_id,
            case_id=event.case_id,
            requested_by_case_id=getattr(event, "requested_by_case_id", None),
            asset_id=event.payload.get("asset_id"),
            idempotency_key=str(event.payload.get("idempotency_key", job_id)),
            payload_json=dump_json(event.payload.get("job_payload", {})),
            result_json=None,
            error_json=None,
            attempts=int(event.payload.get("attempts", 0)),
            max_retries=int(event.payload.get("max_retries", 0)),
            next_run_at=str(event.payload.get("next_run_at", context.created_at)),
            progress=getattr(event, "progress", None),
            worker_id=event.payload.get("worker_id"),
            heartbeat_at=event.payload.get("heartbeat_at"),
            created_at=str(event.payload.get("created_at", context.created_at)),
            started_at=event.payload.get("started_at"),
            finished_at=event.payload.get("finished_at"),
        )
    )


def _update_running_jobs(
    context: _ReducerContext,
    case_id: str,
    event: DomainEventBase,
    status: str,
) -> None:
    state = context.load_case(case_id)
    existing = [job.model_dump(mode="json") for job in state.running_jobs]
    job_id = _required_attr(event, "job_id")
    if status in {"succeeded", "failed", "cancelled"}:
        updated = [job for job in existing if job["job_id"] != job_id]
    else:
        without = [job for job in existing if job["job_id"] != job_id]
        without.append(
            RunningJobRef(
                job_id=job_id,
                kind=str(event.payload.get("kind", "unknown")),
                status=cast(
                    Literal["pending", "running", "succeeded", "failed", "cancelled"],
                    status,
                ),
                progress=getattr(event, "progress", None),
            ).model_dump(mode="json")
        )
        updated = without
    context.patch_case_state(case_id, {"running_jobs": updated})


def _persist_touched_case_states(context: _ReducerContext) -> None:
    repository = CasesRepository(context.connection)
    for case_id in sorted(context.touched_case_ids):
        state = context.case_states[case_id]
        expected = context.original_case_versions[case_id]
        conflict = repository.update_with_state_version(
            case_id,
            expected,
            _case_update_values(state),
        )
        if conflict is not None:
            raise _AbortReducer(
                ReducerApplyResult(
                    status="version_conflict",
                    conflict=VersionConflict(
                        case_id=case_id,
                        expected_base_version=expected,
                        actual_state_version=state.state_version,
                        event_type="CaseStateUpdate",
                    ),
                )
            )
        context.case_states[case_id] = state.model_copy(update={"state_version": expected + 1})


def _append_events(context: _ReducerContext) -> list[AppliedEvent]:
    repository = EventLogRepository(context.connection)
    applied: list[AppliedEvent] = []
    for event in context.events_to_log:
        state_version = _state_version_for_event(context, event)
        event_id = append_domain_event(
            repository,
            event,
            state_version=state_version,
            created_at=context.created_at,
        )
        applied.append(
            AppliedEvent(
                event_id=event_id,
                event_type=event.event,
                state_version=state_version,
            )
        )
    return applied


def _state_version_for_event(context: _ReducerContext, event: DomainEventBase) -> int | None:
    case_id = _event_case_id(event)
    if case_id is None:
        requested_case_id = getattr(event, "requested_by_case_id", None)
        case_id = requested_case_id if isinstance(requested_case_id, str) else None
    if case_id is None:
        return None
    state = context.case_states.get(case_id)
    return None if state is None else state.state_version


def _event_version_mode(event: DomainEventBase) -> VersionMode:
    if isinstance(event, DecisionEventBase):
        return type(event).reducer_version_mode(event.scope_type)
    return type(event).version_mode


def _event_case_id(event: DomainEventBase) -> str | None:
    case_id = getattr(event, "case_id", None)
    return case_id if isinstance(case_id, str) else None


def _is_duplicate_merge_event(connection: Connection, event: DomainEventBase) -> bool:
    if _event_version_mode(event) != "merge":
        return False
    merge_key = type(event).merge_key
    if not merge_key:
        return False
    rows = connection.execute(
        select(schema.event_log.c.payload_json).where(schema.event_log.c.event_type == event.event)
    ).all()
    for row in rows:
        payload = load_json(row._mapping["payload_json"])
        if isinstance(payload, dict) and all(
            payload.get(key) == getattr(event, key) for key in merge_key
        ):
            return True
    return False


def _decision_from_created_event(context: _ReducerContext, event: DecisionEventBase) -> Decision:
    decision_data = dict(event.payload.get("decision", {}))
    if event.scope_type == "case" and event.case_id is not None:
        case_state = context.load_case(event.case_id)
        decision_data.setdefault("project_id", case_state.project_id)
        decision_data.setdefault("case_id", event.case_id)
    decision_data.setdefault("decision_id", event.decision_id)
    decision_data.setdefault("scope_type", event.scope_type)
    decision_data.setdefault("project_id", event.project_id)
    decision_data.setdefault("case_id", event.case_id)
    decision_data.setdefault("type", event.payload.get("type", "generic"))
    decision_data.setdefault("question", event.payload.get("question", ""))
    decision_data.setdefault("options", event.payload.get("options", []))
    decision_data.setdefault("status", event.payload.get("status", "pending"))
    decision_data.setdefault("answer", event.payload.get("answer"))
    decision_data.setdefault("pending_tool_call", event.payload.get("pending_tool_call"))
    decision_data.setdefault(
        "pending_tool_call_status",
        event.payload.get("pending_tool_call_status"),
    )
    decision_data.setdefault(
        "blocking",
        bool(event.payload.get("blocking", event.scope_type == "case")),
    )
    decision_data.setdefault(
        "created_by_tool_call_id",
        event.payload.get("created_by_tool_call_id"),
    )
    return Decision.model_validate(decision_data)


def _answer_from_event(event: DecisionEventBase) -> DecisionAnswer:
    answer_data = dict(event.payload.get("answer", {}))
    answer_data.setdefault("option_id", event.payload.get("option_id"))
    answer_data.setdefault("free_text", event.payload.get("free_text"))
    answer_data.setdefault("answered_via", event.payload.get("answered_via", "button"))
    answer_data.setdefault("payload", event.payload.get("answer_payload", {}))
    return DecisionAnswer.model_validate(answer_data)


def _decision_insert_values(decision: Decision) -> dict[str, Any]:
    values = decision.model_dump(mode="json")
    values.pop("allow_free_text", None)
    return {
        **values,
        "options": values["options"],
        "answer": values["answer"],
        "pending_tool_call": values["pending_tool_call"],
    }


def _case_insert_values(case_state: CaseState) -> dict[str, Any]:
    values = _case_update_values(case_state)
    values["case_id"] = case_state.case_id
    values["state_version"] = case_state.state_version
    return _encode_case_values(values)


def _case_update_values(case_state: CaseState) -> dict[str, Any]:
    return {
        "project_id": case_state.project_id,
        "name": case_state.name,
        "status": case_state.status,
        "pending_decision_id": case_state.pending_decision_id,
        "running_jobs": [job.model_dump(mode="json") for job in case_state.running_jobs],
        "last_error": None
        if case_state.last_error is None
        else case_state.last_error.model_dump(mode="json"),
        "brief": case_state.brief.model_dump(mode="json"),
        "content_plan": case_state.content_plan,
        "audio_plan": None
        if case_state.audio_plan is None
        else case_state.audio_plan.model_dump(mode="json"),
        "cut_plan": None
        if case_state.cut_plan is None
        else case_state.cut_plan.model_dump(mode="json", by_alias=True),
        "candidate_pack_id": case_state.candidate_pack_id,
        "timeline_current_version": case_state.timeline_current_version,
        "timeline_validated": case_state.timeline_validated,
        "preview_current_id": case_state.preview_current_id,
        "last_viewed_preview_id": case_state.last_viewed_preview_id,
        "rough_cut_approved": case_state.rough_cut_approved,
        "rough_cut_approved_version": case_state.rough_cut_approved_version,
        "postprocess_plan": None
        if case_state.postprocess_plan is None
        else case_state.postprocess_plan.model_dump(mode="json"),
        "export_current_id": case_state.export_current_id,
        "selected_asset_ids": case_state.selected_asset_ids,
        "disabled_asset_ids": case_state.disabled_asset_ids,
        "scratch_memory": case_state.scratch_memory,
    }


def _encode_case_values(values: Mapping[str, Any]) -> dict[str, Any]:
    json_columns = {
        "running_jobs",
        "last_error",
        "brief",
        "content_plan",
        "audio_plan",
        "cut_plan",
        "postprocess_plan",
        "selected_asset_ids",
        "disabled_asset_ids",
        "scratch_memory",
    }
    encoded = dict(values)
    for column in json_columns:
        if column in encoded:
            encoded[column] = None if encoded[column] is None else dump_json(encoded[column])
    return encoded


def _asset_insert_values(asset_id: str, payload: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "asset_id": asset_id,
        "storage_mode": str(payload.get("storage_mode", "reference")),
        "object_hash": payload.get("object_hash"),
        "reference_path": payload.get("reference_path"),
        "kind": str(payload.get("kind", "video")),
        "source": str(payload.get("source", "upload")),
        "hash": str(payload.get("hash", asset_id)),
        "mtime": payload.get("mtime"),
        "size": int(payload.get("size", 0)),
        "probe": None if payload.get("probe") is None else dump_json(payload["probe"]),
        "proxy_object_hash": payload.get("proxy_object_hash"),
        "ingest_status": str(payload.get("ingest_status", "imported")),
        "annotation_status": str(payload.get("annotation_status", "pending")),
        "annotation_pass": str(payload.get("annotation_pass", "none")),
        "index_status": str(payload.get("index_status", "none")),
        "usable": bool(payload.get("usable", False)),
        "failure": None if payload.get("failure") is None else dump_json(payload["failure"]),
    }


def _asset_update_values_for_event(event: DomainEventBase) -> dict[str, Any]:
    payload = event.payload
    values: dict[str, Any] = {}
    if event.event == "AssetProbed":
        values["probe"] = dump_json(payload.get("probe", {}))
        values["ingest_status"] = str(payload.get("ingest_status", "probing"))
    elif event.event == "ProxyGenerated":
        values["proxy_object_hash"] = payload.get("proxy_object_hash")
        values["ingest_status"] = str(payload.get("ingest_status", "proxying"))
    elif event.event == "AnnotationCompleted":
        values["annotation_status"] = "completed"
        values["annotation_pass"] = str(payload.get("annotation_pass", "cheap"))
        values["index_status"] = str(payload.get("index_status", "ready"))
        values["usable"] = bool(payload.get("usable", True))
    elif event.event == "AnnotationFailed":
        values["annotation_status"] = "failed"
        values["failure"] = dump_json(payload.get("failure", {"message": "annotation failed"}))
        values["usable"] = False
    elif event.event == "AssetInvalidated":
        values["usable"] = False
        values["failure"] = dump_json(payload.get("failure", {"message": "asset invalidated"}))
    return values


def _timeline_document_from_event(
    event: DomainEventBase,
    case_id: str,
    version: int,
) -> dict[str, Any]:
    document = event.payload.get("document_json") or event.payload.get("timeline")
    if isinstance(document, dict):
        return document
    return _empty_timeline_document(
        case_id=case_id,
        version=version,
        parent_version=getattr(event, "parent_version", None),
        patch_id=getattr(event, "patch_id", None),
    )


def _empty_timeline_document(
    *,
    case_id: str,
    version: int,
    parent_version: int | None,
    patch_id: str | None,
) -> dict[str, Any]:
    document = {
        "timeline_id": f"{case_id}:v{version}",
        "case_id": case_id,
        "version": version,
        "fps": 30,
        "duration_frames": 1,
        "tracks": [
            {"track_id": "visual_base", "track_type": "primary_visual", "clips": []},
            {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
            {"track_id": "original_audio", "track_type": "audio", "clips": []},
            {"track_id": "voiceover", "track_type": "audio", "clips": []},
            {"track_id": "bgm", "track_type": "audio", "clips": []},
            {"track_id": "subtitles", "track_type": "text", "clips": []},
        ],
        "parent_version": parent_version,
        "created_by_patch_id": patch_id,
    }
    TimelineState.model_validate(document)
    return document


def _timeline_creation_resets_rough_cut(event: DomainEventBase) -> bool:
    changed_tracks = event.payload.get("changed_track_ids") or event.payload.get(
        "affected_track_ids"
    )
    if changed_tracks is None:
        return True
    if not isinstance(changed_tracks, Sequence) or isinstance(changed_tracks, str | bytes):
        return True
    return bool(
        {"visual_base", "original_audio", "voiceover"} & {str(track) for track in changed_tracks}
    )


def _ensure_object(context: _ReducerContext, object_hash: str) -> None:
    if _row_exists(context.connection, schema.objects, "hash", object_hash):
        return
    context.connection.execute(
        schema.objects.insert().values(
            hash=object_hash,
            rel_path=f"objects/{object_hash}",
            size=0,
            created_at=context.created_at,
        )
    )


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


def _job_status_for_event(event: DomainEventBase) -> str:
    return {
        "JobEnqueued": "pending",
        "JobProgress": "running",
        "JobSucceeded": "succeeded",
        "JobFailed": "failed",
        "JobCancelled": "cancelled",
    }[event.event]


def _row_exists(connection: Connection, table: Any, key: str, value: str) -> bool:
    row = connection.execute(select(table.c[key]).where(table.c[key] == value)).first()
    return row is not None


def _row_exists_pair(connection: Connection, table: Any, keys: Mapping[str, str]) -> bool:
    statement = select(next(iter(table.c)))
    for key, value in keys.items():
        statement = statement.where(table.c[key] == value)
    return connection.execute(statement).first() is not None


def _required_attr(event: DomainEventBase, name: str) -> str:
    value = getattr(event, name, None)
    if not isinstance(value, str) or value == "":
        raise ValueError(f"{event.event} requires {name}")
    return value


def _required_int_attr(event: DomainEventBase, name: str) -> int:
    value = getattr(event, name, None)
    if not isinstance(value, int):
        raise ValueError(f"{event.event} requires integer {name}")
    return value


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
