"""Render tool handlers."""

from __future__ import annotations

import hashlib
import json
from typing import Any

from sqlalchemy import select

from contracts.case import CaseState
from contracts.events import JobEnqueued
from contracts.tool_result import ToolError, ToolResult
from storage import schema
from storage.repositories._json import load_json
from tools.context import ToolExecutionContext
from tools.specs import RenderFinalMp4Input, RenderPreviewInput, RenderStatusInput


def preview(input_model: RenderPreviewInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    return _enqueue_render_job("render.preview", "render_preview", context)


def final_mp4(input_model: RenderFinalMp4Input, context: ToolExecutionContext) -> ToolResult:
    del input_model
    return _enqueue_render_job("render.final_mp4", "render_final", context)


def status(input_model: RenderStatusInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    tool_name = "render.status"
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if context.readonly_connection is None:
        return _failed(tool_name, context, "missing_connection", "repository access required")

    previews = _artifact_rows(
        context,
        table_name="previews",
        case_state=case_state,
        current_id=case_state.preview_current_id,
    )
    exports = _artifact_rows(
        context,
        table_name="exports",
        case_state=case_state,
        current_id=case_state.export_current_id,
    )
    jobs = _render_jobs(context, case_state)
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation="loaded render status",
        data={
            "case_id": case_state.case_id,
            "timeline_current_version": case_state.timeline_current_version,
            "preview_current_id": case_state.preview_current_id,
            "export_current_id": case_state.export_current_id,
            "previews": previews,
            "exports": exports,
            "running_jobs": jobs,
        },
    )


def _enqueue_render_job(
    tool_name: str,
    kind: str,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = context.case_state
    if case_state is None:
        return _failed(tool_name, context, "missing_case", "active case required")
    if case_state.timeline_current_version is None:
        return _failed(tool_name, context, "missing_timeline", "current timeline required")
    arguments = {"timeline_version": case_state.timeline_current_version}
    idempotency_key = (
        f"case:{case_state.case_id}:{kind}:"
        f"{hashlib.sha256(json.dumps(arguments, sort_keys=True).encode()).hexdigest()}"
    )
    event = JobEnqueued(
        job_id=_job_id(kind, idempotency_key),
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        requested_by_case_id=case_state.case_id,
        payload={
            "kind": kind,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "tool_name": tool_name,
                "arguments": arguments,
                "tool_call_id": context.tool_call_id,
                "turn_id": context.turn_id,
            },
            "tool_name": tool_name,
            "tool_call_id": context.tool_call_id,
            "attempts": 0,
            "max_retries": 2,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="running",
        observation=f"job queued: {event.job_id}",
        data={"case_id": case_state.case_id, "job_id": event.job_id, "job_kind": kind},
        events=[event.model_dump(mode="json")],
    )


def _artifact_rows(
    context: ToolExecutionContext,
    *,
    table_name: str,
    case_state: CaseState,
    current_id: str | None,
) -> list[dict[str, Any]]:
    assert context.readonly_connection is not None
    table = schema.previews if table_name == "previews" else schema.exports
    id_column = table.c.preview_id if table_name == "previews" else table.c.export_id
    rows = context.readonly_connection.execute(
        select(table)
        .where(table.c.case_id == case_state.case_id)
        .order_by(table.c.created_at.desc())
    ).all()
    result: list[dict[str, Any]] = []
    for row in rows:
        values = dict(row._mapping)
        artifact_id = str(values[id_column.name])
        quality = load_json(values["quality"]) if isinstance(values.get("quality"), str) else {}
        result.append(
            {
                "artifact_id": artifact_id,
                "timeline_version": values["timeline_version"],
                "object_hash": values["object_hash"],
                "quality": quality,
                "created_at": values["created_at"],
                "current": artifact_id == current_id,
            }
        )
    return result


def _render_jobs(context: ToolExecutionContext, case_state: CaseState) -> list[dict[str, Any]]:
    assert context.readonly_connection is not None
    rows = context.readonly_connection.execute(
        select(schema.jobs)
        .where(schema.jobs.c.case_id == case_state.case_id)
        .where(schema.jobs.c.kind.in_(("render_preview", "render_final")))
        .where(schema.jobs.c.status.in_(("pending", "running")))
        .order_by(schema.jobs.c.created_at)
    ).all()
    jobs: list[dict[str, Any]] = []
    for row in rows:
        values = dict(row._mapping)
        jobs.append(
            {
                "job_id": values["job_id"],
                "kind": values["kind"],
                "status": values["status"],
                "progress": values["progress"],
                "payload_json": _json_or_empty(values.get("payload_json")),
                "created_at": values["created_at"],
                "started_at": values["started_at"],
            }
        )
    return jobs


def _json_or_empty(value: Any) -> dict[str, Any]:
    decoded = load_json(value) if isinstance(value, str) else value
    return decoded if isinstance(decoded, dict) else {}


def _job_id(kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


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
        error=ToolError(error_code=error_code, message=message, retryable=False),
    )
