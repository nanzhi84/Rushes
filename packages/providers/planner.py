"""ProviderGateway-backed LLM planner adapter.

This module intentionally does not import ``agent_harness``.  The harness
accepts a structural planner object, while this adapter returns a small mapping
object that exposes the same fields and ``model_dump`` surface used by the loop.
"""

from __future__ import annotations

import json
from collections.abc import Iterator, Mapping, Sequence
from typing import Any, Protocol

from contracts.provider import ProviderError
from contracts.tool import ToolSpec
from providers.capabilities import LLM_CHAT, ProviderRequest
from providers.gateway import ProviderCallRecorder, ProviderGateway
from providers.openai_compatible import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    DEFAULT_OPENAI_COMPATIBLE_MODEL,
    OpenAICompatibleLLMProvider,
    openai_compatible_llm_descriptor,
)
from providers.registry import ProviderRegistry

CONTEXT_BLOCK_ORDER: tuple[str, ...] = (
    "system",
    "workspace",
    "case_header",
    "artifacts",
    "pending_decision",
    "memory",
    "assets",
    "messages",
    "allowed_tools",
)


class ContextBundleLike(Protocol):
    blocks: Mapping[str, str]


class PlannerToolCall(Mapping[str, Any]):
    def __init__(
        self,
        *,
        tool_name: str,
        arguments: Mapping[str, Any] | None = None,
        tool_call_id: str | None = None,
        idempotency_key: str | None = None,
    ) -> None:
        self.tool_name = tool_name
        self.arguments = dict(arguments or {})
        self.tool_call_id = tool_call_id
        self.idempotency_key = idempotency_key

    def __getitem__(self, key: str) -> Any:
        return self.model_dump()[key]

    def __iter__(self) -> Iterator[str]:
        return iter(self.model_dump())

    def __len__(self) -> int:
        return len(self.model_dump())

    def model_dump(self, *args: Any, **kwargs: Any) -> dict[str, Any]:
        del args, kwargs
        return {
            "tool_name": self.tool_name,
            "arguments": self.arguments,
            "tool_call_id": self.tool_call_id,
            "idempotency_key": self.idempotency_key,
        }


class GatewayLLMPlanner:
    def __init__(
        self,
        gateway: ProviderGateway,
        *,
        model: str | None = None,
        provider_id: str | None = None,
        tool_choice: str | Mapping[str, Any] = "required",
        params: Mapping[str, Any] | None = None,
    ) -> None:
        self._gateway = gateway
        self._model = model
        self._provider_id = provider_id
        self._tool_choice = tool_choice
        self._params = dict(params or {})

    async def plan(
        self,
        context: ContextBundleLike,
        tools: Sequence[ToolSpec],
    ) -> PlannerToolCall:
        response = await self._gateway.call(
            ProviderRequest(
                capability=LLM_CHAT,
                model=self._model,
                payload={
                    "messages": context_messages(context),
                    "tools": tool_specs_to_openai_tools(tools),
                    "tool_choice": self._tool_choice,
                    "params": self._params,
                },
            ),
            provider_id=self._provider_id,
        )
        if response.result.error is not None:
            return _provider_error_respond(response.result.error)
        tool_call = _first_tool_call(response.result.normalized_output)
        if tool_call is not None:
            return tool_call
        content = response.result.normalized_output.get("content")
        message = content if isinstance(content, str) and content else "模型没有返回工具调用。"
        return PlannerToolCall(tool_name="respond", arguments={"message": message})


def build_openai_compatible_planner(
    *,
    base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    api_key: str,
    model: str = DEFAULT_OPENAI_COMPATIBLE_MODEL,
    timeout: float = 60.0,
    priority: int = 100,
    recorder: ProviderCallRecorder | None = None,
    params: Mapping[str, Any] | None = None,
) -> GatewayLLMPlanner:
    registry = ProviderRegistry()
    registry.register(
        openai_compatible_llm_descriptor(priority=priority),
        OpenAICompatibleLLMProvider(
            base_url=base_url,
            api_key=api_key,
            model=model,
            timeout=timeout,
            default_params=params,
        ),
    )
    return GatewayLLMPlanner(
        ProviderGateway(registry=registry, recorder=recorder),
        model=model,
        provider_id="openai_compatible.llm",
    )


def context_messages(context: ContextBundleLike) -> list[dict[str, str]]:
    return [{"role": "system", "content": context_system_content(context.blocks)}]


def context_system_content(blocks: Mapping[str, str]) -> str:
    parts: list[str] = []
    for name in CONTEXT_BLOCK_ORDER:
        text = blocks.get(name)
        if text:
            parts.append(f"## {name}\n{text}")
    for name in sorted(set(blocks) - set(CONTEXT_BLOCK_ORDER)):
        text = blocks.get(name)
        if text:
            parts.append(f"## {name}\n{text}")
    return "\n\n".join(parts)


def tool_specs_to_openai_tools(tools: Sequence[ToolSpec]) -> list[dict[str, Any]]:
    return [
        {
            "type": "function",
            "function": {
                "name": spec.name,
                "description": spec.description,
                "parameters": spec.input_model.model_json_schema(),
                "strict": True,
            },
        }
        for spec in tools
    ]


def _first_tool_call(output: Mapping[str, Any]) -> PlannerToolCall | None:
    tool_call = output.get("tool_call")
    if isinstance(tool_call, Mapping):
        return _tool_call_from_mapping(tool_call)
    tool_calls = output.get("tool_calls")
    if isinstance(tool_calls, Sequence) and not isinstance(tool_calls, str | bytes | bytearray):
        for item in tool_calls:
            if isinstance(item, Mapping):
                return _tool_call_from_mapping(item)
    return None


def _tool_call_from_mapping(value: Mapping[str, Any]) -> PlannerToolCall | None:
    name = value.get("name") or value.get("tool_name")
    if not isinstance(name, str) or not name:
        function = value.get("function")
        if isinstance(function, Mapping):
            function_name = function.get("name")
            if isinstance(function_name, str):
                name = function_name
    if not isinstance(name, str) or not name:
        return None
    raw_arguments = value.get("arguments")
    if raw_arguments is None:
        function = value.get("function")
        if isinstance(function, Mapping):
            raw_arguments = function.get("arguments")
    return PlannerToolCall(
        tool_name=name,
        arguments=_arguments_dict(raw_arguments),
        tool_call_id=value.get("id") if isinstance(value.get("id"), str) else None,
    )


def _arguments_dict(value: object) -> dict[str, Any]:
    if isinstance(value, Mapping):
        return dict(value)
    if not isinstance(value, str) or value == "":
        return {}
    try:
        parsed = json.loads(value)
    except json.JSONDecodeError:
        return {"_raw_arguments": value}
    if isinstance(parsed, Mapping):
        return dict(parsed)
    return {"value": parsed}


def _provider_error_respond(error: ProviderError) -> PlannerToolCall:
    return PlannerToolCall(
        tool_name="respond",
        arguments={
            "message": (
                "LLM provider 调用失败："
                f"{error.error_code}: {error.message}。请检查模型配置或稍后重试。"
            )
        },
    )
