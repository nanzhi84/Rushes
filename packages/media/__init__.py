"""Local media probing, proxy generation, ASR prep, render, and reference validation."""

from .audio_extract import AudioExtractError, ExtractedAudio, extract_audio_to_wav
from .final_mp4 import FINAL_MP4_PROFILE, render_final_mp4
from .preview import PREVIEW_PROFILE, render_preview
from .probe import MediaProbeError, probe_media
from .proxy import MediaProxyError, generate_proxy
from .render_cache import SegmentRenderCache, segment_cache_key
from .segment_render import (
    MediaSource,
    RenderProfile,
    SegmentRenderError,
    TimelineRenderOutput,
    compute_segment_cache_key,
    parse_ffmpeg_progress,
    split_timeline_segments,
)
from .vad import SileroModelMissing, VadError, VadResult, run_silero_vad

__all__ = [
    "FINAL_MP4_PROFILE",
    "PREVIEW_PROFILE",
    "AudioExtractError",
    "ExtractedAudio",
    "MediaProbeError",
    "MediaProxyError",
    "MediaSource",
    "RenderProfile",
    "SegmentRenderCache",
    "SegmentRenderError",
    "SileroModelMissing",
    "TimelineRenderOutput",
    "VadError",
    "VadResult",
    "compute_segment_cache_key",
    "extract_audio_to_wav",
    "generate_proxy",
    "parse_ffmpeg_progress",
    "probe_media",
    "render_final_mp4",
    "render_preview",
    "run_silero_vad",
    "segment_cache_key",
    "split_timeline_segments",
]
