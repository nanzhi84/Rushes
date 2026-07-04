"""Registry-driven tool execution router."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from pydantic import ValidationError

from contracts.tool_result import ToolError, ToolResult
from tools.context import ToolExecutionContext
from tools.registry import ToolRegistry

from .policy_gate import ToolCall


class ToolRouter:
    """Resolve a tool by name, validate input, and execute its handler."""

    def __init__(
        self,
        registry: ToolRegistry,
        *,
        include_experimental: bool = False,
    ) -> None:
        self._registry = registry
        self._include_experimental = include_experimental

    def execute(
        self,
        tool_call: ToolCall | Mapping[str, Any],
        context: ToolExecutionContext,
    ) -> ToolResult:
        parsed_call = ToolCall.from_input(tool_call)
        registered = self._registry.get(
            parsed_call.tool_name,
            include_experimental=self._include_experimental,
        )
        if registered is None:
            return _failed_result(
                parsed_call,
                "unknown_tool",
                f"tool is not registered: {parsed_call.tool_name}",
            )
        try:
            input_model = registered.spec.input_model.model_validate(parsed_call.arguments)
        except ValidationError as exc:
            return _failed_result(
                parsed_call,
                "invalid_tool_input",
                "input_model strict validation failed",
                details={"validation_errors": exc.errors(include_url=False)},
            )
        return registered.handler(input_model, context)


def _failed_result(
    tool_call: ToolCall,
    error_code: str,
    message: str,
    *,
    details: dict[str, Any] | None = None,
) -> ToolResult:
    return ToolResult(
        tool_call_id=tool_call.tool_call_id or "tool_call_unknown",
        tool_name=tool_call.tool_name,
        status="failed",
        observation=message,
        error=ToolError(
            error_code=error_code,
            message=message,
            retryable=False,
            details=details or {},
        ),
    )
