"""OpenAI-compatible embeddings adapter for embedding.text."""

from __future__ import annotations

import time
from collections.abc import Mapping
from typing import Any
from uuid import uuid4

import httpx
from pydantic import BaseModel, ConfigDict

from contracts.provider import ProviderDescriptor, ProviderError, ProviderResult
from providers.capabilities import EMBEDDING_TEXT, ProviderRequest

DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_MODEL = "text-embedding-v4"
OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID = "openai_compatible.embedding"
_EMBEDDINGS_PATH = "embeddings"


class OpenAICompatibleEmbeddingConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    base_url: str = DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_BASE_URL
    api_key_env: str = "RUSHES_EMBEDDING_API_KEY"
    model: str = DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_MODEL
    priority: int = 100


class OpenAICompatibleEmbeddingProvider:
    def __init__(
        self,
        *,
        base_url: str = DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_BASE_URL,
        api_key: str | None = None,
        model: str = DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_MODEL,
        timeout: float | httpx.Timeout = 60.0,
        transport: httpx.AsyncBaseTransport | None = None,
        force_ipv4: bool = True,
    ) -> None:
        self.provider_id = OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID
        self._base_url = base_url.rstrip("/") + "/"
        self._api_key = api_key
        self._model = model
        self._timeout = timeout
        if transport is None and force_ipv4:
            transport = httpx.AsyncHTTPTransport(local_address="0.0.0.0")
        self._transport = transport

    async def invoke(self, request: ProviderRequest) -> ProviderResult:
        started = time.monotonic()
        request_id = request.request_id or f"openai_embedding_{uuid4().hex}"
        model = request.model or self._model
        headers = {"Content-Type": "application/json"}
        if self._api_key:
            headers["Authorization"] = f"Bearer {self._api_key}"
        async with httpx.AsyncClient(
            base_url=self._base_url,
            headers=headers,
            timeout=self._timeout,
            trust_env=False,
            transport=self._transport,
        ) as client:
            try:
                response = await client.post(
                    _EMBEDDINGS_PATH,
                    json={"model": model, "input": _embedding_input(request.payload)},
                )
            except httpx.TimeoutException as exc:
                return _error_result(request, request_id, model, started, "timeout", exc)
            except httpx.TransportError as exc:
                return _error_result(request, request_id, model, started, "network_error", exc)
        if response.status_code >= 400:
            return ProviderResult(
                provider_id=self.provider_id,
                capability=request.capability,
                request_id=request_id,
                model=model,
                latency_ms=_elapsed_ms(started),
                error=ProviderError(
                    error_code=f"http_status_{response.status_code}",
                    message=response.text[:500],
                    retryable=response.status_code == 429 or response.status_code >= 500,
                    details={"status_code": response.status_code},
                ),
            )
        payload = _response_json(response)
        normalized = {
            "data": payload.get("data", []),
            "embedding": _first_embedding(payload),
        }
        raw_usage = payload.get("usage")
        usage = dict(raw_usage) if isinstance(raw_usage, Mapping) else {}
        return ProviderResult(
            provider_id=self.provider_id,
            capability=request.capability,
            request_id=str(payload.get("id") or request_id),
            model=str(payload.get("model") or model),
            latency_ms=_elapsed_ms(started),
            usage=usage,
            raw_ref=str(payload.get("id") or request_id),
            normalized_output=normalized,
        )


def openai_compatible_embedding_descriptor(*, priority: int = 100) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID,
        display_name="OpenAI-compatible Text Embeddings",
        version="1",
        capabilities=[EMBEDDING_TEXT],
        config_model=OpenAICompatibleEmbeddingConfig,
        client_ref="providers.openai_compatible.embedding.OpenAICompatibleEmbeddingProvider",
        priority=priority,
    )


def _embedding_input(payload: Mapping[str, Any]) -> str | list[str]:
    value = payload.get("input") or payload.get("retrieval_sentence") or ""
    if isinstance(value, list):
        return [str(item) for item in value]
    return str(value)


def _first_embedding(payload: Mapping[str, Any]) -> list[float]:
    data = payload.get("data")
    if not isinstance(data, list) or not data:
        return []
    first = data[0]
    if not isinstance(first, Mapping):
        return []
    embedding = first.get("embedding")
    if not isinstance(embedding, list):
        return []
    return [float(item) for item in embedding]


def _response_json(response: httpx.Response) -> Mapping[str, Any]:
    try:
        payload = response.json()
    except ValueError:
        return {}
    return dict(payload) if isinstance(payload, Mapping) else {}


def _error_result(
    request: ProviderRequest,
    request_id: str,
    model: str,
    started: float,
    code: str,
    exc: httpx.HTTPError,
) -> ProviderResult:
    return ProviderResult(
        provider_id=OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID,
        capability=request.capability,
        request_id=request_id,
        model=model,
        latency_ms=_elapsed_ms(started),
        error=ProviderError(
            error_code=code,
            message=str(exc),
            retryable=True,
            details={"exception_type": type(exc).__name__},
        ),
    )


def _elapsed_ms(started: float) -> int:
    return max(0, int((time.monotonic() - started) * 1000))
