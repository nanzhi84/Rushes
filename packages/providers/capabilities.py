"""Provider capability constants and adapter protocols."""

from __future__ import annotations

from typing import Any, Protocol

from pydantic import BaseModel, ConfigDict, Field

from contracts.provider import ProviderCapability, ProviderResult

LLM_CHAT: ProviderCapability = "llm.chat"
VLM_UNDERSTANDING: ProviderCapability = "vlm.understanding"
ASR_TRANSCRIBE: ProviderCapability = "asr.transcribe"
TTS_SPEECH: ProviderCapability = "tts.speech"
RERANK_TEXT: ProviderCapability = "rerank.text"

ALL_CAPABILITIES: tuple[ProviderCapability, ...] = (
    LLM_CHAT,
    VLM_UNDERSTANDING,
    ASR_TRANSCRIBE,
    TTS_SPEECH,
    RERANK_TEXT,
)


class ProviderRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    capability: ProviderCapability
    payload: dict[str, Any] = Field(default_factory=dict)
    request_id: str | None = None
    model: str | None = None
    case_id: str | None = None
    job_id: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class ProviderAdapter(Protocol):
    async def invoke(self, request: ProviderRequest) -> ProviderResult | dict[str, Any]:
        """Invoke one provider adapter and return a normalized or normalizable result."""
