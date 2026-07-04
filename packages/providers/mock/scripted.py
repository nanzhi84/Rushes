"""Scriptable in-memory mock provider for tests and golden replay."""

from __future__ import annotations

from collections import deque
from collections.abc import Mapping, Sequence
from typing import Any

from contracts.provider import ProviderCapability, ProviderError, ProviderResult
from providers.capabilities import ProviderRequest


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
        if "tool_call" in item:
            item = {
                "normalized_output": {"tool_call": item["tool_call"]},
                "usage": item.get("usage", {}),
            }
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

    def remaining(self, capability: ProviderCapability) -> int:
        return len(self._scripts.get(capability, ()))
