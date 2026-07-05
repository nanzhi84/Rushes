"""OpenAI-compatible provider adapters."""

from .llm import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    DEFAULT_OPENAI_COMPATIBLE_MODEL,
    OPENAI_COMPATIBLE_LLM_PROVIDER_ID,
    OpenAICompatibleLLMConfig,
    OpenAICompatibleLLMProvider,
    openai_compatible_llm_descriptor,
)

__all__ = [
    "DEFAULT_OPENAI_COMPATIBLE_BASE_URL",
    "DEFAULT_OPENAI_COMPATIBLE_MODEL",
    "OPENAI_COMPATIBLE_LLM_PROVIDER_ID",
    "OpenAICompatibleLLMConfig",
    "OpenAICompatibleLLMProvider",
    "openai_compatible_llm_descriptor",
]
