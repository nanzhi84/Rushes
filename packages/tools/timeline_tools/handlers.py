"""Timeline tool handlers."""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping, Sequence
from datetime import UTC, datetime
from typing import Any

from sqlalchemy import select

from contracts.candidate import CandidatePack, CandidatePackSnapshot, CandidateSlot
from contracts.case import CaseState
from contracts.decision import Decision, DecisionOption
from contracts.events import (
    DecisionCreated,
    TimelineValidated,
    TimelineValidationFailed,
    TimelineVersionCreated,
    TimelineVersionRestored,
)
from contracts.patch import AddBgmOp, TimelinePatchRequest
from contracts.tool_result import ToolArtifact, ToolError, ToolResult
from indexing import RevalidationResult, compute_scope_snapshot, revalidate_pack
from storage import schema
from storage.repositories._json import load_json
from timeline import (
    AnchorConflict,
    MaterializationError,
    PatchOutcome,
    get_timeline_version,
    materialize_from_selection,
    render_timeline_summary,
    restore_timeline_version,
    store_timeline_version,
    update_timeline_validation_report,
    validate_timeline,
)
from timeline import (
    apply_patch as apply_timeline_patch,
)
from tools.context import ToolExecutionContext
from tools.specs import (
    TimelineInspectInput,
    TimelinePlanFromCandidatesInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
)


def plan_from_candidates(
    input_model: TimelinePlanFromCandidatesInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "timeline.plan_from_candidates"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")
    if case_state.candidate_pack_id is None:
        return _failed(tool_name, context, "candidate_pack_missing", "candidate_pack_id required")

    pack = _load_candidate_pack(context, case_state.candidate_pack_id)
    if pack is None:
        return _failed(
            tool_name,
            context,
            "candidate_pack_not_found",
            f"candidate pack not found: {case_state.candidate_pack_id}",
        )
    selected_by_slot = {
        selection.slot_id: selection.candidate_id for selection in input_model.selections
    }
    revalidated = revalidate_pack(context.readonly_connection, case_state, pack)
    selected_removed = _selected_removed(revalidated, selected_by_slot)
    if selected_removed:
        return _requires_user_for_invalid_selection(
            context,
            selected_removed=selected_removed,
            removed=revalidated,
        )
    if revalidated.scope_changed and not revalidated.removed:
        return _failed(
            tool_name,
            context,
            "candidate_pack_scope_changed",
            "candidate pack scope changed; rerun retrieval.search_candidates",
            details={"candidate_pack_id": pack.candidate_pack_id},
        )

    try:
        timeline = materialize_from_selection(
            context.readonly_connection,
            case_state,
            pack,
            [selection.model_dump(mode="json") for selection in input_model.selections],
        )
    except MaterializationError as exc:
        return _failed(tool_name, context, "timeline_materialization_failed", str(exc))

    report = validate_timeline(context.readonly_connection, case_state, timeline)
    timeline = timeline.model_copy(update={"validation_report": report})
    store_timeline_version(context.readonly_connection, timeline, created_at=_created_at(context))
    created_event = TimelineVersionCreated(
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        timeline_version=timeline.version,
        parent_version=timeline.parent_version,
        payload={
            "timeline_id": timeline.timeline_id,
            "timeline_version": timeline.version,
            "parent_version": timeline.parent_version,
            "timeline": timeline.model_dump(mode="json"),
            "validation_report": report.model_dump(mode="json"),
            "changed_track_ids": ["visual_base", "voiceover"],
            "candidate_pack_id": pack.candidate_pack_id,
            "removed_candidates": _removed_payload(revalidated),
            "created_at": _created_at(context),
        },
    )
    validation_event = _validation_event(
        case_state,
        timeline.version,
        report.model_dump(mode="json"),
    )
    status = "succeeded" if report.valid else "failed"
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status=status,
        observation=_plan_observation(timeline.version, report.valid, revalidated),
        data={
            "case_id": case_state.case_id,
            "timeline_version": timeline.version,
            "timeline": timeline.model_dump(mode="json"),
            "validation_report": report.model_dump(mode="json"),
            "removed_candidates": _removed_payload(revalidated),
        },
        artifacts=[ToolArtifact(artifact_id=timeline.timeline_id, kind="timeline")],
        events=[created_event.model_dump(mode="json"), validation_event.model_dump(mode="json")],
        error=None
        if report.valid
        else ToolError(
            error_code="timeline_validation_failed",
            message="timeline materialized but failed validation",
            details={"validation_report": report.model_dump(mode="json")},
        ),
    )


def apply_patch(
    input_model: TimelinePatchRequest,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "timeline.apply_patch"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")
    if case_state.timeline_current_version is None:
        return _failed(tool_name, context, "timeline_missing", "current timeline required")

    invalid = _validate_bgm_asset(input_model, context)
    if invalid is not None:
        return invalid

    outcome = apply_timeline_patch(
        context.readonly_connection,
        case_state,
        input_model,
        created_at=_created_at(context),
    )
    if outcome.status == "conflict" and outcome.conflict is not None:
        return _requires_user_for_anchor_conflict(
            context,
            request=input_model,
            conflict=outcome.conflict,
        )
    if outcome.error is not None and outcome.timeline is None:
        code = outcome.error.code
        message = str(outcome.error)
        details = getattr(outcome.error, "details", {})
        return _failed(tool_name, context, code, message, details=dict(details))
    if (
        outcome.timeline is None
        or outcome.resolved_patch is None
        or outcome.validation_report is None
    ):
        return _failed(
            tool_name,
            context,
            "timeline_patch_failed",
            "timeline patch did not produce a timeline",
        )

    report = outcome.validation_report
    status = "succeeded" if report.valid else "failed"
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status=status,
        observation=_patch_observation(outcome.timeline.version, report.valid, outcome),
        data={
            "case_id": case_state.case_id,
            "timeline_version": outcome.timeline.version,
            "timeline": outcome.timeline.model_dump(mode="json"),
            "validation_report": report.model_dump(mode="json"),
            "resolved_patch": outcome.resolved_patch.model_dump(mode="json", by_alias=True),
            "changed_track_ids": list(outcome.changed_track_ids),
        },
        artifacts=[ToolArtifact(artifact_id=outcome.timeline.timeline_id, kind="timeline")],
        events=list(outcome.events),
        error=None
        if report.valid
        else ToolError(
            error_code="timeline_validation_failed",
            message="timeline patched but failed validation",
            details={"validation_report": report.model_dump(mode="json")},
        ),
    )


def _validate_bgm_asset(
    input_model: TimelinePatchRequest,
    context: ToolExecutionContext,
) -> ToolResult | None:
    op = input_model.op
    if not isinstance(op, AddBgmOp):
        return None
    assert context.readonly_connection is not None
    row = context.readonly_connection.execute(
        select(schema.assets.c.kind).where(schema.assets.c.asset_id == op.asset_id)
    ).first()
    if row is None or str(row._mapping["kind"]) != "audio":
        return _failed(
            "timeline.apply_patch",
            context,
            "asset_not_found",
            f"BGM 素材不存在或不是音频：{op.asset_id}",
            details={"asset_id": op.asset_id},
        )
    return None


def validate(
    input_model: TimelineValidateInput,
    context: ToolExecutionContext,
) -> ToolResult:
    del input_model
    tool_name = "timeline.validate"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")
    if case_state.timeline_current_version is None:
        return _failed(tool_name, context, "timeline_missing", "current timeline required")
    record = get_timeline_version(
        context.readonly_connection,
        case_state.case_id,
        case_state.timeline_current_version,
    )
    if record is None:
        return _failed(tool_name, context, "timeline_not_found", "current timeline not found")
    report = validate_timeline(context.readonly_connection, case_state, record.timeline)
    update_timeline_validation_report(
        context.readonly_connection,
        case_id=case_state.case_id,
        version=record.version,
        report=report,
    )
    event = _validation_event(case_state, record.version, report.model_dump(mode="json"))
    if report.valid:
        observation = f"timeline v{record.version} 校验通过"
    else:
        # LLM 只读 observation：失败项要点名，否则模型无从修起
        failed = [
            str(check.get("code", "unknown"))
            for check in report.checks
            if check.get("severity") == "error"
        ]
        detail = "、".join(failed[:6]) if failed else "未知原因"
        observation = f"timeline v{record.version} 校验失败：{detail}"
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data={
            "case_id": case_state.case_id,
            "timeline_version": record.version,
            "valid": report.valid,
            "validation_report": report.model_dump(mode="json"),
        },
        events=[event.model_dump(mode="json")],
    )


def inspect(
    input_model: TimelineInspectInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "timeline.inspect"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")
    version = input_model.version or case_state.timeline_current_version
    if version is None:
        return _failed(tool_name, context, "timeline_missing", "current timeline required")
    record = get_timeline_version(context.readonly_connection, case_state.case_id, version)
    if record is None:
        return _failed(tool_name, context, "timeline_not_found", f"timeline v{version} not found")
    aspect_ratio = _project_aspect_ratio(context)
    summary = render_timeline_summary(record.timeline, aspect_ratio=aspect_ratio)
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=summary,
        data={
            "case_id": case_state.case_id,
            "timeline_version": version,
            "timeline_summary": summary,
        },
    )


def restore_version(
    input_model: TimelineRestoreVersionInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "timeline.restore_version"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")
    if case_state.timeline_current_version is None:
        return _failed(tool_name, context, "timeline_missing", "current timeline required")
    try:
        restored = restore_timeline_version(
            context.readonly_connection,
            case_state,
            source_version=input_model.source_version,
            created_at=_created_at(context),
        )
    except KeyError:
        return _failed(
            tool_name,
            context,
            "timeline_source_version_not_found",
            f"timeline v{input_model.source_version} not found",
        )
    event = TimelineVersionRestored(
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        timeline_version=restored.version,
        payload={
            "timeline_version": restored.version,
            "source_version": input_model.source_version,
            "parent_version": restored.parent_version,
            "timeline": restored.model_dump(mode="json"),
            "created_at": _created_at(context),
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=f"restored timeline v{input_model.source_version} as v{restored.version}",
        data={
            "case_id": case_state.case_id,
            "source_version": input_model.source_version,
            "timeline_version": restored.version,
            "timeline": restored.model_dump(mode="json"),
        },
        artifacts=[ToolArtifact(artifact_id=restored.timeline_id, kind="timeline")],
        events=[event.model_dump(mode="json")],
    )


def _load_candidate_pack(
    context: ToolExecutionContext,
    candidate_pack_id: str,
) -> CandidatePack | None:
    metadata_pack = _candidate_pack_from_metadata(context.metadata, candidate_pack_id)
    if metadata_pack is not None:
        return metadata_pack
    if context.readonly_connection is None or context.case_state is None:
        return None
    event_pack = _candidate_pack_from_event_log(
        context,
        candidate_pack_id=candidate_pack_id,
        case_id=context.case_state.case_id,
    )
    if event_pack is not None:
        return event_pack
    return _candidate_pack_from_row(context, candidate_pack_id)


def _candidate_pack_from_metadata(
    metadata: Mapping[str, Any],
    candidate_pack_id: str,
) -> CandidatePack | None:
    direct = metadata.get("candidate_pack")
    parsed = _parse_candidate_pack(direct)
    if parsed is not None and parsed.candidate_pack_id == candidate_pack_id:
        return parsed
    packs = metadata.get("candidate_packs")
    if isinstance(packs, Mapping):
        parsed = _parse_candidate_pack(packs.get(candidate_pack_id))
        if parsed is not None:
            return parsed
    return None


def _candidate_pack_from_event_log(
    context: ToolExecutionContext,
    *,
    candidate_pack_id: str,
    case_id: str,
) -> CandidatePack | None:
    assert context.readonly_connection is not None
    rows = context.readonly_connection.execute(
        select(schema.event_log.c.payload_json)
        .where(schema.event_log.c.event_type == "CandidatePackCreated")
        .where(schema.event_log.c.case_id == case_id)
        .order_by(schema.event_log.c.event_id.desc())
    ).all()
    for row in rows:
        event_payload = load_json(str(row._mapping["payload_json"]))
        if not isinstance(event_payload, dict):
            continue
        payload = event_payload.get("payload")
        if not isinstance(payload, dict):
            continue
        pack = _parse_candidate_pack(payload.get("candidate_pack"))
        if pack is not None and pack.candidate_pack_id == candidate_pack_id:
            return pack
    return None


def _candidate_pack_from_row(
    context: ToolExecutionContext,
    candidate_pack_id: str,
) -> CandidatePack | None:
    assert context.readonly_connection is not None
    assert context.case_state is not None
    row = context.readonly_connection.execute(
        select(schema.candidate_packs).where(
            schema.candidate_packs.c.candidate_pack_id == candidate_pack_id
        )
    ).first()
    if row is None:
        return None
    values = row._mapping
    raw_slots = load_json(str(values["slots"]))
    if not isinstance(raw_slots, list):
        return None
    scope = compute_scope_snapshot(context.readonly_connection, context.case_state)
    return CandidatePack(
        candidate_pack_id=candidate_pack_id,
        case_id=str(values["case_id"]),
        query_context={},
        snapshot=CandidatePackSnapshot(
            generated_at=str(values["created_at"]),
            asset_scope_hash=scope.asset_scope_hash,
            annotation_versions=dict(scope.annotation_versions),
        ),
        slots=[CandidateSlot.model_validate(slot) for slot in raw_slots],
    )


def _parse_candidate_pack(value: Any) -> CandidatePack | None:
    if isinstance(value, CandidatePack):
        return value
    if isinstance(value, Mapping):
        return CandidatePack.model_validate(value)
    return None


def _selected_removed(
    revalidated: RevalidationResult,
    selected_by_slot: Mapping[str, str],
) -> list[dict[str, Any]]:
    removed: list[dict[str, Any]] = []
    for item in revalidated.removed:
        if selected_by_slot.get(item.slot_id) == item.candidate_id:
            removed.append(
                {
                    "slot_id": item.slot_id,
                    "candidate_id": item.candidate_id,
                    "asset_id": item.asset_id,
                    "clip_id": item.clip_id,
                    "reason": item.reason,
                }
            )
    return removed


def _requires_user_for_invalid_selection(
    context: ToolExecutionContext,
    *,
    selected_removed: Sequence[Mapping[str, Any]],
    removed: RevalidationResult,
) -> ToolResult:
    assert context.case_state is not None
    case_state = context.case_state
    question = "已选候选已失效：请重新检索或换一个候选。"
    options = [
        DecisionOption(
            option_id="rerun_search",
            label="重新检索",
            payload={"action": "retrieval.search_candidates"},
        ),
        DecisionOption(
            option_id="choose_another",
            label="换候选",
            payload={"action": "choose_another_candidate"},
        ),
    ]
    decision = Decision(
        decision_id=_decision_id(
            "timeline_invalid_candidate",
            case_state.case_id,
            selected_removed,
        ),
        scope_type="case",
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        type="generic",
        question=question,
        options=options,
        allow_free_text=True,
        status="pending",
        blocking=True,
        created_by_tool_call_id=context.tool_call_id,
    )
    event = DecisionCreated(
        decision_id=decision.decision_id,
        scope_type="case",
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        payload={
            "decision": decision.model_dump(mode="json"),
            "type": decision.type,
            "question": decision.question,
            "invalid_selected_candidates": list(selected_removed),
            "removed_candidates": _removed_payload(removed),
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="timeline.plan_from_candidates",
        status="requires_user",
        observation=question,
        data={
            "case_id": case_state.case_id,
            "decision": decision.model_dump(mode="json"),
            "invalid_selected_candidates": list(selected_removed),
            "removed_candidates": _removed_payload(removed),
        },
        events=[event.model_dump(mode="json")],
    )


def _requires_user_for_anchor_conflict(
    context: ToolExecutionContext,
    *,
    request: TimelinePatchRequest,
    conflict: AnchorConflict,
) -> ToolResult:
    assert context.case_state is not None
    assert context.readonly_connection is not None
    case_state = context.case_state
    version = case_state.timeline_current_version
    current_summary = ""
    if version is not None:
        record = get_timeline_version(context.readonly_connection, case_state.case_id, version)
        if record is not None:
            current_summary = render_timeline_summary(
                record.timeline,
                aspect_ratio=_project_aspect_ratio(context),
            )
    question = "时间锚点对应的片段已被后续版本修改。请确认要改当前时间线里的哪一段。"
    decision = Decision(
        decision_id=_decision_id(
            "timeline_anchor_conflict",
            case_state.case_id,
            {
                "request": request.model_dump(mode="json", by_alias=True),
                "code": conflict.code,
                "details": conflict.details,
                "current_version": version,
            },
        ),
        scope_type="case",
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        type="generic",
        question=question,
        options=[
            DecisionOption(
                option_id="use_current_time",
                label="按当前时间确认",
                payload={"action": "retry_patch_with_current_anchor"},
            ),
            DecisionOption(
                option_id="inspect_timeline",
                label="先查看时间线",
                payload={"action": "timeline.inspect", "version": version},
            ),
        ],
        allow_free_text=True,
        status="pending",
        blocking=True,
        created_by_tool_call_id=context.tool_call_id,
    )
    event = DecisionCreated(
        decision_id=decision.decision_id,
        scope_type="case",
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        payload={
            "decision": decision.model_dump(mode="json"),
            "type": decision.type,
            "question": question,
            "anchor_conflict": {"code": conflict.code, "details": conflict.details},
            "timeline_summary": current_summary,
            "request": request.model_dump(mode="json", by_alias=True),
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="timeline.apply_patch",
        status="requires_user",
        observation=question,
        data={
            "case_id": case_state.case_id,
            "decision": decision.model_dump(mode="json"),
            "anchor_conflict": {"code": conflict.code, "details": conflict.details},
            "timeline_summary": current_summary,
        },
        events=[event.model_dump(mode="json")],
    )


def _validation_event(
    case_state: CaseState,
    version: int,
    report: dict[str, Any],
) -> TimelineValidated | TimelineValidationFailed:
    payload = {"timeline_version": version, "validation_report": report}
    if report.get("valid") is True:
        return TimelineValidated(
            project_id=case_state.project_id,
            case_id=case_state.case_id,
            timeline_version=version,
            payload=payload,
        )
    return TimelineValidationFailed(
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        timeline_version=version,
        payload=payload,
    )


def _removed_payload(revalidated: RevalidationResult) -> list[dict[str, Any]]:
    return [
        {
            "slot_id": item.slot_id,
            "candidate_id": item.candidate_id,
            "asset_id": item.asset_id,
            "clip_id": item.clip_id,
            "reason": item.reason,
        }
        for item in revalidated.removed
    ]


def _plan_observation(
    version: int,
    valid: bool,
    revalidated: RevalidationResult,
) -> str:
    removed = _removed_payload(revalidated)
    suffix = "" if not removed else f"; removed {len(removed)} stale candidate(s)"
    validity = "valid" if valid else "invalid"
    return f"created timeline v{version}: {validity}{suffix}"


def _patch_observation(
    version: int,
    valid: bool,
    outcome: PatchOutcome,
) -> str:
    changed = outcome.changed_track_ids
    suffix = "" if not changed else f"; changed tracks: {', '.join(changed)}"
    validity = "valid" if valid else "invalid"
    return f"patched timeline v{version}: {validity}{suffix}"


def _project_aspect_ratio(context: ToolExecutionContext) -> str:
    if context.project_state is not None:
        return context.project_state.defaults.aspect_ratio
    if context.readonly_connection is None or context.case_state is None:
        return "unknown"
    row = context.readonly_connection.execute(
        select(schema.projects.c.defaults).where(
            schema.projects.c.project_id == context.case_state.project_id
        )
    ).first()
    if row is None:
        return "unknown"
    defaults = load_json(str(row._mapping["defaults"]))
    if isinstance(defaults, Mapping):
        aspect_ratio = defaults.get("aspect_ratio")
        if isinstance(aspect_ratio, str):
            return aspect_ratio
    return "unknown"


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


def _decision_id(prefix: str, case_id: str, payload: Any) -> str:
    raw = json.dumps({"case_id": case_id, "payload": payload}, sort_keys=True)
    digest = hashlib.sha256(raw.encode("utf-8")).hexdigest()[:16]
    return f"dec_{prefix}_{digest}"


def _created_at(context: ToolExecutionContext) -> str:
    return context.created_at or datetime.now(UTC).isoformat()
