"""Project and Case lifecycle tool handlers."""

from __future__ import annotations

import uuid
from collections.abc import Mapping
from typing import Any

from sqlalchemy import select

from contracts.events import (
    CaseClosed,
    CaseCreated,
    CaseMoved,
    ProjectCopied,
    ProjectCreated,
    ProjectRenamed,
    ProjectTrashed,
)
from contracts.tool_result import ToolError, ToolResult
from storage import schema

from ..context import ToolExecutionContext
from ..specs import (
    ProjectCloseCaseInput,
    ProjectCopyInput,
    ProjectCreateCaseInput,
    ProjectCreateInput,
    ProjectDeleteInput,
    ProjectListTreeInput,
    ProjectMoveCaseInput,
    ProjectRenameInput,
)


def create(input_model: ProjectCreateInput, context: ToolExecutionContext) -> ToolResult:
    project_id = input_model.project_id or _new_id("project")
    event = ProjectCreated(
        project_id=project_id,
        name=input_model.name,
        payload={
            "name": input_model.name,
            "defaults": input_model.defaults,
            "status": "active",
        },
    )
    return _succeeded(
        "project.create",
        context,
        f"created project {project_id}",
        data={"project_id": project_id, "name": input_model.name},
        events=[event.model_dump(mode="json")],
    )


def rename(input_model: ProjectRenameInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "project.rename",
            context,
            "missing_project",
            "project.rename requires an active project",
        )
    event = ProjectRenamed(
        project_id=project_id,
        name=input_model.name,
        payload={"name": input_model.name},
    )
    return _succeeded(
        "project.rename",
        context,
        f"renamed project {project_id}",
        data={"project_id": project_id, "name": input_model.name},
        events=[event.model_dump(mode="json")],
    )


def delete(input_model: ProjectDeleteInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "project.delete",
            context,
            "missing_project",
            "project.delete requires an active project",
        )
    event = ProjectTrashed(project_id=project_id)
    return _succeeded(
        "project.delete",
        context,
        f"trashed project {project_id}",
        data={"project_id": project_id},
        events=[event.model_dump(mode="json")],
    )


def copy(input_model: ProjectCopyInput, context: ToolExecutionContext) -> ToolResult:
    source_project_id = _active_project_id(context, input_model.source_project_id)
    if source_project_id is None:
        return _failed(
            "project.copy",
            context,
            "missing_project",
            "project.copy requires an active source project",
        )
    project_id = input_model.project_id or _new_id("project")
    name = input_model.name or _copied_project_name(context, source_project_id)
    event = ProjectCopied(
        project_id=project_id,
        source_project_id=source_project_id,
        payload={"name": name},
    )
    return _succeeded(
        "project.copy",
        context,
        f"copied project {source_project_id} to {project_id}",
        data={
            "project_id": project_id,
            "source_project_id": source_project_id,
            "name": name,
        },
        events=[event.model_dump(mode="json")],
    )


def create_case(
    input_model: ProjectCreateCaseInput,
    context: ToolExecutionContext,
) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "project.create_case",
            context,
            "missing_project",
            "project.create_case requires an active project",
        )
    case_id = input_model.case_id or _new_id("case")
    brief = _brief_payload(input_model.brief, input_model.goal)
    event = CaseCreated(
        project_id=project_id,
        case_id=case_id,
        payload={
            "name": input_model.name,
            "brief": brief,
            "status": "active",
        },
    )
    return _succeeded(
        "project.create_case",
        context,
        f"created case {case_id}",
        data={"project_id": project_id, "case_id": case_id, "brief": brief},
        events=[event.model_dump(mode="json")],
    )


def move_case(input_model: ProjectMoveCaseInput, context: ToolExecutionContext) -> ToolResult:
    case_id = _active_case_id(context, input_model.case_id)
    if case_id is None or context.case_state is None:
        return _failed(
            "project.move_case",
            context,
            "missing_case",
            "project.move_case requires an active case",
        )
    if input_model.case_id is not None and input_model.case_id != context.case_state.case_id:
        return _failed(
            "project.move_case",
            context,
            "case_mismatch",
            "project.move_case can only move the active case",
        )
    if not _project_exists(context, input_model.target_project_id):
        return _failed(
            "project.move_case",
            context,
            "target_project_not_found",
            f"target project not found: {input_model.target_project_id}",
        )
    event = CaseMoved(
        case_id=case_id,
        project_id=input_model.target_project_id,
        source_project_id=context.case_state.project_id,
        target_project_id=input_model.target_project_id,
    )
    return _succeeded(
        "project.move_case",
        context,
        f"moved case {case_id} to {input_model.target_project_id}",
        data={
            "case_id": case_id,
            "source_project_id": context.case_state.project_id,
            "target_project_id": input_model.target_project_id,
        },
        events=[event.model_dump(mode="json")],
    )


def close_case(input_model: ProjectCloseCaseInput, context: ToolExecutionContext) -> ToolResult:
    case_id = _active_case_id(context, input_model.case_id)
    if case_id is None:
        return _failed(
            "project.close_case",
            context,
            "missing_case",
            "project.close_case requires an active case",
        )
    event = CaseClosed(case_id=case_id, project_id=_event_project_id(context))
    return _succeeded(
        "project.close_case",
        context,
        f"closed case {case_id}",
        data={"case_id": case_id},
        events=[event.model_dump(mode="json")],
    )


def list_tree(input_model: ProjectListTreeInput, context: ToolExecutionContext) -> ToolResult:
    if context.readonly_connection is None:
        return _failed(
            "project.list_tree",
            context,
            "missing_connection",
            "project.list_tree requires repository access",
        )
    projects = _project_tree(
        context.readonly_connection,
        include_trashed=input_model.include_trashed,
    )
    return _succeeded(
        "project.list_tree",
        context,
        "loaded project tree",
        data={"projects": projects},
        events=[],
    )


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


def _active_case_id(context: ToolExecutionContext, requested: str | None) -> str | None:
    active = context.case_state.case_id if context.case_state is not None else None
    if requested is None:
        return active
    if active is not None and requested != active:
        return None
    return requested


def _event_project_id(context: ToolExecutionContext) -> str | None:
    if context.case_state is not None:
        return context.case_state.project_id
    if context.project_state is not None:
        return context.project_state.project_id
    return None


def _brief_payload(brief: Mapping[str, Any], goal: str | None) -> dict[str, Any]:
    payload = dict(brief)
    if goal is not None:
        payload["goal"] = goal
    payload.setdefault("goal", "")
    return payload


def _copied_project_name(context: ToolExecutionContext, source_project_id: str) -> str:
    if context.project_state is not None and context.project_state.project_id == source_project_id:
        return f"{context.project_state.name} Copy"
    return "Copied Project"


def _project_exists(context: ToolExecutionContext, project_id: str) -> bool:
    if (
        context.project_state is not None
        and context.project_state.project_id == project_id
        and context.project_state.status == "active"
    ):
        return True
    if context.readonly_connection is None:
        return True
    row = context.readonly_connection.execute(
        select(schema.projects.c.status).where(schema.projects.c.project_id == project_id)
    ).first()
    return row is not None and row._mapping["status"] == "active"


def _project_tree(connection: Any, *, include_trashed: bool) -> list[dict[str, Any]]:
    project_rows = connection.execute(
        select(schema.projects).order_by(schema.projects.c.created_at)
    ).all()
    case_rows = connection.execute(select(schema.cases).order_by(schema.cases.c.name)).all()
    cases_by_project: dict[str, list[dict[str, Any]]] = {}
    for row in case_rows:
        values = dict(row._mapping)
        if not include_trashed and values["status"] == "trashed":
            continue
        project_id = str(values["project_id"])
        cases_by_project.setdefault(project_id, []).append(
            {
                "case_id": values["case_id"],
                "project_id": project_id,
                "name": values["name"],
                "status": values["status"],
            }
        )
    projects: list[dict[str, Any]] = []
    for row in project_rows:
        values = dict(row._mapping)
        if not include_trashed and values["status"] == "trashed":
            continue
        project_id = str(values["project_id"])
        projects.append(
            {
                "project_id": project_id,
                "name": values["name"],
                "status": values["status"],
                "cases": cases_by_project.get(project_id, []),
            }
        )
    return projects


def _succeeded(
    tool_name: str,
    context: ToolExecutionContext,
    observation: str,
    *,
    data: dict[str, Any],
    events: list[dict[str, Any]],
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data=data,
        events=events,
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
        error=ToolError(
            error_code=error_code,
            message=message,
            retryable=False,
            details={},
        ),
    )


def _new_id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"
