"""Harness-level built-in tool handlers."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from contracts.decision import Decision
from contracts.events import DecisionAnswered, TurnEnded
from contracts.tool_result import ToolError, ToolResult
from storage.repositories import DecisionsRepository

from ..context import ToolExecutionContext
from ..specs import DecisionAnswerInput, FinishTurnInput, RefuseInput, RespondInput


def respond(input_model: RespondInput, context: ToolExecutionContext) -> ToolResult:
    if context.case_state is None:
        return _failed("respond", context, "missing_case", "respond requires an active case")
    content = input_model.message
    message_row = {
        "message_id": input_model.message_id or _message_id(context),
        "case_id": context.case_state.case_id,
        "role": "assistant",
        "content": content,
        "created_at": _created_at(context),
    }
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="respond",
        status="succeeded",
        observation=content,
        data={"message_row": message_row},
        events=[
            TurnEnded(
                turn_id=context.turn_id,
                case_id=context.case_state.case_id,
                project_id=context.case_state.project_id,
                payload={"reason": "respond"},
            ).model_dump(mode="json")
        ],
    )


def refuse(input_model: RefuseInput, context: ToolExecutionContext) -> ToolResult:
    if context.case_state is None:
        return _failed("refuse", context, "missing_case", "refuse requires an active case")
    content = input_model.message or (f"这个请求超出了当前剪辑 Agent 的边界：{input_model.reason}")
    message_row = {
        "message_id": input_model.message_id or _message_id(context),
        "case_id": context.case_state.case_id,
        "role": "assistant",
        "content": content,
        "created_at": _created_at(context),
    }
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="refuse",
        status="succeeded",
        observation=content,
        data={"message_row": message_row, "refusal_reason": input_model.reason},
        events=[
            TurnEnded(
                turn_id=context.turn_id,
                case_id=context.case_state.case_id,
                project_id=context.case_state.project_id,
                payload={"reason": "refuse", "refusal_reason": input_model.reason},
            ).model_dump(mode="json")
        ],
    )


def finish_turn(input_model: FinishTurnInput, context: ToolExecutionContext) -> ToolResult:
    if context.case_state is None:
        return _failed(
            "finish_turn",
            context,
            "missing_case",
            "finish_turn requires an active case",
        )
    reason = input_model.reason or "finish_turn"
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="finish_turn",
        status="succeeded",
        observation=reason,
        events=[
            TurnEnded(
                turn_id=context.turn_id,
                case_id=context.case_state.case_id,
                project_id=context.case_state.project_id,
                payload={"reason": reason},
            ).model_dump(mode="json")
        ],
    )


def decision_answer(input_model: DecisionAnswerInput, context: ToolExecutionContext) -> ToolResult:
    decision = _find_decision(input_model.decision_id, context)
    if decision is None:
        return _failed(
            "decision.answer",
            context,
            "decision_not_found",
            f"decision not found: {input_model.decision_id}",
        )
    event = DecisionAnswered(
        decision_id=decision.decision_id,
        scope_type=decision.scope_type,
        project_id=decision.project_id,
        case_id=decision.case_id,
        payload={"answer": input_model.answer.model_dump(mode="json")},
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="decision.answer",
        status="succeeded",
        observation=f"answered decision {decision.decision_id}",
        events=[event.model_dump(mode="json")],
    )


def _find_decision(decision_id: str, context: ToolExecutionContext) -> Decision | None:
    for decision in context.decisions:
        if decision.decision_id == decision_id:
            return decision
    if context.readonly_connection is None:
        return None
    row = DecisionsRepository(context.readonly_connection).get(decision_id)
    return None if row is None else Decision.model_validate(row)


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


def _message_id(context: ToolExecutionContext) -> str:
    safe_turn = context.turn_id.replace(":", "_")
    safe_call = context.tool_call_id.replace(":", "_")
    return f"msg_{safe_turn}_{safe_call}"


def _created_at(context: ToolExecutionContext) -> str:
    return context.created_at or datetime.now(UTC).isoformat()
