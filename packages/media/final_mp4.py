"""Final MP4 render profile and entrypoint."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import replace
from pathlib import Path

from contracts.timeline import TimelineState
from storage.workspace_paths import WorkspacePaths

from .render_cache import DEFAULT_MAX_BYTES, SegmentRenderCache
from .segment_render import (
    MediaSource,
    ProgressCallback,
    RenderProfile,
    TimelineRenderOutput,
    render_timeline_to_file,
)

FINAL_MP4_PROFILE = RenderProfile(
    name="final_mp4",
    width=1080,
    height=1920,
    fps=30,
    preset="slow",
    crf=18,
    video_bitrate="8M",
    audio_bitrate="192k",
)


async def render_final_mp4(
    timeline: TimelineState,
    *,
    sources: Mapping[str, MediaSource],
    paths: WorkspacePaths,
    output_path: Path,
    ffmpeg_bin: str = "ffmpeg",
    ffmpeg_version: str | None = None,
    cache: SegmentRenderCache | None = None,
    cache_max_bytes: int = DEFAULT_MAX_BYTES,
    progress_callback: ProgressCallback | None = None,
) -> TimelineRenderOutput:
    return await render_timeline_to_file(
        timeline,
        sources=sources,
        paths=paths,
        profile=replace(FINAL_MP4_PROFILE, fps=timeline.fps),
        output_path=output_path,
        ffmpeg_bin=ffmpeg_bin,
        ffmpeg_version=ffmpeg_version,
        cache=cache,
        cache_max_bytes=cache_max_bytes,
        progress_callback=progress_callback,
    )
