"""OpenAI-compatible provider adapters."""

from .embedding import (
    DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_MODEL,
    OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID,
    OpenAICompatibleEmbeddingConfig,
    OpenAICompatibleEmbeddingProvider,
    openai_compatible_embedding_descriptor,
)
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
    "DEFAULT_OPENAI_COMPATIBLE_EMBEDDING_MODEL",
    "DEFAULT_OPENAI_COMPATIBLE_MODEL",
    "DEFAULT_OPENAI_COMPATIBLE_VLM_MODEL",
    "OPENAI_COMPATIBLE_EMBEDDING_PROVIDER_ID",
    "OPENAI_COMPATIBLE_LLM_PROVIDER_ID",
    "OPENAI_COMPATIBLE_VLM_PROVIDER_ID",
    "OpenAICompatibleEmbeddingConfig",
    "OpenAICompatibleEmbeddingProvider",
    "OpenAICompatibleLLMConfig",
    "OpenAICompatibleLLMProvider",
    "OpenAICompatibleVLMConfig",
    "OpenAICompatibleVLMProvider",
    "openai_compatible_embedding_descriptor",
    "openai_compatible_llm_descriptor",
    "openai_compatible_vlm_descriptor",
]
