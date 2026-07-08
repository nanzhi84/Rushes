"""Default provider gateway for agent tool execution.

Agent 链路里的工具（media.view_frames / understand.materials 的理解子代理 /
memory.extract_from_draft / content.create_plan）都经 ToolExecutionContext.metadata
的 provider_gateway 调 provider；缺了它全部走降级路径（M9 实测）。
"""

from __future__ import annotations

import os

from .gateway import ProviderCallRecorder, ProviderGateway
from .openai_compatible import (
    OpenAICompatibleLLMProvider,
    OpenAICompatibleVLMProvider,
    openai_compatible_llm_descriptor,
    openai_compatible_vlm_descriptor,
)
from .registry import ProviderRegistry


def build_default_tool_gateway(
    *,
    recorder: ProviderCallRecorder | None = None,
) -> ProviderGateway | None:
    """按环境变量构造 LLM/VLM 全能力 gateway；无任何 key 时返回 None。"""

    dashscope_key = os.environ.get("RUSHES_DASHSCOPE_API_KEY")
    llm_key = os.environ.get("RUSHES_LLM_API_KEY") or dashscope_key
    vlm_key = os.environ.get("RUSHES_VLM_API_KEY") or dashscope_key
    if not any((llm_key, vlm_key)):
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
        # 推理型多模态模型（qwen3.7-plus）非流式整包返回常超 60s，超时默认放宽到
        # 180s（RUSHES_VLM_TIMEOUT_S 可覆盖），否则理解子代理会高频撞超时重试。
        vlm_timeout = _vlm_timeout_seconds()
        vlm_provider = (
            OpenAICompatibleVLMProvider(api_key=vlm_key, model=vlm_model, timeout=vlm_timeout)
            if vlm_model
            else OpenAICompatibleVLMProvider(api_key=vlm_key, timeout=vlm_timeout)
        )
        registry.register(openai_compatible_vlm_descriptor(), vlm_provider)
    return ProviderGateway(registry=registry, recorder=recorder)


_DEFAULT_VLM_TIMEOUT_SECONDS = 180.0


def _vlm_timeout_seconds() -> float:
    raw = os.environ.get("RUSHES_VLM_TIMEOUT_S")
    if raw is None:
        return _DEFAULT_VLM_TIMEOUT_SECONDS
    try:
        value = float(raw)
    except ValueError:
        return _DEFAULT_VLM_TIMEOUT_SECONDS
    return value if value > 0 else _DEFAULT_VLM_TIMEOUT_SECONDS
