"""OpenAI-compatible provider adapters."""

from .llm import (
    DEFAULT_OPENAI_COMPATIBLE_BASE_URL,
    DEFAULT_OPENAI_COMPATIBLE_MODEL,
    OPENAI_COMPATIBLE_LLM_PROVIDER_ID,
    OpenAICompatibleLLMConfig,
    OpenAICompatibleLLMProvider,
    openai_compatible_llm_descriptor,
)
from .vlm import (
    DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL,
    OPENAI_COMPATIBLE_VLM_PROVIDER_ID,
    OpenAICompatibleVLMConfig,
    OpenAICompatibleVLMProvider,
    openai_compatible_vlm_descriptor,
)

__all__ = [
    "DEFAULT_OPENAI_COMPATIBLE_BASE_URL",
    "DEFAULT_OPENAI_COMPATIBLE_MODEL",
    "DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL",
    "OPENAI_COMPATIBLE_LLM_PROVIDER_ID",
    "OPENAI_COMPATIBLE_VLM_PROVIDER_ID",
    "OpenAICompatibleLLMConfig",
    "OpenAICompatibleLLMProvider",
    "OpenAICompatibleVLMConfig",
    "OpenAICompatibleVLMProvider",
    "openai_compatible_llm_descriptor",
    "openai_compatible_vlm_descriptor",
]
