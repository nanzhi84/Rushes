"""Interaction tool handlers."""

from __future__ import annotations

import hashlib
import json
from typing import Any

from contracts.decision import Decision, DecisionOption
from contracts.events import DecisionCreated
from contracts.interaction import InteractionOption, StructuredInteractionEvent
from contracts.tool_result import ToolError, ToolResult

from ..context import ToolExecutionContext
from ..specs import (
    AskUserInput,
    ConfirmActionInput,
    ShowErrorInput,
    ShowPreviewInput,
    ShowProgressInput,
    ShowTimelineInput,
)


def ask_user(input_model: AskUserInput, context: ToolExecutionContext) -> ToolResult:
    resolved = _resolve_scope(
        input_model.scope_type,
        input_model.project_id,
        input_model.case_id,
        context,
    )
    if resolved.error is not None:
        return resolved.error
    decision = Decision(
        decision_id=input_model.decision_id
        or _decision_id("ask", input_model.question, input_model.model_dump(mode="json")),
        scope_type=input_model.scope_type,
        project_id=resolved.project_id,
        case_id=resolved.case_id,
        type=input_model.decision_type,
        question=input_model.question,
        options=input_model.options,
        allow_free_text=input_model.allow_free_text,
        status="pending",
        blocking=input_model.scope_type == "case" and input_model.blocking,
        created_by_tool_call_id=context.tool_call_id,
    )
    interaction = StructuredInteractionEvent(
        kind="question",
        title=input_model.question,
        options=[_interaction_option(option) for option in input_model.options],
        metadata={
            **input_model.metadata,
            "decision_id": decision.decision_id,
            "allow_free_text": input_model.allow_free_text,
            # generic decision 未声明归约目标时默认 scratch_memory（安全语义），
            # 避免 answer 阶段无处归约（M9 实测 LLM 常不带该参数）
            "reduce_target": input_model.reduce_target or "scratch_memory",
        },
    )
    return _decision_result("interaction.ask_user", decision, interaction, context)


def confirm_action(input_model: ConfirmActionInput, context: ToolExecutionContext) -> ToolResult:
    resolved = _resolve_scope(
        input_model.scope_type,
        input_model.project_id,
        input_model.case_id,
        context,
    )
    if resolved.error is not None:
        return resolved.error
    options = input_model.options or _default_confirmation_options()
    decision = Decision(
        decision_id=input_model.decision_id
        or _decision_id("confirm", input_model.question, input_model.model_dump(mode="json")),
        scope_type=input_model.scope_type,
        project_id=resolved.project_id,
        case_id=resolved.case_id,
        type=input_model.decision_type,
        question=input_model.question,
        options=options,
        allow_free_text=False,
        status="pending",
        pending_tool_call=input_model.pending_tool_call,
        pending_tool_call_status="pending" if input_model.pending_tool_call is not None else None,
        blocking=input_model.scope_type == "case" and input_model.blocking,
        created_by_tool_call_id=context.tool_call_id,
    )
    interaction = StructuredInteractionEvent(
        kind="confirmation",
        title=input_model.question,
        options=[_interaction_option(option) for option in options],
        metadata={**input_model.metadata, "decision_id": decision.decision_id},
    )
    return _decision_result("interaction.confirm_action", decision, interaction, context)


def show_progress(input_model: ShowProgressInput, context: ToolExecutionContext) -> ToolResult:
    interaction = StructuredInteractionEvent(
        kind="progress",
        title=input_model.title,
        body=input_model.body,
        progress=input_model.progress,
        metadata=input_model.metadata,
    )
    return _interaction_result("interaction.show_progress", interaction, context)


def show_preview(input_model: ShowPreviewInput, context: ToolExecutionContext) -> ToolResult:
    interaction = StructuredInteractionEvent(
        kind="preview",
        title=input_model.title,
        body=input_model.body,
        media_ref=input_model.media_ref or input_model.preview_id,
        metadata={**input_model.metadata, "preview_id": input_model.preview_id},
    )
    return _interaction_result("interaction.show_preview", interaction, context)


def show_timeline(input_model: ShowTimelineInput, context: ToolExecutionContext) -> ToolResult:
    interaction = StructuredInteractionEvent(
        kind="timeline",
        title=input_model.title,
        body=input_model.body,
        timeline_summary=input_model.timeline_summary,
        metadata=input_model.metadata,
    )
    return _interaction_result("interaction.show_timeline", interaction, context)


def show_error(input_model: ShowErrorInput, context: ToolExecutionContext) -> ToolResult:
    interaction = StructuredInteractionEvent(
        kind="error",
        title=input_model.title,
        body=input_model.body,
        error_code=input_model.error_code,
        retryable=input_model.retryable,
        metadata=input_model.metadata,
    )
    return _interaction_result("interaction.show_error", interaction, context)


class _ResolvedScope:
    def __init__(
        self,
        *,
        project_id: str | None,
        case_id: str | None,
        error: ToolResult | None,
    ) -> None:
        self.project_id = project_id
        self.case_id = case_id
        self.error = error


def _resolve_scope(
    scope_type: str,
    project_id: str | None,
    case_id: str | None,
    context: ToolExecutionContext,
) -> _ResolvedScope:
    if scope_type == "workspace":
        return _ResolvedScope(project_id=None, case_id=None, error=None)
    if scope_type == "project":
        resolved_project_id = (
            project_id
            or (context.project_state.project_id if context.project_state is not None else None)
            or (context.case_state.project_id if context.case_state is not None else None)
        )
        if resolved_project_id is None:
            return _ResolvedScope(
                project_id=None,
                case_id=None,
                error=_failed(
                    "interaction",
                    context,
                    "missing_project",
                    "project-scoped decision requires project_id",
                ),
            )
        return _ResolvedScope(project_id=resolved_project_id, case_id=None, error=None)
    resolved_case_id = case_id or (
        context.case_state.case_id if context.case_state is not None else None
    )
    resolved_project_id = project_id or (
        context.case_state.project_id if context.case_state is not None else None
    )
    if resolved_project_id is None or resolved_case_id is None:
        return _ResolvedScope(
            project_id=None,
            case_id=None,
            error=_failed(
                "interaction",
                context,
                "missing_case",
                "case-scoped decision requires project_id and case_id",
            ),
        )
    return _ResolvedScope(project_id=resolved_project_id, case_id=resolved_case_id, error=None)


def _decision_result(
    tool_name: str,
    decision: Decision,
    interaction: StructuredInteractionEvent,
    context: ToolExecutionContext,
) -> ToolResult:
    pending_tool_call = (
        None
        if decision.pending_tool_call is None
        else decision.pending_tool_call.model_dump(mode="json")
    )
    event = DecisionCreated(
        decision_id=decision.decision_id,
        scope_type=decision.scope_type,
        project_id=decision.project_id,
        case_id=decision.case_id,
        payload={
            "decision": decision.model_dump(mode="json"),
            "type": decision.type,
            "question": decision.question,
            "options": [option.model_dump(mode="json") for option in decision.options],
            "pending_tool_call": pending_tool_call,
            "pending_tool_call_status": decision.pending_tool_call_status,
            "blocking": decision.blocking,
            "created_by_tool_call_id": context.tool_call_id,
        },
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="requires_user",
        observation=decision.question,
        data={"interaction": interaction.model_dump(mode="json")},
        events=[event.model_dump(mode="json")],
    )


def _interaction_result(
    tool_name: str,
    interaction: StructuredInteractionEvent,
    context: ToolExecutionContext,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=interaction.title,
        data={"interaction": interaction.model_dump(mode="json")},
    )


def _interaction_option(option: DecisionOption) -> InteractionOption:
    return InteractionOption(
        option_id=option.option_id,
        label=option.label,
        description=option.description,
        payload=option.payload,
    )


def _default_confirmation_options() -> list[DecisionOption]:
    return [
        DecisionOption(option_id="approve", label="确认", payload={"approved": True}),
        DecisionOption(option_id="reject", label="取消", payload={"approved": False}),
    ]


def _decision_id(prefix: str, question: str, payload: dict[str, Any]) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    digest = hashlib.sha256(f"{question}:{encoded}".encode()).hexdigest()[:16]
    return f"dec_{prefix}_{digest}"


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
