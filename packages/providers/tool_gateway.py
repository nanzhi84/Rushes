"""Default provider gateway for agent tool execution.

Agent 链路里的工具（media.view_frames / memory.extract_from_case /
content.create_plan / retrieval 向量检索）都经 ToolExecutionContext.metadata
的 provider_gateway 调 provider；缺了它全部走降级路径（M9 实测）。
"""

from __future__ import annotations

import os

from .gateway import ProviderCallRecorder, ProviderGateway
from .openai_compatible import (
    OpenAICompatibleEmbeddingProvider,
    OpenAICompatibleLLMProvider,
    OpenAICompatibleVLMProvider,
    openai_compatible_embedding_descriptor,
    openai_compatible_llm_descriptor,
    openai_compatible_vlm_descriptor,
)
from .registry import ProviderRegistry


def build_default_tool_gateway(
    *,
    recorder: ProviderCallRecorder | None = None,
) -> ProviderGateway | None:
    """按环境变量构造 LLM/VLM/embedding 全能力 gateway；无任何 key 时返回 None。"""

    dashscope_key = os.environ.get("RUSHES_DASHSCOPE_API_KEY")
    llm_key = os.environ.get("RUSHES_LLM_API_KEY") or dashscope_key
    vlm_key = os.environ.get("RUSHES_VLM_API_KEY") or dashscope_key
    embedding_key = os.environ.get("RUSHES_EMBEDDING_API_KEY") or dashscope_key
    if not any((llm_key, vlm_key, embedding_key)):
        return None
    registry = ProviderRegistry()
    if llm_key:
        model = os.environ.get("RUSHES_LLM_MODEL")
        provider = (
            OpenAICompatibleLLMProvider(api_key=llm_key, model=model)
            if model
            else OpenAICompatibleLLMProvider(api_key=llm_key)
        )
        registry.register(openai_compatible_llm_descriptor(), provider)
    if vlm_key:
        vlm_model = os.environ.get("RUSHES_VLM_MODEL")
        vlm_provider = (
            OpenAICompatibleVLMProvider(api_key=vlm_key, model=vlm_model)
            if vlm_model
            else OpenAICompatibleVLMProvider(api_key=vlm_key)
        )
        registry.register(openai_compatible_vlm_descriptor(), vlm_provider)
    if embedding_key:
        registry.register(
            openai_compatible_embedding_descriptor(),
            OpenAICompatibleEmbeddingProvider(api_key=embedding_key),
        )
    return ProviderGateway(registry=registry, recorder=recorder)
