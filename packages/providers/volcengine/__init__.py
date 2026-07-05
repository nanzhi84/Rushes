"""Volcengine provider adapters."""

from .tts import (
    VOLCENGINE_TTS_PROVIDER_ID,
    VolcengineTTSConfig,
    VolcengineTTSProvider,
    volcengine_tts_descriptor,
)

__all__ = [
    "VOLCENGINE_TTS_PROVIDER_ID",
    "VolcengineTTSConfig",
    "VolcengineTTSProvider",
    "volcengine_tts_descriptor",
]
