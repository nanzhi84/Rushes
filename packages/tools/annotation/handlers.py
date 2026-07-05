"""annotation.* tool handlers."""

from __future__ import annotations

import hashlib
from typing import Any, Literal

from sqlalchemy import select

from contracts.events import JobEnqueued
from contracts.tool_result import ToolError, ToolResult
from storage import schema
from storage.repositories._json import load_json
from tools.context import ToolExecutionContext
from tools.specs import (
    AnnotationEnqueueInput,
    AnnotationInspectInput,
    AnnotationRetryInput,
    AnnotationStatusInput,
)


def enqueue(input_model: AnnotationEnqueueInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed("annotation.enqueue", context, "missing_project", "active project required")
    asset_id = input_model.asset_id
    event = _annotation_job_event(project_id=project_id, asset_id=asset_id, pass_=input_model.pass_)
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="annotation.enqueue",
        status="running",
        observation=f"annotation queued: {event.job_id}",
        data={"project_id": project_id, "asset_id": asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


def status(input_model: AnnotationStatusInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None or context.readonly_connection is None:
        return _failed(
            "annotation.status",
            context,
            "missing_project",
            "repository access required",
        )
    rows = context.readonly_connection.execute(
        select(schema.assets, schema.project_asset_links.c.enabled)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == project_id)
        .order_by(schema.assets.c.asset_id)
    ).all()
    assets = [_asset_status_payload(dict(row._mapping)) for row in rows]
    return _succeeded(
        "annotation.status",
        context,
        "loaded annotation status",
        data={
            "project_id": project_id,
            "summary": _status_summary(assets),
            "assets": assets,
        },
    )


def retry(input_model: AnnotationRetryInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None or context.readonly_connection is None:
        return _failed("annotation.retry", context, "missing_project", "repository access required")
    asset = _asset_for_project(context, project_id, input_model.asset_id)
    if asset is None:
        return _failed("annotation.retry", context, "asset_not_found", "asset is not linked")
    if asset["annotation_status"] != "failed":
        return _failed(
            "annotation.retry",
            context,
            "annotation_not_failed",
            "only failed annotations can be retried",
        )
    pass_ = input_model.pass_ or _pass_from_asset(asset)
    event = _annotation_job_event(
        project_id=project_id,
        asset_id=input_model.asset_id,
        pass_=pass_,
        retry_key=context.tool_call_id,
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="annotation.retry",
        status="running",
        observation=f"annotation retry queued: {event.job_id}",
        data={"project_id": project_id, "asset_id": input_model.asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


def inspect(input_model: AnnotationInspectInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None or context.readonly_connection is None:
        return _failed(
            "annotation.inspect",
            context,
            "missing_project",
            "repository access required",
        )
    asset = _asset_for_project(context, project_id, input_model.asset_id)
    if asset is None:
        return _failed("annotation.inspect", context, "asset_not_found", "asset is not linked")
    annotation = _annotation_for_asset(context, input_model.asset_id)
    quality_events: list[dict[str, Any]] = []
    usable_spans: list[dict[str, int | str]] = []
    if annotation is not None:
        document = annotation["document_json"]
        if isinstance(document, dict):
            quality_events = list(document.get("quality_events", []))
            usable_spans = _usable_spans(context, annotation["annotation_id"])
    return _succeeded(
        "annotation.inspect",
        context,
        "loaded annotation inspection",
        data={
            "project_id": project_id,
            "asset_id": input_model.asset_id,
            "annotation_status": asset["annotation_status"],
            "annotation_pass": asset["annotation_pass"],
            "usable": asset["usable"],
            "failure": asset["failure"],
            "quality_events": quality_events,
            "usable_spans": usable_spans,
        },
    )


def _annotation_job_event(
    *,
    project_id: str,
    asset_id: str,
    pass_: Literal["cheap", "deep"],
    retry_key: str | None = None,
) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:annotation:{pass_}"
    if retry_key is not None:
        retry_digest = hashlib.sha256(retry_key.encode()).hexdigest()[:12]
        idempotency_key = f"{idempotency_key}:retry:{retry_digest}"
    return JobEnqueued(
        job_id=_job_id("annotation", idempotency_key),
        project_id=project_id,
        payload={
            "kind": "annotation",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id, "pass": pass_},
            "attempts": 0,
            "max_retries": 1,
        },
    )


def _asset_for_project(
    context: ToolExecutionContext,
    project_id: str,
    asset_id: str,
) -> dict[str, Any] | None:
    if context.readonly_connection is None:
        return None
    row = context.readonly_connection.execute(
        select(schema.assets)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == project_id)
        .where(schema.assets.c.asset_id == asset_id)
    ).first()
    return None if row is None else _asset_status_payload(dict(row._mapping))


def _annotation_for_asset(
    context: ToolExecutionContext,
    asset_id: str,
) -> dict[str, Any] | None:
    if context.readonly_connection is None:
        return None
    row = context.readonly_connection.execute(
        select(schema.annotations_table)
        .where(schema.annotations_table.c.asset_id == asset_id)
        .order_by(schema.annotations_table.c.updated_at.desc())
        .limit(1)
    ).first()
    if row is None:
        return None
    values = dict(row._mapping)
    values["document_json"] = load_json(values["document_json"])
    return values


def _usable_spans(
    context: ToolExecutionContext,
    annotation_id: str,
) -> list[dict[str, int | str]]:
    if context.readonly_connection is None:
        return []
    rows = context.readonly_connection.execute(
        select(schema.annotation_clip_projection)
        .where(schema.annotation_clip_projection.c.annotation_id == annotation_id)
        .where(schema.annotation_clip_projection.c.usable.is_(True))
        .order_by(schema.annotation_clip_projection.c.start_frame)
    ).all()
    return [
        {
            "clip_id": str(row._mapping["clip_id"]),
            "start_frame": int(row._mapping["start_frame"]),
            "end_frame": int(row._mapping["end_frame"]),
        }
        for row in rows
    ]


def _asset_status_payload(values: dict[str, Any]) -> dict[str, Any]:
    payload = dict(values)
    for key in ("probe", "failure"):
        raw = payload.get(key)
        if isinstance(raw, str):
            payload[key] = load_json(raw)
    return payload


def _status_summary(assets: list[dict[str, Any]]) -> dict[str, int]:
    summary = {"pending": 0, "analyzing": 0, "completed": 0, "failed": 0}
    for asset in assets:
        status_value = str(asset.get("annotation_status", "pending"))
        if status_value in summary:
            summary[status_value] += 1
    return summary


def _pass_from_asset(asset: dict[str, Any]) -> Literal["cheap", "deep"]:
    return "deep" if asset.get("annotation_pass") == "deep" else "cheap"


def _active_project_id(context: ToolExecutionContext, requested: str | None) -> str | None:
    active = None
    if context.project_state is not None:
        active = context.project_state.project_id
    elif context.case_state is not None:
        active = context.case_state.project_id
    if requested is None:
        return active
    if active is not None and requested != active:
        return None
    return requested


def _job_id(kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


def _succeeded(
    tool_name: str,
    context: ToolExecutionContext,
    observation: str,
    *,
    data: dict[str, Any],
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data=data,
        events=[],
    )


def _failed(
    tool_name: str,
    context: ToolExecutionContext,
    error_code: str,
    message: str,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="failed",
        observation=message,
        error=ToolError(error_code=error_code, message=message, retryable=False, details={}),
    )
