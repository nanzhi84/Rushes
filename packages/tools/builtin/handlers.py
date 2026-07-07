"""Harness-level built-in tool handlers."""

from __future__ import annotations

from typing import Any

from contracts.decision import Decision
from contracts.events import DecisionAnswered
from contracts.tool_result import ToolError, ToolResult
from storage.repositories import DecisionsRepository

from ..context import ToolExecutionContext
from ..specs import DecisionAnswerInput


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
        draft_id=decision.draft_id,
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
