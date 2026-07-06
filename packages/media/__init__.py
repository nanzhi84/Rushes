"""Local media probing, proxy generation, ASR prep, render, and reference validation."""

from .audio_extract import AudioExtractError, ExtractedAudio, extract_audio_to_wav
from .final_mp4 import FINAL_MP4_PROFILE, render_final_mp4
from .font_meta import FontMeta, FontMetaError, read_font_meta
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
from .shots import Shot, ShotSplitConfig, ShotSplitError, split_shots
from .thumbnails import ThumbnailError, extract_video_thumbnail, render_image_thumbnail
from .vad import SileroModelMissing, VadError, VadResult, run_silero_vad
from .waveform import WaveformError, compute_waveform_peaks, downsample_peaks

__all__ = [
    "FINAL_MP4_PROFILE",
    "PREVIEW_PROFILE",
    "AudioExtractError",
    "ExtractedAudio",
    "FontMeta",
    "FontMetaError",
    "MediaProbeError",
    "MediaProxyError",
    "MediaSource",
    "RenderProfile",
    "SegmentRenderCache",
    "SegmentRenderError",
    "Shot",
    "ShotSplitConfig",
    "ShotSplitError",
    "SileroModelMissing",
    "ThumbnailError",
    "TimelineRenderOutput",
    "VadError",
    "VadResult",
    "WaveformError",
    "compute_segment_cache_key",
    "compute_waveform_peaks",
    "downsample_peaks",
    "extract_audio_to_wav",
    "extract_video_thumbnail",
    "generate_proxy",
    "parse_ffmpeg_progress",
    "probe_media",
    "read_font_meta",
    "render_final_mp4",
    "render_image_thumbnail",
    "render_preview",
    "run_silero_vad",
    "segment_cache_key",
    "split_shots",
    "split_timeline_segments",
]
