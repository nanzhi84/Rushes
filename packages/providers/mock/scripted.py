"""Scriptable in-memory mock provider for tests and golden replay."""

from __future__ import annotations

from collections import deque
from collections.abc import Callable, Mapping, Sequence
from typing import Any

from contracts.provider import ProviderCapability, ProviderError, ProviderResult
from providers.capabilities import ProviderRequest

_STREAM_CHUNK_SIZE = 8


class MockProvider:
    def __init__(
        self,
        *,
        provider_id: str = "mock",
        model: str = "mock-model",
        scripts: Mapping[ProviderCapability, Sequence[dict[str, Any]]] | None = None,
    ) -> None:
        self.provider_id = provider_id
        self.model = model
        self._scripts: dict[ProviderCapability, deque[dict[str, Any]]] = {
            capability: deque(dict(item) for item in items)
            for capability, items in (scripts or {}).items()
        }

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        script = self._scripts.setdefault(request.capability, deque())
        if not script:
            return ProviderResult(
                provider_id=self.provider_id,
                capability=request.capability,
                request_id=request.request_id or "mock_request",
                model=request.model or self.model,
                latency_ms=0,
                error=ProviderError(
                    error_code="mock_script_exhausted",
                    message=f"no scripted response for {request.capability}",
                    retryable=False,
                ),
            )
        item = script.popleft()
        if "raise" in item:
            raise RuntimeError(str(item["raise"]))
        item = _expand_script_item(item)
        data = {
            "provider_id": self.provider_id,
            "capability": request.capability,
            "request_id": request.request_id or "mock_request",
            "model": request.model or self.model,
            "latency_ms": 0,
            "usage": {},
            "normalized_output": {},
            "warnings": [],
            "raw_ref": None,
            "error": None,
            **item,
        }
        return ProviderResult.model_validate(data)

    async def invoke_stream(
        self,
        request: ProviderRequest,
        *,
        on_delta: Callable[[Mapping[str, Any]], None],
    ) -> ProviderResult:
        # 消费同一脚本项后，把 content 按固定 8 字符分片回调，再返回终值——
        # 供 golden/流式测试对齐真实 provider 的 delta 形态。
        result = await self.invoke(request)
        content = result.normalized_output.get("content")
        if isinstance(content, str) and content:
            for start in range(0, len(content), _STREAM_CHUNK_SIZE):
                on_delta({"type": "text", "text": content[start : start + _STREAM_CHUNK_SIZE]})
        return result

    def remaining(self, capability: ProviderCapability) -> int:
        return len(self._scripts.get(capability, ()))


def _expand_script_item(item: dict[str, Any]) -> dict[str, Any]:
    """Fold ``content``/``tool_call`` shorthands into ``normalized_output``."""

    if "content" not in item and "tool_call" not in item:
        return item
    normalized = dict(item.get("normalized_output") or {})
    if "content" in item:
        normalized["content"] = item["content"]
    if "tool_call" in item:
        normalized["tool_call"] = item["tool_call"]
    expanded = {key: value for key, value in item.items() if key not in {"content", "tool_call"}}
    expanded["normalized_output"] = normalized
    return expanded
