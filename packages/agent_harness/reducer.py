"""Reducer single write path from PRD §4.5."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, cast

from sqlalchemy import delete, select, update
from sqlalchemy.engine import Connection, Engine

from contracts.decision import Decision, DecisionAnswer
from contracts.draft import DraftState, RunningJobRef
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
from storage.repositories import DecisionsRepository, DraftsRepository, EventLogRepository
from storage.repositories._json import dump_json, encode_json_columns, load_json
from storage.repositories.drafts import JSON_COLUMNS as _DRAFT_JSON_COLUMNS

from .state_validator import TimelineInvariantHook, ValidationFailed, validate_before_commit

REDUCER_DISPATCH_EVENTS: frozenset[str] = frozenset(
    {
        "DraftCreated",
        "DraftRenamed",
        "DraftCopied",
        "DraftTrashed",
        "AssetImported",
        "AssetProbed",
        "ProxyGenerated",
        "AssetInvalidated",
        "AssetIndexReady",
        "AssetIndexFailed",
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingCompleted",
        "MaterialUnderstandingFailed",
        "AssetLinked",
        "AssetUnlinked",
        "DecisionCreated",
        "DecisionAnswered",
        "DecisionCancelled",
        "BriefUpdated",
        "ContentPlanUpdated",
        "AudioPlanUpdated",
        "CutPlanUpdated",
        "PostprocessPlanUpdated",
        "TimelineVersionCreated",
        "TimelineVersionRestored",
        "TimelineValidated",
        "TimelineValidationFailed",
        "PreviewRendered",
        "PreviewViewed",
        "ExportCompleted",
        "MemoryCandidateExtracted",
        "MemoryCandidateDiscarded",
        "MemorySaved",
        "JobEnqueued",
        "JobProgress",
        "JobSucceeded",
        "JobFailed",
        "JobCancelled",
        "PolicyRefusal",
        "ProviderCallRecorded",
        "ContextCompacted",
        "TurnEnded",
        "CapabilityDegraded",
        "SecurityRefusal",
    }
)


@dataclass(frozen=True, slots=True)
class VersionConflict:
    draft_id: str
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
    draft_state_versions: Mapping[str, int] = field(default_factory=dict)
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
        self.draft_states: dict[str, DraftState] = {}
        self.original_draft_versions: dict[str, int] = {}
        self.touched_draft_ids: set[str] = set()
        self.events_to_log: list[DomainEventBase] = []
        self.followups: list[HarnessFollowup] = []
        self.skipped_events = 0

    def load_draft(self, draft_id: str) -> DraftState:
        existing = self.draft_states.get(draft_id)
        if existing is not None:
            return existing
        row = DraftsRepository(self.connection).get(draft_id)
        if row is None:
            raise ValueError(f"draft not found: {draft_id}")
        # drafts 行比 DraftState 多 created_at/updated_at 两列，DraftState extra="forbid"
        # 会拒——load 时先剔这两列再 validate。
        state = DraftState.model_validate(_strip_timestamps(row))
        self.draft_states[draft_id] = state
        self.original_draft_versions[draft_id] = state.state_version
        return state

    def set_draft_state(self, draft_state: DraftState, *, touch: bool = True) -> None:
        self.draft_states[draft_state.draft_id] = draft_state
        if touch:
            self.touched_draft_ids.add(draft_state.draft_id)
            self.original_draft_versions.setdefault(draft_state.draft_id, draft_state.state_version)

    def patch_draft_state(self, draft_id: str, patch: Mapping[str, Any]) -> DraftState:
        state = self.load_draft(draft_id)
        data = state.model_dump(mode="json")
        data.update(dict(patch))
        updated = DraftState.model_validate(data)
        self.set_draft_state(updated)
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
                draft_states=context.draft_states,
                timeline_invariant_hook=timeline_invariant_hook,
            )
            if validation is not None:
                raise _AbortReducer(
                    ReducerApplyResult(status="validation_failed", validation_failed=validation)
                )

            _persist_touched_draft_states(context)
            applied_events = _append_events(context)
            return ReducerApplyResult(
                status="applied",
                applied_events=tuple(applied_events),
                followups=tuple(context.followups),
                draft_state_versions={
                    draft_id: state.state_version
                    for draft_id, state in context.draft_states.items()
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
        draft_id = _event_draft_id(event)
        if draft_id is None:
            raise ValueError(f"strict event requires draft_id: {event.event}")
        draft_state = context.load_draft(draft_id)
        expected = event.base_version if event.base_version is not None else context.base_version
        if expected != draft_state.state_version:
            raise _AbortReducer(
                ReducerApplyResult(
                    status="version_conflict",
                    conflict=VersionConflict(
                        draft_id=draft_id,
                        expected_base_version=expected,
                        actual_state_version=draft_state.state_version,
                        event_type=event.event,
                    ),
                )
            )


def _apply_event(context: _ReducerContext, event: DomainEventBase) -> None:
    name = event.event
    event_log_index = len(context.events_to_log)
    if name == "DraftCreated":
        _apply_draft_created(context, event)
    elif name in {"DraftRenamed", "DraftCopied", "DraftTrashed"}:
        _apply_draft_update(context, event)
    elif name in {
        "AssetImported",
        "AssetProbed",
        "ProxyGenerated",
        "AssetInvalidated",
        "AssetIndexReady",
        "AssetIndexFailed",
        "MaterialUnderstandingStarted",
        "MaterialUnderstandingCompleted",
        "MaterialUnderstandingFailed",
    }:
        _apply_asset_event(context, event)
    elif name == "AssetLinked":
        _apply_asset_linked(context, event)
    elif name == "AssetUnlinked":
        _apply_asset_unlinked(context, event)
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


def _apply_draft_created(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    if _row_exists(context.connection, schema.drafts, "draft_id", draft_id):
        context.load_draft(draft_id)
        return
    draft_state = DraftState.model_validate(
        {
            "draft_id": draft_id,
            "name": event.payload.get("name", "Untitled Draft"),
            "state_version": int(event.payload.get("state_version", 0)),
            "status": event.payload.get("status", "active"),
            # DraftCreated 时 defaults 从 workspace defaults 拷贝写入（由 REST 层带入 payload）。
            "defaults": event.payload.get("defaults", {}),
            "pending_decision_id": None,
            "running_jobs": [],
            "last_error": None,
            "brief": event.payload.get("brief", {"goal": ""}),
            "content_plan": None,
            "audio_plan": None,
            "cut_plan": None,
            "timeline_current_version": None,
            "timeline_validated": False,
            "preview_current_id": None,
            "last_viewed_preview_id": None,
            "rough_cut_approved": False,
            "rough_cut_approved_version": None,
            "postprocess_plan": None,
            "export_current_id": None,
            "scratch_memory": {},
            "messages_tail_ref": None,
        }
    )
    created_at = str(event.payload.get("created_at", context.created_at))
    updated_at = str(event.payload.get("updated_at", context.created_at))
    DraftsRepository(context.connection).insert(
        _draft_row_values(draft_state, created_at=created_at, updated_at=updated_at)
    )
    context.draft_states[draft_id] = draft_state
    context.original_draft_versions[draft_id] = draft_state.state_version


def _apply_draft_update(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    if event.event == "DraftCopied":
        _apply_draft_copied(context, event)
        return
    state = context.load_draft(draft_id)
    patch: dict[str, Any] = {}
    if event.event == "DraftRenamed":
        patch["name"] = str(getattr(event, "name", None) or event.payload.get("name") or state.name)
    if event.event == "DraftTrashed":
        patch["status"] = "trashed"
    if patch:
        context.patch_draft_state(draft_id, patch)


def _apply_draft_copied(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    if _row_exists(context.connection, schema.drafts, "draft_id", draft_id):
        context.load_draft(draft_id)
        return
    source_draft_id = getattr(event, "source_draft_id", None) or event.payload.get(
        "source_draft_id"
    )
    if not isinstance(source_draft_id, str):
        raise ValueError("DraftCopied requires source_draft_id")
    source = context.load_draft(source_draft_id)
    copied_data = source.model_dump(mode="json")
    copied_data.update(
        {
            "draft_id": draft_id,
            "name": str(event.payload.get("name") or f"{source.name} Copy"),
            "state_version": 0,
            "status": str(event.payload.get("status", source.status)),
            "pending_decision_id": None,
            "running_jobs": [],
            "last_error": None,
        }
    )
    copied = DraftState.model_validate(copied_data)
    created_at = str(event.payload.get("created_at", context.created_at))
    # 先建 drafts 行再复制子表（timeline_versions/previews/exports 的外键指向 drafts），
    # 子表复制会重映射引用 id，最后回写 drafts 行。
    DraftsRepository(context.connection).insert(
        _draft_row_values(copied, created_at=created_at, updated_at=created_at)
    )
    _copy_draft_asset_links(context, source_draft_id, draft_id)
    copied = _copy_draft_owned_rows(context, source, copied)
    context.connection.execute(
        schema.drafts.update()
        .where(schema.drafts.c.draft_id == draft_id)
        .values(**encode_json_columns(_draft_update_values(copied), _DRAFT_JSON_COLUMNS))
    )
    context.draft_states[draft_id] = copied
    context.original_draft_versions[draft_id] = copied.state_version


def _copy_draft_asset_links(
    context: _ReducerContext,
    source_draft_id: str,
    target_draft_id: str,
) -> None:
    rows = context.connection.execute(
        select(schema.draft_asset_links).where(
            schema.draft_asset_links.c.draft_id == source_draft_id
        )
    ).all()
    for row in rows:
        values = dict(row._mapping)
        asset_id = str(values["asset_id"])
        if _row_exists_pair(
            context.connection,
            schema.draft_asset_links,
            {"draft_id": target_draft_id, "asset_id": asset_id},
        ):
            continue
        context.connection.execute(
            schema.draft_asset_links.insert().values(
                draft_id=target_draft_id,
                asset_id=asset_id,
                linked_at=context.created_at,
                note=str(values["note"]),
                rel_dir=values.get("rel_dir"),
            )
        )


def _copy_draft_owned_rows(
    context: _ReducerContext,
    source: DraftState,
    copied: DraftState,
) -> DraftState:
    data = copied.model_dump(mode="json")
    _copy_draft_timeline_rows(context, source.draft_id, copied.draft_id)
    data["preview_current_id"] = _copy_preview_ref(
        context,
        source.preview_current_id,
        copied.draft_id,
    )
    data["last_viewed_preview_id"] = _copy_preview_ref(
        context,
        source.last_viewed_preview_id,
        copied.draft_id,
    )
    data["export_current_id"] = _copy_export_ref(
        context,
        source.export_current_id,
        copied.draft_id,
    )
    return DraftState.model_validate(data)


def _copy_draft_timeline_rows(
    context: _ReducerContext,
    source_draft_id: str,
    target_draft_id: str,
) -> None:
    rows = context.connection.execute(
        select(schema.timeline_versions)
        .where(schema.timeline_versions.c.draft_id == source_draft_id)
        .order_by(schema.timeline_versions.c.version)
    ).all()
    for row in rows:
        values = dict(row._mapping)
        version = int(values["version"])
        timeline_id = f"{target_draft_id}:v{version}"
        if _row_exists(context.connection, schema.timeline_versions, "timeline_id", timeline_id):
            continue
        context.connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id=timeline_id,
                draft_id=target_draft_id,
                version=version,
                parent_version=values["parent_version"],
                created_by_patch_id=values["created_by_patch_id"],
                document_json=dump_json(
                    _remap_timeline_document(
                        values["document_json"],
                        draft_id=target_draft_id,
                        timeline_id=timeline_id,
                    )
                ),
                validation_report=values["validation_report"],
                created_at=context.created_at,
            )
        )


def _copy_preview_ref(
    context: _ReducerContext,
    source_preview_id: str | None,
    target_draft_id: str,
) -> str | None:
    if source_preview_id is None:
        return None
    source_row = context.connection.execute(
        select(schema.previews).where(schema.previews.c.preview_id == source_preview_id)
    ).first()
    if source_row is None:
        return None
    target_preview_id = f"{target_draft_id}:{source_preview_id}"
    if not _row_exists(context.connection, schema.previews, "preview_id", target_preview_id):
        values = dict(source_row._mapping)
        context.connection.execute(
            schema.previews.insert().values(
                preview_id=target_preview_id,
                draft_id=target_draft_id,
                timeline_version=values["timeline_version"],
                object_hash=values["object_hash"],
                quality=values["quality"],
                created_at=context.created_at,
            )
        )
    return target_preview_id


def _copy_export_ref(
    context: _ReducerContext,
    source_export_id: str | None,
    target_draft_id: str,
) -> str | None:
    if source_export_id is None:
        return None
    source_row = context.connection.execute(
        select(schema.exports).where(schema.exports.c.export_id == source_export_id)
    ).first()
    if source_row is None:
        return None
    target_export_id = f"{target_draft_id}:{source_export_id}"
    if not _row_exists(context.connection, schema.exports, "export_id", target_export_id):
        values = dict(source_row._mapping)
        context.connection.execute(
            schema.exports.insert().values(
                export_id=target_export_id,
                draft_id=target_draft_id,
                timeline_version=values["timeline_version"],
                object_hash=values["object_hash"],
                quality=values["quality"],
                created_at=context.created_at,
            )
        )
    return target_export_id


def _remap_timeline_document(
    raw_document: Any,
    *,
    draft_id: str,
    timeline_id: str,
) -> dict[str, Any]:
    document = load_json(raw_document) if isinstance(raw_document, str) else raw_document
    if not isinstance(document, Mapping):
        raise ValueError("timeline document must be an object")
    copied = dict(document)
    copied["draft_id"] = draft_id
    copied["timeline_id"] = timeline_id
    return copied


def _apply_asset_event(context: _ReducerContext, event: DomainEventBase) -> None:
    asset_id = _required_attr(event, "asset_id")
    payload = event.payload
    object_hash = payload.get("object_hash")
    proxy_object_hash = payload.get("proxy_object_hash")
    thumbnail_object_hash = payload.get("thumbnail_object_hash")
    if isinstance(object_hash, str):
        _ensure_object(context, object_hash, size=_optional_int(payload.get("object_size")))
    if isinstance(proxy_object_hash, str):
        _ensure_object(
            context,
            proxy_object_hash,
            size=_optional_int(payload.get("proxy_object_size")),
        )
    if isinstance(thumbnail_object_hash, str):
        _ensure_object(
            context,
            thumbnail_object_hash,
            size=_optional_int(payload.get("thumbnail_object_size")),
        )
    if not _row_exists(context.connection, schema.assets, "asset_id", asset_id):
        values = _asset_insert_values(asset_id, payload)
        context.connection.execute(schema.assets.insert().values(**values))
    updates = _asset_update_values_for_event(event)
    if updates:
        context.connection.execute(
            update(schema.assets).where(schema.assets.c.asset_id == asset_id).values(**updates)
        )


def _apply_asset_linked(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    asset_id = _required_attr(event, "asset_id")
    if not _row_exists_pair(
        context.connection,
        schema.draft_asset_links,
        {"draft_id": draft_id, "asset_id": asset_id},
    ):
        # 单级草稿模型下 draft_asset_links 无 enabled 列：链接存在即 usable，不用即删。
        context.connection.execute(
            schema.draft_asset_links.insert().values(
                draft_id=draft_id,
                asset_id=asset_id,
                linked_at=str(event.payload.get("linked_at", context.created_at)),
                note=str(event.payload.get("note", "")),
                rel_dir=_optional_str(event.payload.get("rel_dir")),
            )
        )
    else:
        updates: dict[str, Any] = {}
        if "rel_dir" in event.payload:
            updates["rel_dir"] = _optional_str(event.payload.get("rel_dir"))
        if "note" in event.payload:
            updates["note"] = str(event.payload.get("note", ""))
        if updates:
            context.connection.execute(
                update(schema.draft_asset_links)
                .where(schema.draft_asset_links.c.draft_id == draft_id)
                .where(schema.draft_asset_links.c.asset_id == asset_id)
                .values(**updates)
            )


def _optional_str(value: Any) -> str | None:
    return value if isinstance(value, str) and value else None


def _apply_asset_unlinked(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    asset_id = _required_attr(event, "asset_id")
    context.connection.execute(
        delete(schema.draft_asset_links)
        .where(schema.draft_asset_links.c.draft_id == draft_id)
        .where(schema.draft_asset_links.c.asset_id == asset_id)
    )


def _apply_decision_created(context: _ReducerContext, event: DecisionEventBase) -> None:
    decision = _decision_from_created_event(context, event)
    validate_decision_registered(decision)
    DecisionsRepository(context.connection).insert(_decision_insert_values(decision))
    if decision.scope_type == "draft" and decision.blocking and decision.draft_id is not None:
        context.patch_draft_state(decision.draft_id, {"pending_decision_id": decision.decision_id})


def _apply_decision_answered(context: _ReducerContext, event: DecisionEventBase) -> None:
    decision_row = DecisionsRepository(context.connection).get(event.decision_id)
    if decision_row is None:
        raise ValueError(f"decision not found: {event.decision_id}")
    decision = Decision.model_validate(decision_row)
    answer = _answer_from_event(event)
    pending_status = pending_tool_call_status_after_answer(
        decision.pending_tool_call,
        answer,
        decision=decision,
    )
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
    if decision.scope_type != "draft" or decision.draft_id is None:
        return
    state = context.load_draft(decision.draft_id)
    effect = reduce_decision_answer(state, decision, answer)
    if effect.state_patch:
        state = context.patch_draft_state(decision.draft_id, effect.state_patch)
    if state.pending_decision_id == decision.decision_id:
        context.patch_draft_state(decision.draft_id, {"pending_decision_id": None})
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
    if decision.scope_type == "draft" and decision.draft_id is not None:
        state = context.load_draft(decision.draft_id)
        if state.pending_decision_id == decision.decision_id:
            context.patch_draft_state(decision.draft_id, {"pending_decision_id": None})


def _apply_plan_updated(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
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
    context.patch_draft_state(draft_id, {key: event.payload[key]})


def _apply_timeline_version_created(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError("TimelineVersionCreated requires timeline_version")
    document = _timeline_document_from_event(event, draft_id, version)
    if not _timeline_exists(context.connection, draft_id, version):
        context.connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id=str(event.payload.get("timeline_id", f"{draft_id}:v{version}")),
                draft_id=draft_id,
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
    context.patch_draft_state(draft_id, patch)


def _apply_timeline_version_restored(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError("TimelineVersionRestored requires timeline_version")
    state = context.load_draft(draft_id)
    source_version = event.payload.get("source_version")
    restores_approved_cut = version == state.rough_cut_approved_version or (
        isinstance(source_version, int) and source_version == state.rough_cut_approved_version
    )
    patch: dict[str, Any] = {
        "timeline_current_version": version,
        "timeline_validated": False,
        "rough_cut_approved": restores_approved_cut,
    }
    if restores_approved_cut:
        patch["rough_cut_approved_version"] = version
    context.patch_draft_state(
        draft_id,
        patch,
    )


def _apply_timeline_validation_event(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    version = getattr(event, "timeline_version", None) or event.payload.get("timeline_version")
    if not isinstance(version, int):
        raise ValueError(f"{event.event} requires timeline_version")
    valid = event.event == "TimelineValidated"
    report = event.payload.get("validation_report", {"valid": valid, "checks": []})
    context.connection.execute(
        update(schema.timeline_versions)
        .where(schema.timeline_versions.c.draft_id == draft_id)
        .where(schema.timeline_versions.c.version == version)
        .values(validation_report=dump_json(report))
    )
    state = context.load_draft(draft_id)
    if state.timeline_current_version == version:
        context.patch_draft_state(draft_id, {"timeline_validated": valid})


def _apply_preview_rendered(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    preview_id = _required_attr(event, "artifact_id")
    timeline_version = _required_int_attr(event, "timeline_version")
    object_hash = str(event.payload.get("object_hash", preview_id))
    _ensure_object(context, object_hash)
    if not _row_exists(context.connection, schema.previews, "preview_id", preview_id):
        context.connection.execute(
            schema.previews.insert().values(
                preview_id=preview_id,
                draft_id=draft_id,
                timeline_version=timeline_version,
                object_hash=object_hash,
                quality=dump_json(event.payload.get("quality", {})),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    state = context.load_draft(draft_id)
    if state.timeline_current_version == timeline_version:
        context.patch_draft_state(draft_id, {"preview_current_id": preview_id})


def _apply_preview_viewed(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    preview_id = _required_attr(event, "preview_id")
    if _preview_belongs_to_draft(context.connection, preview_id, draft_id):
        context.patch_draft_state(draft_id, {"last_viewed_preview_id": preview_id})


def _apply_export_completed(context: _ReducerContext, event: DomainEventBase) -> None:
    draft_id = _required_attr(event, "draft_id")
    export_id = _required_attr(event, "artifact_id")
    timeline_version = _required_int_attr(event, "timeline_version")
    object_hash = str(event.payload.get("object_hash", export_id))
    _ensure_object(context, object_hash)
    if not _row_exists(context.connection, schema.exports, "export_id", export_id):
        context.connection.execute(
            schema.exports.insert().values(
                export_id=export_id,
                draft_id=draft_id,
                timeline_version=timeline_version,
                object_hash=object_hash,
                quality=dump_json(event.payload.get("quality", {})),
                created_at=str(event.payload.get("created_at", context.created_at)),
            )
        )
    state = context.load_draft(draft_id)
    if state.timeline_current_version == timeline_version:
        context.patch_draft_state(draft_id, {"export_current_id": export_id})


def _apply_memory_candidate_extracted(context: _ReducerContext, event: DomainEventBase) -> None:
    candidate_id = _required_attr(event, "candidate_id")
    if _row_exists(context.connection, schema.memory_candidates, "candidate_id", candidate_id):
        return
    draft_id = getattr(event, "draft_id", None) or str(event.payload.get("draft_id", ""))
    context.connection.execute(
        schema.memory_candidates.insert().values(
            candidate_id=candidate_id,
            draft_id=draft_id,
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
        # 单级草稿模型下记忆只有 user 一域（无 project 域列）。
        context.connection.execute(
            schema.memories.insert().values(
                memory_id=memory_id,
                scope=str(event.payload.get("scope", "user")),
                content=str(event.payload.get("content", "")),
                tags=dump_json(event.payload.get("tags", [])),
                created_from_draft_id=event.payload.get("created_from_draft_id"),
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
    requested_by_draft_id = getattr(event, "requested_by_draft_id", None)
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
    if event.event == "JobFailed":
        _mark_asset_job_failed(context, event)
    running_jobs_draft_id = (
        requested_by_draft_id if isinstance(requested_by_draft_id, str) else None
    )
    if running_jobs_draft_id is None:
        event_draft_id = getattr(event, "draft_id", None)
        running_jobs_draft_id = event_draft_id if isinstance(event_draft_id, str) else None
    if running_jobs_draft_id is not None:
        _update_running_jobs(context, running_jobs_draft_id, event, status)


def _insert_job(context: _ReducerContext, event: DomainEventBase) -> None:
    job_id = _required_attr(event, "job_id")
    kind = str(event.payload.get("kind", "unknown"))
    # import_url 这类 job 的 asset 在下载完成建档前不存在：
    # jobs.asset_id 有外键，行不存在时置 None（asset_id 仍在 job_payload 里）。
    raw_asset_id = event.payload.get("asset_id")
    asset_id = (
        raw_asset_id
        if isinstance(raw_asset_id, str)
        and _row_exists(context.connection, schema.assets, "asset_id", raw_asset_id)
        else None
    )
    context.connection.execute(
        schema.jobs.insert().values(
            job_id=job_id,
            kind=kind,
            status=_job_status_for_event(event),
            draft_id=event.draft_id,
            requested_by_draft_id=getattr(event, "requested_by_draft_id", None),
            asset_id=asset_id,
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
    draft_id: str,
    event: DomainEventBase,
    status: str,
) -> None:
    state = context.load_draft(draft_id)
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
    context.patch_draft_state(draft_id, {"running_jobs": updated})


def _persist_touched_draft_states(context: _ReducerContext) -> None:
    repository = DraftsRepository(context.connection)
    for draft_id in sorted(context.touched_draft_ids):
        state = context.draft_states[draft_id]
        expected = context.original_draft_versions[draft_id]
        values = _draft_update_values(state)
        values["updated_at"] = context.created_at
        conflict = repository.update_with_state_version(
            draft_id,
            expected,
            values,
        )
        if conflict is not None:
            raise _AbortReducer(
                ReducerApplyResult(
                    status="version_conflict",
                    conflict=VersionConflict(
                        draft_id=draft_id,
                        expected_base_version=expected,
                        actual_state_version=state.state_version,
                        event_type="DraftStateUpdate",
                    ),
                )
            )
        context.draft_states[draft_id] = state.model_copy(update={"state_version": expected + 1})


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
    draft_id = _event_draft_id(event)
    if draft_id is None:
        requested_draft_id = getattr(event, "requested_by_draft_id", None)
        draft_id = requested_draft_id if isinstance(requested_draft_id, str) else None
    if draft_id is None:
        return None
    state = context.draft_states.get(draft_id)
    return None if state is None else state.state_version


def _event_version_mode(event: DomainEventBase) -> VersionMode:
    if isinstance(event, DecisionEventBase):
        return type(event).reducer_version_mode(event.scope_type)
    return type(event).version_mode


def _event_draft_id(event: DomainEventBase) -> str | None:
    draft_id = getattr(event, "draft_id", None)
    return draft_id if isinstance(draft_id, str) else None


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
    del context
    decision_data = dict(event.payload.get("decision", {}))
    decision_data.setdefault("decision_id", event.decision_id)
    decision_data.setdefault("scope_type", event.scope_type)
    decision_data.setdefault("draft_id", event.draft_id)
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
        bool(event.payload.get("blocking", event.scope_type == "draft")),
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


def _draft_row_values(
    draft_state: DraftState,
    *,
    created_at: str,
    updated_at: str,
) -> dict[str, Any]:
    values = _draft_update_values(draft_state)
    values["draft_id"] = draft_state.draft_id
    values["state_version"] = draft_state.state_version
    values["created_at"] = created_at
    values["updated_at"] = updated_at
    return values


def _draft_update_values(draft_state: DraftState) -> dict[str, Any]:
    return {
        "name": draft_state.name,
        "status": draft_state.status,
        "defaults": draft_state.defaults.model_dump(mode="json"),
        "pending_decision_id": draft_state.pending_decision_id,
        "running_jobs": [job.model_dump(mode="json") for job in draft_state.running_jobs],
        "last_error": None
        if draft_state.last_error is None
        else draft_state.last_error.model_dump(mode="json"),
        "brief": draft_state.brief.model_dump(mode="json"),
        "content_plan": draft_state.content_plan,
        "audio_plan": None
        if draft_state.audio_plan is None
        else draft_state.audio_plan.model_dump(mode="json"),
        "cut_plan": None
        if draft_state.cut_plan is None
        else draft_state.cut_plan.model_dump(mode="json", by_alias=True),
        "timeline_current_version": draft_state.timeline_current_version,
        "timeline_validated": draft_state.timeline_validated,
        "preview_current_id": draft_state.preview_current_id,
        "last_viewed_preview_id": draft_state.last_viewed_preview_id,
        "rough_cut_approved": draft_state.rough_cut_approved,
        "rough_cut_approved_version": draft_state.rough_cut_approved_version,
        "postprocess_plan": None
        if draft_state.postprocess_plan is None
        else draft_state.postprocess_plan.model_dump(mode="json"),
        "export_current_id": draft_state.export_current_id,
        "scratch_memory": draft_state.scratch_memory,
        "messages_tail_ref": draft_state.messages_tail_ref,
    }


def _strip_timestamps(row: Mapping[str, Any]) -> dict[str, Any]:
    data = dict(row)
    data.pop("created_at", None)
    data.pop("updated_at", None)
    return data


def _asset_insert_values(asset_id: str, payload: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "asset_id": asset_id,
        "storage_mode": str(payload.get("storage_mode", "reference")),
        "object_hash": payload.get("object_hash"),
        "reference_path": payload.get("reference_path"),
        "kind": str(payload.get("kind", "video")),
        "source": str(payload.get("source", "upload")),
        "filename": str(payload.get("filename", "")),
        "hash": str(payload.get("hash", asset_id)),
        "mtime": payload.get("mtime"),
        "size": int(payload.get("size", 0)),
        "probe": None if payload.get("probe") is None else dump_json(payload["probe"]),
        "proxy_object_hash": payload.get("proxy_object_hash"),
        "ingest_status": str(payload.get("ingest_status", "imported")),
        "usable": bool(payload.get("usable", True)),
        "failure": None if payload.get("failure") is None else dump_json(payload["failure"]),
    }


def _asset_update_values_for_event(event: DomainEventBase) -> dict[str, Any]:
    payload = event.payload
    values: dict[str, Any] = {}
    if event.event == "AssetImported":
        for key in (
            "storage_mode",
            "object_hash",
            "reference_path",
            "kind",
            "source",
            "filename",
            "hash",
            "mtime",
            "size",
            "ingest_status",
            "usable",
        ):
            if key in payload:
                values[key] = payload[key]
        if "probe" in payload:
            values["probe"] = None if payload["probe"] is None else dump_json(payload["probe"])
        if "proxy_object_hash" in payload:
            values["proxy_object_hash"] = payload["proxy_object_hash"]
        if "failure" in payload:
            values["failure"] = (
                None if payload["failure"] is None else dump_json(payload["failure"])
            )
    elif event.event == "AssetProbed":
        values["probe"] = dump_json(payload.get("probe", {}))
        values["ingest_status"] = str(payload.get("ingest_status", "probing"))
    elif event.event == "ProxyGenerated":
        values["proxy_object_hash"] = payload.get("proxy_object_hash")
        values["ingest_status"] = str(payload.get("ingest_status", "proxying"))
    elif event.event == "AssetInvalidated":
        values["usable"] = False
        values["failure"] = dump_json(payload.get("failure", {"message": "asset invalidated"}))
    elif event.event == "AssetIndexReady":
        # 便宜本地索引就绪：写结构化索引 JSON、缩略图对象哈希。
        if "index_json" in payload:
            values["index_json"] = (
                None if payload["index_json"] is None else dump_json(payload["index_json"])
            )
        if "thumbnail_object_hash" in payload:
            values["thumbnail_object_hash"] = payload["thumbnail_object_hash"]
        # 只在事件显式带 ingest_status 时推进摄入状态：poster 快任务只产封面/缩略图，
        # 借本事件秒出缩略图但不该把状态跳到 indexed（proxy/scenes 还没做）；
        # 真正的 index handler 始终带 ingest_status="indexed"，行为不变。
        if "ingest_status" in payload:
            values["ingest_status"] = str(payload["ingest_status"])
    elif event.event == "AssetIndexFailed":
        # 索引失败不阻塞任何流程，仅记录失败信息（Spec C §C1）。
        values["failure"] = dump_json(payload.get("failure", {"message": "index failed"}))
    elif event.event == "MaterialUnderstandingStarted":
        values["understanding_status"] = "running"
    elif event.event == "MaterialUnderstandingCompleted":
        values["understanding_status"] = "ready"
    elif event.event == "MaterialUnderstandingFailed":
        values["understanding_status"] = "failed"
    return values


def _mark_asset_job_failed(context: _ReducerContext, event: DomainEventBase) -> None:
    asset_id = event.payload.get("asset_id")
    if not isinstance(asset_id, str):
        job_row = context.connection.execute(
            select(schema.jobs.c.asset_id).where(
                schema.jobs.c.job_id == _required_attr(event, "job_id")
            )
        ).first()
        if job_row is not None:
            candidate = job_row._mapping["asset_id"]
            asset_id = candidate if isinstance(candidate, str) else None
    if not isinstance(asset_id, str):
        return
    error_payload = event.payload.get("error")
    failure = (
        error_payload
        if isinstance(error_payload, Mapping)
        else {
            "error_code": "job_failed",
            "message": "asset job failed",
            "retryable": True,
        }
    )
    if not _row_exists(context.connection, schema.assets, "asset_id", asset_id):
        return
    context.connection.execute(
        update(schema.assets)
        .where(schema.assets.c.asset_id == asset_id)
        .values(
            ingest_status="failed",
            usable=False,
            failure=dump_json(failure),
        )
    )


def _timeline_document_from_event(
    event: DomainEventBase,
    draft_id: str,
    version: int,
) -> dict[str, Any]:
    document = event.payload.get("document_json") or event.payload.get("timeline")
    if isinstance(document, dict):
        return document
    return _empty_timeline_document(
        draft_id=draft_id,
        version=version,
        parent_version=getattr(event, "parent_version", None),
        patch_id=getattr(event, "patch_id", None),
    )


def _empty_timeline_document(
    *,
    draft_id: str,
    version: int,
    parent_version: int | None,
    patch_id: str | None,
) -> dict[str, Any]:
    document = {
        "timeline_id": f"{draft_id}:v{version}",
        "draft_id": draft_id,
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


def _ensure_object(context: _ReducerContext, object_hash: str, *, size: int | None = None) -> None:
    if _row_exists(context.connection, schema.objects, "hash", object_hash):
        return
    rel_path = (
        f"{object_hash[:2]}/{object_hash[2:4]}/{object_hash}"
        if len(object_hash) == 64
        else f"objects/{object_hash}"
    )
    context.connection.execute(
        schema.objects.insert().values(
            hash=object_hash,
            rel_path=rel_path,
            size=size or 0,
            created_at=context.created_at,
        )
    )


def _optional_int(value: Any) -> int | None:
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


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
