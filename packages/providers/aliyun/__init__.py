"""Aliyun provider adapters."""

from .asr_paraformer import (
    ALIYUN_PARAFORMER_ASR_PROVIDER_ID,
    AliyunParaformerASRConfig,
    AliyunParaformerASRProvider,
    aliyun_paraformer_asr_descriptor,
)

__all__ = [
    "ALIYUN_PARAFORMER_ASR_PROVIDER_ID",
    "AliyunParaformerASRConfig",
    "AliyunParaformerASRProvider",
    "aliyun_paraformer_asr_descriptor",
]
