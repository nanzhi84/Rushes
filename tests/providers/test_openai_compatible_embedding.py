"""Embedding adapter coverage (mock transport)."""

from __future__ import annotations

import json
from typing import Any

import httpx

from providers.capabilities import EMBEDDING_TEXT, ProviderRequest
from providers.openai_compatible.embedding import (
    OpenAICompatibleEmbeddingProvider,
    openai_compatible_embedding_descriptor,
)


def _provider(handler: Any) -> OpenAICompatibleEmbeddingProvider:
    return OpenAICompatibleEmbeddingProvider(
        api_key="test-key",
        transport=httpx.MockTransport(handler),
    )


async def test_embedding_happy_path_normalizes_vector_and_usage() -> None:
    captured: list[dict[str, Any]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(json.loads(request.content.decode()))
        assert request.headers["authorization"] == "Bearer test-key"
        return httpx.Response(
            200,
            json={
                "data": [{"embedding": [0.1, 0.2, 0.3], "index": 0}],
                "model": "text-embedding-v4",
                "usage": {"prompt_tokens": 5, "total_tokens": 5},
            },
        )

    result = await _provider(handler).invoke(
        ProviderRequest(capability=EMBEDDING_TEXT, payload={"input": "产品特写"})
    )

    assert result.error is None
    assert result.normalized_output["embedding"] == [0.1, 0.2, 0.3]
    assert result.usage["total_tokens"] == 5
    assert captured[0]["input"] == "产品特写"


async def test_embedding_http_error_is_structured() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        del request
        return httpx.Response(400, json={"error": {"message": "bad input"}})

    result = await _provider(handler).invoke(
        ProviderRequest(capability=EMBEDDING_TEXT, payload={"input": "x"})
    )

    assert result.error is not None
    assert result.error.retryable is False


async def test_embedding_missing_vector_returns_empty_list_without_error() -> None:
    # adapter 契约宽松：空向量不报错，由下游投影层校验
    def handler(request: httpx.Request) -> httpx.Response:
        del request
        return httpx.Response(200, json={"data": []})

    result = await _provider(handler).invoke(
        ProviderRequest(capability=EMBEDDING_TEXT, payload={"input": "x"})
    )

    assert result.error is None
    assert result.normalized_output["embedding"] == []


async def test_embedding_network_error_is_retryable() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("boom", request=request)

    result = await _provider(handler).invoke(
        ProviderRequest(capability=EMBEDDING_TEXT, payload={"input": "x"})
    )

    assert result.error is not None
    assert result.error.retryable is True


def test_embedding_descriptor_declares_capability() -> None:
    descriptor = openai_compatible_embedding_descriptor()
    assert "embedding.text" in descriptor.capabilities
