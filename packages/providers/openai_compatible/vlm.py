"""OpenAI-compatible multimodal adapter for vlm.understanding."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

import httpx
from pydantic import BaseModel, ConfigDict

from contracts.provider import ProviderDescriptor
from providers.capabilities import VLM_UNDERSTANDING

from .llm import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    OpenAICompatibleLLMProvider,
)

DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL = "qwen-vl-plus"
OPENAI_COMPATIBLE_VLM_PROVIDER_ID = "openai_compatible.vlm"


class OpenAICompatibleVLMConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")

    base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL
    api_key_env: str = "RUSHES_VLM_API_KEY"
    model: str = DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL
    priority: int = 100


class OpenAICompatibleVLMProvider(OpenAICompatibleLLMProvider):
    """Same chat-completions wire protocol, registered for multimodal understanding."""

    def __init__(
        self,
        *,
        base_url: str = DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
        api_key: str | None = None,
        model: str = DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL,
        timeout: float | httpx.Timeout = 60.0,
        default_params: Mapping[str, Any] | None = None,
        max_retries: int = 2,
        retry_base_delay_seconds: float = 0.1,
        transport: httpx.AsyncBaseTransport | None = None,
        force_ipv4: bool = True,
    ) -> None:
        params = {"response_format": {"type": "json_object"}, **dict(default_params or {})}
        super().__init__(
            base_url=base_url,
            api_key=api_key,
            model=model,
            timeout=timeout,
            default_params=params,
            max_retries=max_retries,
            retry_base_delay_seconds=retry_base_delay_seconds,
            transport=transport,
            force_ipv4=force_ipv4,
        )
        self.provider_id = OPENAI_COMPATIBLE_VLM_PROVIDER_ID


def openai_compatible_vlm_descriptor(*, priority: int = 100) -> ProviderDescriptor:
    return ProviderDescriptor(
        provider_id=OPENAI_COMPATIBLE_VLM_PROVIDER_ID,
        display_name="OpenAI-compatible Multimodal Understanding",
        version="1",
        capabilities=[VLM_UNDERSTANDING],
        config_model=OpenAICompatibleVLMConfig,
        client_ref="providers.openai_compatible.vlm.OpenAICompatibleVLMProvider",
        supports_json_schema=True,
        priority=priority,
    )
