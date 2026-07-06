"""OpenAI-compatible chat completions adapter for the llm.chat capability."""

from __future__ import annotations

import asyncio
import json
import time
from collections.abc import Callable, Mapping, Sequence
from typing import Any, cast
from uuid import uuid4

import httpx
from pydantic import BaseModel, ConfigDict

from contracts.provider import ProviderDescriptor, ProviderError, ProviderResult
from providers.capabilities import LLM_CHAT, ProviderRequest

DEFAULT_OPENAI_COMPATIBLE_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
DEFAULT_OPENAI_COMPATIBLE_MODEL = "qwen-plus"
OPENAI_COMPATIBLE_LLM_PROVIDER_ID = "openai_compatible.llm"
_CHAT_COMPLETIONS_PATH = "chat/completions"
_DEFAULT_MAX_RETRIES = 2


class OpenAICompatibleLLMConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL
    api_key_env: str = "RUSHES_LLM_API_KEY"
    model: str = DEFAULT_OPENAI_COMPATIBLE_MODEL
    priority: int = 100


class OpenAICompatibleLLMProvider:
    """Invoke an OpenAI-compatible ``/chat/completions`` endpoint."""

    def __init__(
        self,
        *,
        base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
        api_key: str | None = None,
        model: str = DEFAULT_OPENAI_COMPATIBLE_MODEL,
        timeout: float | httpx.Timeout = 60.0,
        default_params: Mapping[str, Any] | None = None,
        max_retries: int = _DEFAULT_MAX_RETRIES,
        retry_base_delay_seconds: float = 0.1,
        transport: httpx.AsyncBaseTransport | None = None,
        force_ipv4: bool = True,
    ) -> None:
        self.provider_id = OPENAI_COMPATIBLE_LLM_PROVIDER_ID
        self._base_url = base_url.rstrip("/") + "/"
        self._api_key = api_key
        self._model = model
        self._timeout = timeout
        self._default_params = dict(default_params or {})
        self._max_retries = max(0, max_retries)
        self._retry_base_delay_seconds = max(0.0, retry_base_delay_seconds)
        # 本地网络下 IPv6 直连国内端点会在 TLS 握手被重置（实测），
        # local_address 绑 0.0.0.0 强制 IPv4；传入自定义 transport 时不干预。
        if transport is None and force_ipv4:
            transport = httpx.AsyncHTTPTransport(local_address="0.0.0.0")
        self._transport = transport

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or f"openai_compatible_{uuid4().hex}"
        model = request.model or self._model
        body = self._request_body(request, model=model)
        headers = {"Content-Type": "application/json"}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"

        last_error: ProviderError | None = None
        async with httpx.AsyncClient(
            base_url=self._base_url,
            headers=headers,
            timeout=self._timeout,
            trust_env=False,
            transport=self._transport,
        ) as client:
            attempt = 0
            while attempt <= self._max_retries:
                try:
                    response = await client.post(_CHAT_COMPLETIONS_PATH, json=body)
                except httpx.TimeoutException as exc:
                    last_error = _transport_error("timeout", exc, retryable=True)
                    if not _should_retry_error(last_error, attempt, self._max_retries):
                        return self._error_result(request, request_id, model, started, last_error)
                    await self._sleep_before_retry(attempt)
                    attempt += 1
                    continue
                except httpx.TransportError as exc:
                    last_error = _transport_error("network_error", exc, retryable=True)
                    if not _should_retry_error(last_error, attempt, self._max_retries):
                        return self._error_result(request, request_id, model, started, last_error)
                    await self._sleep_before_retry(attempt)
                    attempt += 1
                    continue

                if response.status_code >= 400:
                    last_error = _http_error(response)
                    if not _should_retry_error(last_error, attempt, self._max_retries):
                        return self._error_result(request, request_id, model, started, last_error)
                    await self._sleep_before_retry(attempt)
                    attempt += 1
                    continue

                return self._success_result(
                    request,
                    request_id=request_id,
                    model=model,
                    started=started,
                    payload=_response_json(response),
                )

        if last_error is None:
            last_error = ProviderError(
                error_code="retry_exhausted",
                message="provider retry loop exhausted without a response",
                retryable=True,
            )
        return self._error_result(request, request_id, model, started, last_error)

    async def invoke_stream(
        self,
        request: ProviderRequest,
        *,
        on_delta: Callable[[Mapping[str, Any]], None],
    ) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or f"openai_compatible_{uuid4().hex}"
        model = request.model or self._model
        body = self._request_body(request, model=model)
        # 流式：让服务端逐块吐 token，并在末尾附带 usage 统计块。
        body["stream"] = True
        body["stream_options"] = {"include_usage": True}
        headers = {"Content-Type": "application/json"}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"

        accumulator = _StreamAccumulator()
        # 流式不做中途重试：一旦连上就单次消费，失败即返回 error result，
        # 重试/降级语义交给上层（loop 会退回非流式 invoke）。
        async with httpx.AsyncClient(
            base_url=self._base_url,
            headers=headers,
            timeout=self._timeout,
            trust_env=False,
            transport=self._transport,
        ) as client:
            try:
                async with client.stream("POST", _CHAT_COMPLETIONS_PATH, json=body) as response:
                    if response.status_code >= 400:
                        await response.aread()
                        error = _http_error(response)
                        return self._error_result(request, request_id, model, started, error)
                    async for line in response.aiter_lines():
                        data = _decode_sse_data(line)
                        if data is None:
                            continue
                        if data == "[DONE]":
                            break
                        # on_delta 回调抛出的异常有意向上传播（不吞），由调用方自行处理。
                        accumulator.consume(data, on_delta)
            except httpx.TimeoutException as exc:
                error = _transport_error("timeout", exc, retryable=True)
                return self._error_result(request, request_id, model, started, error)
            except httpx.TransportError as exc:
                error = _transport_error("network_error", exc, retryable=True)
                return self._error_result(request, request_id, model, started, error)

        return self._success_result(
            request,
            request_id=request_id,
            model=model,
            started=started,
            payload=accumulator.as_chat_payload(fallback_id=request_id, fallback_model=model),
        )

    def _request_body(self, request: ProviderRequest, *, model: str) -> dict[str, Any]:
        payload = request.payload
        messages = _message_list(payload.get("messages"))
        body: dict[str, Any] = {
            "model": model,
            "messages": messages,
        }
        tools = _openai_tools(payload.get("tools"))
        if tools:
            body["tools"] = tools
        tool_choice = payload.get("tool_choice")
        if tool_choice is not None:
            body["tool_choice"] = tool_choice
        body.update(self._default_params)
        params = payload.get("params")
        if isinstance(params, Mapping):
            body.update(dict(params))
        return body

    def _success_result(
        self,
        request: ProviderRequest,
        *,
        request_id: str,
        model: str,
        started: float,
        payload: Mapping[str, Any],
    ) -> ProviderResult:
        normalized, usage, warnings = _normalize_chat_response(payload)
        response_model = _string_or_default(payload.get("model"), model)
        response_id = _string_or_default(payload.get("id"), request_id)
        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=request.request_id or response_id,
            model=response_model,
            latency_ms=_elapsed_ms(started),
            usage=usage,
            raw_ref=response_id,
            normalized_output=normalized,
            warnings=warnings,
        )

    def _error_result(
        self,
        request: ProviderRequest,
        request_id: str,
        model: str,
        started: float,
        error: ProviderError,
    ) -> ProviderResult:
        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=request_id,
            model=model,
            latency_ms=_elapsed_ms(started),
            error=error,
        )

    async def _sleep_before_retry(self, attempt: int) -> None:
        if self._retry_base_delay_seconds == 0:
            return
        await asyncio.sleep(self._retry_base_delay_seconds * (2**attempt))


def openai_compatible_llm_descriptor(*, priority: int = 100) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=OPENAI_COMPATIBLE_LLM_PROVIDER_ID,
        display_name="OpenAI-compatible Chat Completions",
        version="1",
        capabilities=[LLM_CHAT],
        config_model=OpenAICompatibleLLMConfig,
        client_ref="providers.openai_compatible.llm.OpenAICompatibleLLMProvider",
        supports_json_schema=True,
        priority=priority,
    )


def _message_list(value: object) -> list[dict[str, Any]]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
        return []
    messages: list[dict[str, Any]] = []
    for item in value:
        if isinstance(item, Mapping):
            messages.append(dict(item))
    return messages


def _openai_tools(value: object) -> list[dict[str, Any]]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
        return []
    tools: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, Mapping):
            continue
        tools.append(_openai_tool(item))
    return tools


def _openai_tool(tool: Mapping[str, Any]) -> dict[str, Any]:
    if tool.get("type") == "function" and isinstance(tool.get("function"), Mapping):
        function = dict(cast(Mapping[str, Any], tool["function"]))
        function["strict"] = True
        return {"type": "function", "function": function}
    parameters = tool.get("parameters")
    if not isinstance(parameters, Mapping):
        parameters = {}
    description = tool.get("description")
    return {
        "type": "function",
        "function": {
            "name": str(tool.get("name", "")),
            "description": str(description) if description is not None else "",
            "parameters": dict(parameters),
            "strict": True,
        },
    }


def _response_json(response: httpx.Response) -> Mapping[str, Any]:
    try:
        payload = response.json()
    except ValueError:
        return {}
    if isinstance(payload, Mapping):
        return payload
    return {}


def _decode_sse_data(line: str) -> str | None:
    """Extract the payload of a ``data:`` SSE line; ignore blanks/comments."""
    stripped = line.strip()
    if not stripped.startswith("data:"):
        return None
    return stripped[len("data:") :].strip()


class _StreamAccumulator:
    """Fold OpenAI SSE delta chunks back into a non-streaming chat payload."""

    def __init__(self) -> None:
        self._content: list[str] = []
        self._tool_calls: dict[int, dict[str, Any]] = {}
        self._usage: dict[str, Any] = {}
        self._finish_reason: str | None = None
        self._response_id: str | None = None
        self._response_model: str | None = None

    def consume(self, data: str, on_delta: Callable[[Mapping[str, Any]], None]) -> None:
        try:
            chunk = json.loads(data)
        except json.JSONDecodeError:
            return
        if not isinstance(chunk, Mapping):
            return
        chunk_id = chunk.get("id")
        if isinstance(chunk_id, str) and chunk_id:
            self._response_id = chunk_id
        chunk_model = chunk.get("model")
        if isinstance(chunk_model, str) and chunk_model:
            self._response_model = chunk_model
        usage = chunk.get("usage")
        if isinstance(usage, Mapping):
            self._usage = dict(usage)
        choice = _first_choice(chunk.get("choices"))
        if not choice:
            return
        finish_reason = choice.get("finish_reason")
        if isinstance(finish_reason, str):
            self._finish_reason = finish_reason
        delta = choice.get("delta")
        if not isinstance(delta, Mapping):
            return
        content = delta.get("content")
        if isinstance(content, str) and content:
            self._content.append(content)
            on_delta({"type": "text", "text": content})
        self._accumulate_tool_calls(delta.get("tool_calls"))

    def _accumulate_tool_calls(self, value: object) -> None:
        if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
            return
        for item in value:
            if not isinstance(item, Mapping):
                continue
            index = item.get("index")
            if not isinstance(index, int):
                index = len(self._tool_calls)
            entry = self._tool_calls.setdefault(
                index, {"id": None, "type": "function", "name": "", "arguments": ""}
            )
            call_id = item.get("id")
            if isinstance(call_id, str) and call_id:
                entry["id"] = call_id
            call_type = item.get("type")
            if isinstance(call_type, str) and call_type:
                entry["type"] = call_type
            function = item.get("function")
            if isinstance(function, Mapping):
                name = function.get("name")
                if isinstance(name, str):
                    entry["name"] += name
                arguments = function.get("arguments")
                if isinstance(arguments, str):
                    entry["arguments"] += arguments

    def as_chat_payload(self, *, fallback_id: str, fallback_model: str) -> dict[str, Any]:
        message: dict[str, Any] = {"role": "assistant", "content": "".join(self._content)}
        tool_calls = [
            {
                "id": entry["id"],
                "type": entry["type"],
                "function": {"name": entry["name"], "arguments": entry["arguments"]},
            }
            for _, entry in sorted(self._tool_calls.items())
        ]
        if tool_calls:
            message["tool_calls"] = tool_calls
        return {
            "id": self._response_id or fallback_id,
            "model": self._response_model or fallback_model,
            "choices": [{"message": message, "finish_reason": self._finish_reason}],
            "usage": self._usage,
        }


def _normalize_chat_response(
    payload: Mapping[str, Any],
) -> tuple[dict[str, Any], dict[str, Any], list[str]]:
    raw_usage = payload.get("usage")
    usage = dict(raw_usage) if isinstance(raw_usage, Mapping) else {}
    choice = _first_choice(payload.get("choices"))
    message = choice.get("message") if isinstance(choice.get("message"), Mapping) else {}
    content = _content_text(message.get("content") if isinstance(message, Mapping) else None)
    raw_tool_calls = message.get("tool_calls") if isinstance(message, Mapping) else None
    tool_calls = _normalize_tool_calls(raw_tool_calls)
    warnings: list[str] = []
    if len(tool_calls) > 1:
        warnings.append("parallel tool calls truncated")
        tool_calls = tool_calls[:1]
    finish_reason = choice.get("finish_reason")
    normalized = {
        "content": content,
        "tool_calls": tool_calls,
        "finish_reason": finish_reason if isinstance(finish_reason, str) else None,
        "usage": usage,
    }
    if tool_calls:
        normalized["tool_call"] = tool_calls[0]
    return normalized, usage, warnings


def _first_choice(value: object) -> dict[str, Any]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray) or not value:
        return {}
    first = value[0]
    return dict(first) if isinstance(first, Mapping) else {}


def _content_text(value: object) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, sort_keys=True)


def _normalize_tool_calls(value: object) -> list[dict[str, Any]]:
    if not isinstance(value, Sequence) or isinstance(value, str | bytes | bytearray):
        return []
    tool_calls: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, Mapping):
            continue
        function = item.get("function")
        if not isinstance(function, Mapping):
            continue
        name = function.get("name")
        if not isinstance(name, str):
            continue
        tool_calls.append(
            {
                "id": item.get("id") if isinstance(item.get("id"), str) else None,
                "type": item.get("type") if isinstance(item.get("type"), str) else "function",
                "name": name,
                "arguments": _arguments_dict(function.get("arguments")),
            }
        )
    return tool_calls


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


def _http_error(response: httpx.Response) -> ProviderError:
    status = response.status_code
    retryable = status == 429 or 500 <= status <= 599
    return ProviderError(
        error_code=f"http_status_{status}",
        message=_error_message(response),
        retryable=retryable,
        details={"status_code": status},
    )


def _error_message(response: httpx.Response) -> str:
    try:
        payload = response.json()
    except ValueError:
        return response.text[:500]
    if isinstance(payload, Mapping):
        error = payload.get("error")
        if isinstance(error, Mapping):
            message = error.get("message")
            if isinstance(message, str):
                return message
        message = payload.get("message")
        if isinstance(message, str):
            return message
    return response.text[:500]


def _transport_error(error_code: str, exc: httpx.HTTPError, *, retryable: bool) -> ProviderError:
    return ProviderError(
        error_code=error_code,
        message=str(exc),
        retryable=retryable,
        details={"exception_type": type(exc).__name__},
    )


def _should_retry_error(error: ProviderError, attempt: int, max_retries: int) -> bool:
    return error.retryable and attempt < max_retries


def _string_or_default(value: object, default: str) -> str:
    return value if isinstance(value, str) and value else default


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))
