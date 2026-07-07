"""Timeline tool handlers."""

from __future__ import annotations

import hashlib
import json
from collections.abc import Mapping
from datetime import UTC, datetime
from typing import Any

from sqlalchemy import select

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
from storage import schema
from storage.repositories._json import load_json
from timeline import (
    AnchorConflict,
    MaterializationError,
    PatchOutcome,
    get_timeline_version,
    materialize_from_clips,
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
    ComposeInitialInput,
    TimelineInspectInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
)


def compose_initial(
    input_model: ComposeInitialInput,
    context: ToolExecutionContext,
) -> ToolResult:
    tool_name = "timeline.compose_initial"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")

    try:
        timeline = materialize_from_clips(
            context.readonly_connection,
            case_state,
            [clip.model_dump(mode="json") for clip in input_model.clips],
            voiceover_asset_id=input_model.voiceover_asset_id,
        )
    except MaterializationError as exc:
        return _failed(tool_name, context, "timeline_materialization_failed", str(exc))

    report = validate_timeline(context.readonly_connection, case_state, timeline)
    timeline = timeline.model_copy(update={"validation_report": report})
    store_timeline_version(context.readonly_connection, timeline, created_at=_created_at(context))
    changed_track_ids = ["visual_base"]
    if input_model.voiceover_asset_id is not None:
        changed_track_ids.append("voiceover")
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
            "changed_track_ids": changed_track_ids,
            "created_at": _created_at(context),
        },
    )
    validation_event = _validation_event(
        case_state,
        timeline.version,
        report.model_dump(mode="json"),
    )
    status = "succeeded" if report.valid else "failed"
    observation = (
        f"composed timeline v{timeline.version} with {len(input_model.clips)} clip(s): "
        f"{'valid' if report.valid else 'invalid'}"
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status=status,
        observation=observation,
        data={
            "case_id": case_state.case_id,
            "timeline_version": timeline.version,
            "timeline": timeline.model_dump(mode="json"),
            "validation_report": report.model_dump(mode="json"),
        },
        artifacts=[ToolArtifact(artifact_id=timeline.timeline_id, kind="timeline")],
        events=[created_event.model_dump(mode="json"), validation_event.model_dump(mode="json")],
        error=None
        if report.valid
        else ToolError(
            error_code="timeline_validation_failed",
            message="timeline composed but failed validation",
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
