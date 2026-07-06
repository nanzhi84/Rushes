from __future__ import annotations

import json
import os
from collections.abc import Mapping
from typing import Any

import httpx
import pytest
from pydantic import BaseModel, ConfigDict

from agent_harness.context_builder import ContextBundle
from contracts.provider import ProviderDescriptor, ProviderError, ProviderResult
from contracts.tool import ToolSpec
from providers.capabilities import LLM_CHAT, ProviderRequest
from providers.gateway import ProviderGateway
from providers.openai_compatible.llm import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    DEFAULT_OPENAI_COMPATIBLE_MODEL,
    OPENAI_COMPATIBLE_LLM_PROVIDER_ID,
    OpenAICompatibleLLMProvider,
    openai_compatible_llm_descriptor,
)
from providers.planner import GatewayLLMPlanner, build_openai_compatible_planner
from providers.registry import ProviderRegistry


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


class EchoInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    text: str


async def test_request_body_uses_required_tool_choice_and_strict_function_schema() -> None:
    captured: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(json.loads(request.content.decode()))
        assert request.headers["authorization"] == "Bearer test-key"
        return _chat_response(tool_calls=[_tool_call("call_1", "echo", {"text": "hi"})])

    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=LLM_CHAT,
            request_id="req_1",
            payload={
                "messages": [{"role": "system", "content": "Use tools."}],
                "tools": [
                    {
                        "name": "echo",
                        "description": "Echo text.",
                        "parameters": EchoInput.model_json_schema(),
                    }
                ],
                "tool_choice": "required",
            },
        )
    )

    assert result.error is None
    body = captured[0]
    assert body["tool_choice"] == "required"
    assert body["tools"] == [
        {
            "type": "function",
            "function": {
                "name": "echo",
                "description": "Echo text.",
                "parameters": EchoInput.model_json_schema(),
                "strict": True,
            },
        }
    ]


async def test_single_tool_call_response_is_normalized() -> None:
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(
            lambda _request: _chat_response(
                content="",
                tool_calls=[_tool_call("call_1", "echo", {"text": "hi"})],
                usage={"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
            )
        ),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(_provider_request())

    assert result.error is None
    assert result.usage == {"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7}
    assert result.normalized_output["content"] == ""
    assert result.normalized_output["finish_reason"] == "tool_calls"
    assert result.normalized_output["usage"] == result.usage
    assert result.normalized_output["tool_calls"] == [
        {"id": "call_1", "type": "function", "name": "echo", "arguments": {"text": "hi"}}
    ]


async def test_parallel_tool_calls_are_truncated_with_warning() -> None:
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(
            lambda _request: _chat_response(
                tool_calls=[
                    _tool_call("call_1", "echo", {"text": "first"}),
                    _tool_call("call_2", "echo", {"text": "second"}),
                ]
            )
        ),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(_provider_request())

    assert result.error is None
    assert result.warnings == ["parallel tool calls truncated"]
    assert result.normalized_output["tool_calls"] == [
        {"id": "call_1", "type": "function", "name": "echo", "arguments": {"text": "first"}}
    ]


async def test_retryable_429_retries_then_succeeds() -> None:
    attempts = 0

    def handler(_request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        if attempts == 1:
            return httpx.Response(429, json={"error": {"message": "rate limited"}})
        return _chat_response(tool_calls=[_tool_call("call_1", "echo", {"text": "ok"})])

    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(_provider_request())

    assert attempts == 2
    assert result.error is None
    assert result.normalized_output["tool_calls"][0]["arguments"] == {"text": "ok"}


async def test_non_retryable_4xx_does_not_retry() -> None:
    attempts = 0

    def handler(_request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        return httpx.Response(400, json={"error": {"message": "bad request"}})

    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(_provider_request())

    assert attempts == 1
    assert result.error is not None
    assert result.error.error_code == "http_status_400"
    assert result.error.retryable is False


async def test_timeout_retries_then_succeeds() -> None:
    attempts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        if attempts == 1:
            raise httpx.ReadTimeout("slow", request=request)
        return _chat_response(tool_calls=[_tool_call("call_1", "echo", {"text": "ok"})])

    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(_provider_request())

    assert attempts == 2
    assert result.error is None
    assert result.normalized_output["tool_calls"][0]["arguments"] == {"text": "ok"}


async def test_planner_returns_content_only_when_no_tool_call() -> None:
    adapter = RecordingAdapter(
        ProviderResult(
            provider_id="mock",
            capability=LLM_CHAT,
            request_id="req_1",
            model="mock-model",
            latency_ms=0,
            normalized_output={"content": "直接回复用户"},
        )
    )
    planner = GatewayLLMPlanner(_gateway_for(adapter))

    call = await planner.plan(_context_bundle(), [_echo_spec()])

    assert call.tool_name is None
    assert call.content == "直接回复用户"
    assert call.arguments == {}


async def test_planner_provider_error_returns_content_reply() -> None:
    adapter = RecordingAdapter(
        ProviderResult(
            provider_id="mock",
            capability=LLM_CHAT,
            request_id="req_1",
            model="mock-model",
            latency_ms=0,
            error=ProviderError(
                error_code="timeout",
                message="slow",
                retryable=True,
            ),
        )
    )
    planner = GatewayLLMPlanner(_gateway_for(adapter))

    call = await planner.plan(_context_bundle(), [_echo_spec()])

    assert call.tool_name is None
    assert call.content is not None
    assert "timeout: slow" in call.content


async def test_planner_accepts_singular_tool_call_with_json_arguments() -> None:
    adapter = RecordingAdapter(
        ProviderResult(
            provider_id="mock",
            capability=LLM_CHAT,
            request_id="req_1",
            model="mock-model",
            latency_ms=0,
            normalized_output={
                "tool_call": {
                    "id": "call_1",
                    "function": {
                        "name": "echo",
                        "arguments": '{"text":"ok"}',
                    },
                }
            },
        )
    )
    planner = GatewayLLMPlanner(_gateway_for(adapter))

    call = await planner.plan(_context_bundle(), [_echo_spec()])

    assert call.tool_name == "echo"
    assert call.tool_call_id == "call_1"
    assert call.arguments == {"text": "ok"}


async def test_planner_builds_messages_and_tools_in_prd_block_order() -> None:
    adapter = RecordingAdapter(
        ProviderResult(
            provider_id="mock",
            capability=LLM_CHAT,
            request_id="req_1",
            model="mock-model",
            latency_ms=0,
            normalized_output={
                "tool_calls": [{"id": "call_1", "name": "echo", "arguments": {"text": "ok"}}]
            },
        )
    )
    planner = GatewayLLMPlanner(_gateway_for(adapter), model="mock-model")
    context = _context_bundle()

    call = await planner.plan(context, context.allowed_tools)

    assert call.tool_name == "echo"
    assert call.arguments == {"text": "ok"}
    payload = adapter.requests[0].payload
    content = payload["messages"][0]["content"]
    block_positions = _positions(
        content,
        "## system",
        "## workspace",
        "## case_header",
        "## artifacts",
        "## pending_decision",
        "## memory",
        "## assets",
        "## messages",
        "## allowed_tools",
    )
    assert block_positions == sorted(block_positions)
    assert payload["tool_choice"] == "auto"
    assert payload["tools"][0]["function"]["strict"] is True
    assert payload["tools"][0]["function"]["parameters"] == EchoInput.model_json_schema()


def test_descriptor_and_factory_use_openai_compatible_llm_contract() -> None:
    descriptor = openai_compatible_llm_descriptor(priority=7)
    planner = build_openai_compatible_planner(api_key="key", model="model")

    assert descriptor.provider_id == OPENAI_COMPATIBLE_LLM_PROVIDER_ID
    assert descriptor.capabilities == [LLM_CHAT]
    assert descriptor.supports_json_schema is True
    assert descriptor.priority == 7
    assert isinstance(planner, GatewayLLMPlanner)


@pytest.mark.external
@pytest.mark.skipif(
    "RUSHES_DASHSCOPE_API_KEY" not in os.environ,
    reason="RUSHES_DASHSCOPE_API_KEY is required for the external smoke test",
)
async def test_external_dashscope_qwen_plus_returns_echo_tool_call() -> None:
    adapter = OpenAICompatibleLLMProvider(
        base_url=os.environ.get("RUSHES_LLM_BASE_URL", DEFAULT_OPENAI_COMPATIBLE_BASE_URL),
        api_key=os.environ["RUSHES_DASHSCOPE_API_KEY"],
        model=os.environ.get("RUSHES_LLM_MODEL", DEFAULT_OPENAI_COMPATIBLE_MODEL),
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=LLM_CHAT,
            payload={
                "messages": [
                    {
                        "role": "user",
                        "content": "Call echo with text ping. Do not answer in prose.",
                    }
                ],
                "tools": [
                    {
                        "name": "echo",
                        "description": "Echo text.",
                        "parameters": EchoInput.model_json_schema(),
                    }
                ],
                "tool_choice": "required",
            },
        )
    )

    assert result.error is None
    assert result.normalized_output["tool_calls"][0]["name"] == "echo"


class RecordingAdapter:
    def __init__(self, result: ProviderResult) -> None:
        self._result = result
        self.requests: list[ProviderRequest] = []

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        self.requests.append(request)
        return self._result.model_copy(update={"request_id": request.request_id or "req_1"})


def _provider_request() -> ProviderRequest:
    return ProviderRequest(
        capability=LLM_CHAT,
        payload={
            "messages": [{"role": "system", "content": "Use tools."}],
            "tools": [
                {
                    "name": "echo",
                    "description": "Echo text.",
                    "parameters": EchoInput.model_json_schema(),
                }
            ],
            "tool_choice": "required",
        },
    )


def _chat_response(
    *,
    content: str = "",
    tool_calls: list[dict[str, Any]] | None = None,
    usage: Mapping[str, int] | None = None,
    finish_reason: str = "tool_calls",
) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "id": "chatcmpl_1",
            "model": "qwen-plus",
            "choices": [
                {
                    "message": {
                        "role": "assistant",
                        "content": content,
                        "tool_calls": tool_calls or [],
                    },
                    "finish_reason": finish_reason,
                }
            ],
            "usage": dict(usage or {"prompt_tokens": 1, "completion_tokens": 1}),
        },
    )


def _tool_call(call_id: str, name: str, arguments: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(dict(arguments))},
    }


def _gateway_for(adapter: RecordingAdapter) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock",
            display_name="Mock",
            version="1",
            capabilities=[LLM_CHAT],
            config_model=EmptyConfig,
            client_ref="tests.RecordingAdapter",
            supports_json_schema=True,
        ),
        adapter,
    )
    return ProviderGateway(registry=registry)


def _context_bundle() -> ContextBundle:
    blocks = {
        "system": "system block",
        "workspace": "workspace block",
        "case_header": "case block",
        "artifacts": "artifacts block",
        "pending_decision": "pending block",
        "memory": "memory block",
        "assets": "assets block",
        "messages": "messages block",
        "allowed_tools": "allowed block",
    }
    return ContextBundle(
        blocks=blocks,
        token_counts={name: 1 for name in blocks},
        allowed_tools=[_echo_spec()],
    )


def _echo_spec() -> ToolSpec:
    return ToolSpec(
        name="echo",
        namespace="test",
        version="1",
        input_model=EchoInput,
        result_model=None,
        handler_ref="tests.echo",
        allowed_scopes=["case_agent_console"],
        requires_artifacts=[],
        requires_active_project=False,
        requires_active_case=False,
        side_effects=[],
        emits_events=[],
        description="Echo text.",
    )


def _positions(content: str, *needles: str) -> list[int]:
    return [content.index(needle) for needle in needles]


def _sse_response(*events: dict[str, Any] | str) -> httpx.Response:
    blocks: list[str] = []
    for event in events:
        data = event if isinstance(event, str) else json.dumps(event)
        blocks.append(f"data: {data}")
    body = "\n\n".join(blocks) + "\n\n"
    return httpx.Response(200, content=body.encode("utf-8"))


async def test_invoke_stream_parses_content_tool_call_and_usage() -> None:
    captured_body: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        captured_body.append(json.loads(request.content.decode()))
        return _sse_response(
            {
                "id": "chatcmpl_stream",
                "model": "qwen-plus",
                "choices": [{"delta": {"role": "assistant", "content": "你好"}}],
            },
            {"choices": [{"delta": {"content": "，世界"}}]},
            {
                "choices": [
                    {
                        "delta": {
                            "tool_calls": [
                                {
                                    "index": 0,
                                    "id": "call_1",
                                    "type": "function",
                                    "function": {"name": "echo", "arguments": '{"te'},
                                }
                            ]
                        }
                    }
                ]
            },
            {
                "choices": [
                    {
                        "delta": {
                            "tool_calls": [{"index": 0, "function": {"arguments": 'xt":"hi"}'}}]
                        }
                    }
                ]
            },
            {"choices": [{"delta": {}, "finish_reason": "tool_calls"}]},
            {
                "choices": [],
                "usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
            },
            "[DONE]",
        )

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    body = captured_body[0]
    assert body["stream"] is True
    assert body["stream_options"] == {"include_usage": True}

    assert list(deltas) == [
        {"type": "text", "text": "你好"},
        {"type": "text", "text": "，世界"},
    ]

    assert result.error is None
    assert result.normalized_output["content"] == "你好，世界"
    assert result.normalized_output["finish_reason"] == "tool_calls"
    assert result.usage == {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11}
    assert result.normalized_output["usage"] == result.usage
    assert result.normalized_output["tool_calls"] == [
        {"id": "call_1", "type": "function", "name": "echo", "arguments": {"text": "hi"}}
    ]
    assert result.raw_ref == "chatcmpl_stream"
    assert result.model == "qwen-plus"


async def test_invoke_stream_plain_text_without_tool_calls() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        return _sse_response(
            {"choices": [{"delta": {"content": "只"}}]},
            {"choices": [{"delta": {"content": "回复"}}]},
            {"choices": [{"delta": {}, "finish_reason": "stop"}]},
            "[DONE]",
        )

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    assert result.error is None
    assert result.normalized_output["content"] == "只回复"
    assert result.normalized_output["tool_calls"] == []
    assert result.normalized_output["finish_reason"] == "stop"
    assert list(deltas) == [
        {"type": "text", "text": "只"},
        {"type": "text", "text": "回复"},
    ]


async def test_invoke_stream_tolerates_non_data_lines_and_malformed_chunks() -> None:
    lines = [
        ": keep-alive comment",
        "data: {bad json",
        'data: "not-a-mapping-chunk"',
        "data: " + json.dumps({"choices": [{"delta": "not-a-mapping-delta"}]}),
        "data: " + json.dumps({"choices": [{"delta": {"content": "你好"}}]}),
        "data: "
        + json.dumps(
            {
                "choices": [
                    {
                        "delta": {
                            "tool_calls": [
                                "not-a-mapping-item",
                                {
                                    "id": "call_1",
                                    "function": {"name": "echo", "arguments": '{"text":"hi"}'},
                                },
                            ]
                        }
                    }
                ]
            }
        ),
        "data: " + json.dumps({"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}),
        "data: [DONE]",
    ]
    body = "\n\n".join(lines) + "\n\n"

    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=body.encode("utf-8"))

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    assert result.error is None
    assert list(deltas) == [{"type": "text", "text": "你好"}]
    assert result.normalized_output["content"] == "你好"
    assert result.normalized_output["finish_reason"] == "tool_calls"
    # 缺 index 的 tool_call 分片按已累积数量兜底到槽位 0；非 Mapping 分片被跳过。
    assert result.normalized_output["tool_calls"] == [
        {"id": "call_1", "type": "function", "name": "echo", "arguments": {"text": "hi"}}
    ]


async def test_invoke_stream_http_400_returns_error_without_retry() -> None:
    attempts = 0

    def handler(_request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        return httpx.Response(400, json={"error": {"message": "bad stream request"}})

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    assert attempts == 1
    assert list(deltas) == []
    assert result.error is not None
    assert result.error.error_code == "http_status_400"
    assert result.error.retryable is False
    assert result.error.message == "bad stream request"


async def test_invoke_stream_transport_error_returns_error_without_retry() -> None:
    attempts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        raise httpx.ConnectError("connection refused", request=request)

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    assert attempts == 1
    assert list(deltas) == []
    assert result.error is not None
    assert result.error.error_code == "network_error"
    assert result.error.retryable is True


async def test_invoke_stream_timeout_returns_error_without_retry() -> None:
    attempts = 0

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal attempts
        attempts += 1
        raise httpx.ReadTimeout("read timed out", request=request)

    deltas: list[Mapping[str, Any]] = []
    adapter = OpenAICompatibleLLMProvider(
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke_stream(_provider_request(), on_delta=deltas.append)

    assert attempts == 1
    assert list(deltas) == []
    assert result.error is not None
    assert result.error.error_code == "timeout"
    assert result.error.retryable is True


async def test_network_error_exhausts_retries_with_structured_error() -> None:
    calls: list[int] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(1)
        raise httpx.ConnectError("connection refused", request=request)

    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=LLM_CHAT,
            request_id="req_net",
            payload={"messages": [{"role": "user", "content": "hi"}]},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "network_error"
    assert result.error.retryable is True
    assert len(calls) == 3


async def test_timeout_exhausts_retries_with_structured_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("read timed out", request=request)

    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=LLM_CHAT,
            request_id="req_timeout",
            payload={"messages": [{"role": "user", "content": "hi"}]},
        )
    )

    assert result.error is not None
    assert result.error.error_code == "timeout"
    assert result.error.retryable is True


async def test_request_body_merges_params_and_passes_prenormalized_function_tool() -> None:
    captured: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(json.loads(request.content.decode()))
        return _chat_response(tool_calls=[_tool_call("call_1", "echo", {"text": "hi"})])

    adapter = OpenAICompatibleLLMProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
        retry_base_delay_seconds=0,
    )

    result = await adapter.invoke(
        ProviderRequest(
            capability=LLM_CHAT,
            request_id="req_params",
            payload={
                "messages": [{"role": "user", "content": "hi"}],
                "tools": [
                    {
                        "type": "function",
                        "function": {"name": "echo", "parameters": {"type": "object"}},
                    }
                ],
                "params": {"temperature": 0.1},
            },
        )
    )

    assert result.error is None
    body = captured[0]
    assert body["temperature"] == 0.1
    assert body["tools"][0]["function"]["strict"] is True
    assert body["tools"][0]["function"]["name"] == "echo"


async def test_planner_handles_unknown_blocks_and_non_json_arguments() -> None:
    adapter = RecordingAdapter(
        ProviderResult(
            provider_id="mock",
            capability=LLM_CHAT,
            request_id="req_1",
            model="mock-model",
            latency_ms=0,
            normalized_output={
                "tool_call": {
                    "id": "call_x",
                    "function": {"name": "echo", "arguments": "not-json"},
                }
            },
        )
    )
    planner = GatewayLLMPlanner(_gateway_for(adapter))
    bundle = _context_bundle()
    bundle.blocks["custom_extra"] = "自定义区块"

    call = await planner.plan(bundle, [_echo_spec()])

    assert call.tool_name == "echo"
    assert call.arguments == {"_raw_arguments": "not-json"}
    sent_messages = adapter.requests[0].payload["messages"]
    assert any("custom_extra" in str(m.get("content", "")) for m in sent_messages)


def test_planner_tool_call_supports_mapping_protocol() -> None:
    from providers.planner import PlannerToolCall

    call = PlannerToolCall(tool_name="echo", arguments={"text": "hi"}, tool_call_id="c1")

    assert call["tool_name"] == "echo"
    assert call["content"] is None
    assert set(iter(call)) == set(call.model_dump())
    assert len(call) == len(call.model_dump())
