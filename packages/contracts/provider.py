"""Provider registry and normalized result contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

ProviderCapability = Literal[
    "llm.chat",
    "vlm.annotation",
    "embedding.text",
    "embedding.image",
    "asr.transcribe",
    "tts.speech",
    "rerank.text",
]


class ProviderDescriptor(BaseModel):
    model_config = ConfigDict(extra="forbid", arbitrary_types_allowed=True)

    provider_id: str
    display_name: str
    version: str
    capabilities: list[ProviderCapability]
    config_model: type[BaseModel]
    client_ref: str
    healthcheck_ref: str | None = None
    supports_streaming: bool = False
    supports_json_schema: bool = False
    supports_word_timestamps: bool = False
    supports_raw_transcript: bool = False
    supports_native_timestamps: bool = False
    local_only: bool = False
    priority: int = 100
    fallback_provider_ids: list[str] = Field(default_factory=list)


class ProviderError(BaseModel):
    model_config = ConfigDict(extra="forbid")

    error_code: str
    message: str
    retryable: bool = False
    details: dict[str, Any] = Field(default_factory=dict)


class ProviderResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    provider_id: str
    capability: ProviderCapability
    request_id: str
    model: str
    latency_ms: int
    usage: dict[str, Any] = Field(default_factory=dict)
    raw_ref: str | None = None
    normalized_output: dict[str, Any] = Field(default_factory=dict)
    warnings: list[str] = Field(default_factory=list)
    error: ProviderError | None = None
