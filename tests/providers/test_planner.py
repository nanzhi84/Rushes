"""GatewayLLMPlanner content protocol + gateway streaming + MappingPlannerAdapter."""

from __future__ import annotations

from collections.abc import Callable, Mapping
from typing import Any

from pydantic import BaseModel, ConfigDict

from agent_harness.context_builder import ContextBundle
from agent_harness.loop import MappingPlannerAdapter, PlannerStep
from contracts.provider import ProviderResult
from contracts.tool import ToolSpec
from providers import LLM_CHAT, ProviderGateway, ProviderRegistry, ProviderRequest
from providers.mock import MockProvider
from providers.planner import GatewayLLMPlanner, PlannerToolCall


class EchoInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    text: str


def _echo_spec() -> ToolSpec:
    return ToolSpec(
        name="echo",
        namespace="test",
        version="1",
        input_model=EchoInput,
        result_model=None,
        handler_ref="tests.echo",
        allowed_scopes=["draft_editor"],
        requires_artifacts=[],
        requires_active_draft=False,
        side_effects=[],
        emits_events=[],
        description="Echo text.",
    )


def _context_bundle() -> ContextBundle:
    blocks = {"system": "system block", "allowed_tools": "allowed block"}
    return ContextBundle(
        blocks=blocks,
        token_counts={name: 1 for name in blocks},
        allowed_tools=[_echo_spec()],
    )


def _gateway_for(provider: object, *, provider_id: str = "mock") -> ProviderGateway:
    from contracts.provider import ProviderDescriptor

    class _EmptyConfig(BaseModel):
        model_config = ConfigDict(extra="forbid")

    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id=provider_id,
            display_name="Mock",
            version="1",
            capabilities=[LLM_CHAT],
            config_model=_EmptyConfig,
            client_ref="tests.mock",
            supports_json_schema=True,
        ),
        provider,
    )
    return ProviderGateway(registry=registry)


class _InvokeOnlyAdapter:
    """Adapter that only exposes ``invoke`` (no ``invoke_stream``)."""

    def __init__(self, content: str) -> None:
        self._content = content

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        return ProviderResult(
            provider_id="mock",
            capability=request.capability,
            request_id=request.request_id or "req_1",
            model=request.model or "mock-model",
            latency_ms=0,
            normalized_output={"content": self._content},
        )


def _mock_gateway(script: list[dict[str, Any]]) -> ProviderGateway:
    return _gateway_for(MockProvider(scripts={LLM_CHAT: script}))


def _planner(script: list[dict[str, Any]]) -> GatewayLLMPlanner:
    return GatewayLLMPlanner(_mock_gateway(script), provider_id="mock")


# --- planner content protocol ------------------------------------------------


async def test_planner_returns_content_only_when_no_tool_call() -> None:
    planner = _planner([{"content": "直接回复用户"}])

    message = await planner.plan(_context_bundle(), [_echo_spec()])

    assert message.model_dump() == {
        "content": "直接回复用户",
        "tool_name": None,
        "arguments": {},
        "tool_call_id": None,
    }


async def test_planner_returns_tool_call_only() -> None:
    planner = _planner([{"tool_call": {"id": "c1", "name": "echo", "arguments": {"text": "ok"}}}])

    message = await planner.plan(_context_bundle(), [_echo_spec()])

    assert message.model_dump() == {
        "content": None,
        "tool_name": "echo",
        "arguments": {"text": "ok"},
        "tool_call_id": "c1",
    }


async def test_planner_returns_mixed_content_and_tool_call() -> None:
    planner = _planner(
        [
            {
                "content": "先说明再执行",
                "tool_call": {"id": "c2", "name": "echo", "arguments": {"text": "go"}},
            }
        ]
    )

    message = await planner.plan(_context_bundle(), [_echo_spec()])

    assert message.model_dump() == {
        "content": "先说明再执行",
        "tool_name": "echo",
        "arguments": {"text": "go"},
        "tool_call_id": "c2",
    }


async def test_planner_provider_error_returns_content_reply() -> None:
    planner = _planner([{"error": {"error_code": "timeout", "message": "slow", "retryable": True}}])

    message = await planner.plan(_context_bundle(), [_echo_spec()])

    dumped = message.model_dump()
    assert dumped["tool_name"] is None
    assert dumped["arguments"] == {}
    assert dumped["tool_call_id"] is None
    assert dumped["content"] is not None
    assert "LLM provider 调用失败：timeout: slow" in dumped["content"]


async def test_planner_default_tool_choice_is_auto() -> None:
    provider = MockProvider(scripts={LLM_CHAT: [{"content": "hi"}]})
    captured: list[ProviderRequest] = []
    original_invoke = provider.invoke

    async def _spy(request: ProviderRequest) -> ProviderResult:
        captured.append(request)
        return await original_invoke(request)

    provider.invoke = _spy  # type: ignore[method-assign]
    planner = GatewayLLMPlanner(_gateway_for(provider), provider_id="mock")

    await planner.plan(_context_bundle(), [_echo_spec()])

    assert captured[0].payload["tool_choice"] == "auto"


# --- streaming through the gateway -------------------------------------------


async def test_gateway_streams_content_in_eight_char_chunks() -> None:
    gateway = _mock_gateway([{"content": "0123456789ABCDEFGHIJ"}])
    chunks: list[Mapping[str, Any]] = []

    result = await gateway.call(ProviderRequest(capability=LLM_CHAT), on_delta=chunks.append)

    assert [chunk["text"] for chunk in chunks] == ["01234567", "89ABCDEF", "GHIJ"]
    assert all(chunk["type"] == "text" for chunk in chunks)
    assert result.result.normalized_output["content"] == "0123456789ABCDEFGHIJ"


async def test_gateway_replays_full_content_when_provider_lacks_stream() -> None:
    gateway = _gateway_for(_InvokeOnlyAdapter("整段一次性回放"))
    chunks: list[Mapping[str, Any]] = []

    result = await gateway.call(ProviderRequest(capability=LLM_CHAT), on_delta=chunks.append)

    assert [chunk["text"] for chunk in chunks] == ["整段一次性回放"]
    assert result.result.normalized_output["content"] == "整段一次性回放"


async def test_gateway_without_on_delta_does_not_stream() -> None:
    provider = MockProvider(scripts={LLM_CHAT: [{"content": "abcdefghij"}]})
    gateway = _gateway_for(provider)

    result = await gateway.call(ProviderRequest(capability=LLM_CHAT))

    assert result.result.normalized_output["content"] == "abcdefghij"


async def test_planner_forwards_stream_text_deltas_as_strings() -> None:
    planner = _planner([{"content": "abcdefghijklmnop"}])
    forwarded: list[str] = []
    on_delta: Callable[[str], None] = forwarded.append

    message = await planner.plan(_context_bundle(), [_echo_spec()], on_delta=on_delta)

    assert forwarded == ["abcdefgh", "ijklmnop"]
    assert message.model_dump()["content"] == "abcdefghijklmnop"


# --- MockProvider.invoke_stream directly -------------------------------------


async def test_mock_provider_invoke_stream_chunks_by_eight() -> None:
    provider = MockProvider(scripts={LLM_CHAT: [{"content": "hello world!!"}]})
    chunks: list[Mapping[str, Any]] = []

    result = await provider.invoke_stream(
        ProviderRequest(capability=LLM_CHAT), on_delta=chunks.append
    )

    assert [chunk["text"] for chunk in chunks] == ["hello wo", "rld!!"]
    assert result.normalized_output["content"] == "hello world!!"


async def test_mock_provider_invoke_stream_tool_call_has_no_content_deltas() -> None:
    provider = MockProvider(
        scripts={LLM_CHAT: [{"tool_call": {"id": "c1", "name": "echo", "arguments": {}}}]}
    )
    chunks: list[Mapping[str, Any]] = []

    result = await provider.invoke_stream(
        ProviderRequest(capability=LLM_CHAT), on_delta=chunks.append
    )

    assert chunks == []
    assert result.normalized_output["tool_call"]["name"] == "echo"


# --- MappingPlannerAdapter -> PlannerStep ------------------------------------


async def test_adapter_maps_content_only_to_plannerstep() -> None:
    adapter = MappingPlannerAdapter(_planner([{"content": "只回复"}]))

    step = await adapter.plan(_context_bundle(), [_echo_spec()])

    assert isinstance(step, PlannerStep)
    assert step.content == "只回复"
    assert step.tool_call is None


async def test_adapter_maps_tool_call_to_plannerstep() -> None:
    adapter = MappingPlannerAdapter(
        _planner([{"tool_call": {"id": "c1", "name": "echo", "arguments": {"text": "ok"}}}])
    )

    step = await adapter.plan(_context_bundle(), [_echo_spec()])

    assert step.content is None
    assert step.tool_call is not None
    assert step.tool_call.tool_name == "echo"
    assert step.tool_call.arguments == {"text": "ok"}
    assert step.tool_call.tool_call_id == "c1"


async def test_adapter_maps_mixed_to_plannerstep() -> None:
    adapter = MappingPlannerAdapter(
        _planner(
            [
                {
                    "content": "先叙述",
                    "tool_call": {"id": "c9", "name": "echo", "arguments": {"text": "x"}},
                }
            ]
        )
    )

    step = await adapter.plan(_context_bundle(), [_echo_spec()])

    assert step.content == "先叙述"
    assert step.tool_call is not None
    assert step.tool_call.tool_name == "echo"
    assert step.tool_call.tool_call_id == "c9"


async def test_adapter_forwards_on_delta() -> None:
    adapter = MappingPlannerAdapter(_planner([{"content": "abcdefghij"}]))
    forwarded: list[str] = []

    await adapter.plan(_context_bundle(), [_echo_spec()], on_delta=forwarded.append)

    assert "".join(forwarded) == "abcdefghij"


def test_planner_tool_call_mapping_protocol_exposes_content_key() -> None:
    call = PlannerToolCall(
        content=None, tool_name="echo", arguments={"text": "hi"}, tool_call_id="c1"
    )

    assert call["tool_name"] == "echo"
    assert set(iter(call)) == {"content", "tool_name", "arguments", "tool_call_id"}
    assert len(call) == 4
