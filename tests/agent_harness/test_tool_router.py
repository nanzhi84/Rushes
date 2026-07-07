import inspect

from pydantic import BaseModel, ConfigDict

from agent_harness.policy_gate import ToolCall
from agent_harness.tool_router import ToolRouter
from contracts.tool import ToolSpec
from contracts.tool_result import ToolResult
from tools import ToolExecutionContext
from tools.registry import ToolRegistry


class EchoInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    text: str


def _echo(input_model: EchoInput, context: ToolExecutionContext) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="x.echo",
        status="succeeded",
        observation=input_model.text,
    )


async def _async_echo(input_model: EchoInput, context: ToolExecutionContext) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="x.async_echo",
        status="succeeded",
        observation=input_model.text,
    )


def _echo_spec(name: str) -> ToolSpec:
    return ToolSpec(
        name=name,
        namespace="x",
        version="1",
        input_model=EchoInput,
        result_model=None,
        handler_ref=f"tests.{name}",
        allowed_scopes=["draft_editor"],
        requires_artifacts=[],
        requires_active_draft=False,
        side_effects=[],
        emits_events=[],
        description="echo",
    )


def _registry() -> ToolRegistry:
    registry = ToolRegistry()
    registry.register(_echo_spec("x.echo"), _echo)
    return registry


def test_tool_router_executes_registered_handler() -> None:
    result = ToolRouter(_registry()).execute(
        ToolCall(tool_name="x.echo", arguments={"text": "hello"}, tool_call_id="tc_1"),
        ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"),
    )

    assert result.status == "succeeded"
    assert result.observation == "hello"


def test_tool_router_returns_structured_error_for_unknown_tool() -> None:
    result = ToolRouter(_registry()).execute(
        ToolCall(tool_name="shell.exec", arguments={}, tool_call_id="tc_1"),
        ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"),
    )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "unknown_tool"


def test_tool_router_uses_strict_input_model_validation() -> None:
    result = ToolRouter(_registry()).execute(
        ToolCall(
            tool_name="x.echo",
            arguments={"text": "hello", "extra": "nope"},
            tool_call_id="tc_1",
        ),
        ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"),
    )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "invalid_tool_input"


async def test_tool_router_returns_awaitable_for_async_handler() -> None:
    registry = ToolRegistry()
    registry.register(_echo_spec("x.async_echo"), _async_echo)

    outcome = ToolRouter(registry).execute(
        ToolCall(tool_name="x.async_echo", arguments={"text": "hi"}, tool_call_id="tc_1"),
        ToolExecutionContext(tool_call_id="tc_1", turn_id="turn_1"),
    )

    assert inspect.isawaitable(outcome)
    result = await outcome
    assert result.status == "succeeded"
    assert result.observation == "hi"
