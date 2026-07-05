"""Frame-accurate visual segment rendering for TimelineState."""

from __future__ import annotations

import asyncio
import inspect
import itertools
import json
import re
import subprocess
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from uuid import uuid4

from contracts.subtitle import SubtitleClip
from contracts.timeline import TimelineMediaClip, TimelineState
from storage.workspace_paths import WorkspacePaths

from .render_cache import DEFAULT_MAX_BYTES, SegmentRenderCache, segment_cache_key
from .subtitles_ass import (
    SubtitleTemplateMap,
    resolve_subtitle_templates,
    subtitle_cache_payload,
    write_segment_ass,
)

type ProgressCallback = Callable[[float], Awaitable[None] | None]


class SegmentRenderError(RuntimeError):
    """Raised when ffmpeg cannot render a timeline segment."""

    def __init__(self, message: str, *, stderr_summary: str | None = None) -> None:
        super().__init__(message)
        self.stderr_summary = stderr_summary


@dataclass(frozen=True, slots=True)
class RenderProfile:
    name: str
    width: int
    height: int
    fps: int
    preset: str
    crf: int | None = None
    video_bitrate: str | None = None
    audio_bitrate: str = "160k"
    loudnorm_i: float = -16.0
    loudnorm_tp: float = -1.5
    loudnorm_lra: float = 11.0

    def cache_payload(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "width": self.width,
            "height": self.height,
            "fps": self.fps,
            "preset": self.preset,
            "crf": self.crf,
            "video_bitrate": self.video_bitrate,
            "audio_bitrate": self.audio_bitrate,
            "loudnorm_i": self.loudnorm_i,
            "loudnorm_tp": self.loudnorm_tp,
            "loudnorm_lra": self.loudnorm_lra,
        }


@dataclass(frozen=True, slots=True)
class MediaSource:
    asset_id: str
    path: Path
    asset_hash: str
    kind: str = "video"


@dataclass(frozen=True, slots=True)
class SegmentClip:
    timeline_clip_id: str
    track_id: str
    asset_id: str
    clip_id: str | None
    timeline_start_frame: int
    timeline_end_frame: int
    source_start_frame: int
    source_end_frame: int
    playback_rate: float
    effect_chain: tuple[str, ...]

    @property
    def source_range(self) -> tuple[int, int]:
        return (self.source_start_frame, self.source_end_frame)


@dataclass(frozen=True, slots=True)
class TimelineSegment:
    segment_id: str
    timeline_start_frame: int
    timeline_end_frame: int
    fps: int
    base: SegmentClip
    overlays: tuple[SegmentClip, ...] = ()
    subtitles: tuple[SubtitleClip, ...] = ()

    @property
    def duration_frames(self) -> int:
        return self.timeline_end_frame - self.timeline_start_frame

    @property
    def duration_seconds(self) -> float:
        return self.duration_frames / self.fps


@dataclass(frozen=True, slots=True)
class RenderedSegment:
    segment: TimelineSegment
    path: Path
    cache_key: str
    cache_hit: bool


@dataclass(frozen=True, slots=True)
class TimelineRenderOutput:
    output_path: Path
    rendered_segments: tuple[RenderedSegment, ...]


@dataclass(frozen=True, slots=True)
class FfmpegProgressSample:
    out_time_ms: int | None
    progress: str | None


def split_timeline_segments(timeline: TimelineState) -> tuple[TimelineSegment, ...]:
    """Split the visual timeline into cacheable render segments."""

    base_clips = _media_clips_for_track(timeline, "visual_base")
    overlay_clips = _media_clips_for_track(timeline, "visual_overlay")
    subtitle_clips = _subtitle_clips_for_track(timeline)
    boundaries = {0, timeline.duration_frames}
    for clip in (*base_clips, *overlay_clips):
        boundaries.add(max(0, clip.timeline_start_frame))
        boundaries.add(min(timeline.duration_frames, clip.timeline_end_frame))
    ordered = sorted(
        boundary for boundary in boundaries if 0 <= boundary <= timeline.duration_frames
    )

    units: list[TimelineSegment] = []
    for start, end in itertools.pairwise(ordered):
        if start >= end:
            continue
        base = _covering_clip(base_clips, start, end)
        if base is None:
            continue
        overlays = tuple(
            _trim_clip(clip, start, end)
            for clip in overlay_clips
            if clip.timeline_start_frame < end and clip.timeline_end_frame > start
        )
        units.append(
            TimelineSegment(
                segment_id="",
                timeline_start_frame=start,
                timeline_end_frame=end,
                fps=timeline.fps,
                base=_trim_clip(base, start, end),
                overlays=overlays,
                subtitles=(),
            )
        )

    merged: list[TimelineSegment] = []
    for unit in units:
        if merged and _can_merge(merged[-1], unit):
            previous = merged[-1]
            merged[-1] = TimelineSegment(
                segment_id="",
                timeline_start_frame=previous.timeline_start_frame,
                timeline_end_frame=unit.timeline_end_frame,
                fps=previous.fps,
                base=SegmentClip(
                    timeline_clip_id=previous.base.timeline_clip_id,
                    track_id=previous.base.track_id,
                    asset_id=previous.base.asset_id,
                    clip_id=previous.base.clip_id,
                    timeline_start_frame=previous.base.timeline_start_frame,
                    timeline_end_frame=unit.base.timeline_end_frame,
                    source_start_frame=previous.base.source_start_frame,
                    source_end_frame=unit.base.source_end_frame,
                    playback_rate=previous.base.playback_rate,
                    effect_chain=previous.base.effect_chain,
                ),
                overlays=(),
                subtitles=(),
            )
            continue
        merged.append(unit)

    return tuple(
        TimelineSegment(
            segment_id=(
                f"seg_{index:04d}_{segment.timeline_start_frame}_{segment.timeline_end_frame}"
            ),
            timeline_start_frame=segment.timeline_start_frame,
            timeline_end_frame=segment.timeline_end_frame,
            fps=segment.fps,
            base=segment.base,
            overlays=segment.overlays,
            subtitles=_subtitle_clips_for_segment(
                subtitle_clips,
                segment.timeline_start_frame,
                segment.timeline_end_frame,
            ),
        )
        for index, segment in enumerate(merged, start=1)
    )


def compute_segment_cache_key(
    segment: TimelineSegment,
    *,
    sources: Mapping[str, MediaSource],
    profile: RenderProfile,
    ffmpeg_version: str,
    subtitle_templates: SubtitleTemplateMap | None = None,
) -> str:
    return segment_cache_key(
        {
            "schema": "rushes.render.segment.v2",
            "segment": _segment_cache_payload(
                segment,
                sources=sources,
                subtitle_templates=subtitle_templates,
            ),
            "project_params": profile.cache_payload(),
            "ffmpeg_version": ffmpeg_version,
        }
    )


async def render_timeline_to_file(
    timeline: TimelineState,
    *,
    sources: Mapping[str, MediaSource],
    paths: WorkspacePaths,
    profile: RenderProfile,
    output_path: Path,
    ffmpeg_bin: str = "ffmpeg",
    ffmpeg_version: str | None = None,
    cache: SegmentRenderCache | None = None,
    cache_max_bytes: int = DEFAULT_MAX_BYTES,
    subtitle_templates: SubtitleTemplateMap | None = None,
    progress_callback: ProgressCallback | None = None,
) -> TimelineRenderOutput:
    """Render visual segments with cache, then concat and mix into output_path."""

    resolved_paths = paths.initialize()
    active_cache = cache or SegmentRenderCache(resolved_paths, max_bytes=cache_max_bytes)
    version = ffmpeg_version or get_ffmpeg_version(ffmpeg_bin=ffmpeg_bin)
    segments = split_timeline_segments(timeline)
    rendered: list[RenderedSegment] = []
    segment_count = max(len(segments), 1)
    for index, segment in enumerate(segments):
        start_weight = index / segment_count * 0.8
        end_weight = (index + 1) / segment_count * 0.8

        async def _segment_progress(
            value: float,
            *,
            _start: float = start_weight,
            _end: float = end_weight,
        ) -> None:
            # 默认参数绑定循环变量，避免闭包晚绑定（B023）
            await _emit_progress(progress_callback, _start + (_end - _start) * value)

        rendered.append(
            await render_segment(
                segment,
                sources=sources,
                paths=resolved_paths,
                profile=profile,
                cache=active_cache,
                ffmpeg_bin=ffmpeg_bin,
                ffmpeg_version=version,
                subtitle_templates=subtitle_templates,
                progress_callback=_segment_progress,
            )
        )
    from .concat import concatenate_and_mix

    async def _concat_progress(value: float) -> None:
        await _emit_progress(progress_callback, 0.8 + value * 0.2)

    await concatenate_and_mix(
        timeline,
        rendered_segments=tuple(rendered),
        sources=sources,
        paths=resolved_paths,
        profile=profile,
        output_path=output_path,
        ffmpeg_bin=ffmpeg_bin,
        progress_callback=_concat_progress,
    )
    await _emit_progress(progress_callback, 1.0)
    return TimelineRenderOutput(output_path=output_path, rendered_segments=tuple(rendered))


async def render_segment(
    segment: TimelineSegment,
    *,
    sources: Mapping[str, MediaSource],
    paths: WorkspacePaths,
    profile: RenderProfile,
    cache: SegmentRenderCache,
    ffmpeg_bin: str,
    ffmpeg_version: str,
    subtitle_templates: SubtitleTemplateMap | None = None,
    progress_callback: ProgressCallback | None = None,
) -> RenderedSegment:
    cache_key = compute_segment_cache_key(
        segment,
        sources=sources,
        profile=profile,
        ffmpeg_version=ffmpeg_version,
        subtitle_templates=subtitle_templates,
    )
    cached = cache.get(cache_key)
    if cached is not None:
        await _emit_progress(progress_callback, 1.0)
        return RenderedSegment(segment=segment, path=cached, cache_key=cache_key, cache_hit=True)

    paths.initialize()
    tmp_path = paths.tmp_dir / f"{segment.segment_id}_{uuid4().hex}.mp4"
    ass_path = tmp_path.with_suffix(".ass")
    if segment.subtitles:
        write_segment_ass(
            ass_path,
            segment.subtitles,
            segment_start_frame=segment.timeline_start_frame,
            segment_end_frame=segment.timeline_end_frame,
            fps=segment.fps,
            play_res=(profile.width, profile.height),
            subtitle_templates=_require_subtitle_templates(segment, subtitle_templates),
        )
    command = build_segment_command(
        segment,
        sources=sources,
        profile=profile,
        output_path=tmp_path,
        ffmpeg_bin=ffmpeg_bin,
    )
    try:
        await run_ffmpeg_command(
            command,
            duration_seconds=segment.duration_seconds,
            progress_callback=progress_callback,
        )
        cached_path = cache.put_file(cache_key, tmp_path)
    except Exception:
        tmp_path.unlink(missing_ok=True)
        raise
    finally:
        ass_path.unlink(missing_ok=True)
    return RenderedSegment(
        segment=segment,
        path=cached_path,
        cache_key=cache_key,
        cache_hit=False,
    )


def build_segment_command(
    segment: TimelineSegment,
    *,
    sources: Mapping[str, MediaSource],
    profile: RenderProfile,
    output_path: Path,
    ffmpeg_bin: str = "ffmpeg",
) -> list[str]:
    base_source = _require_source(segment.base.asset_id, sources)
    command = [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-ss",
        _seconds_arg(segment.base.source_start_frame, segment.fps),
        "-t",
        _seconds_arg(segment.duration_frames, segment.fps),
        "-i",
        str(base_source.path),
    ]
    for overlay in segment.overlays:
        source = _require_source(overlay.asset_id, sources)
        command.extend(
            [
                "-ss",
                _seconds_arg(overlay.source_start_frame, segment.fps),
                "-t",
                _seconds_arg(segment.duration_frames, segment.fps),
                "-i",
                str(source.path),
            ]
        )

    if segment.overlays:
        filtergraph, video_label = _overlay_filtergraph(segment, profile)
        if segment.subtitles:
            filtergraph, video_label = _append_subtitles_filtergraph(
                filtergraph,
                video_label,
                output_path.with_suffix(".ass"),
            )
        command.extend(["-filter_complex", filtergraph, "-map", video_label])
    else:
        visual_filter = _visual_filter(segment.base, profile)
        if segment.subtitles:
            visual_filter = _append_subtitles_filter(
                visual_filter,
                output_path.with_suffix(".ass"),
            )
        command.extend(["-vf", visual_filter, "-map", "0:v:0"])
    command.extend(
        [
            "-an",
            "-r",
            str(profile.fps),
            "-c:v",
            "libx264",
            "-preset",
            profile.preset,
            "-pix_fmt",
            "yuv420p",
            "-movflags",
            "+faststart",
        ]
    )
    if profile.video_bitrate is not None:
        command.extend(["-b:v", profile.video_bitrate])
    elif profile.crf is not None:
        command.extend(["-crf", str(profile.crf)])
    command.extend(["-progress", "pipe:1", "-nostats", str(output_path)])
    return command


async def run_ffmpeg_command(
    command: Sequence[str],
    *,
    duration_seconds: float | None = None,
    progress_callback: ProgressCallback | None = None,
) -> None:
    process = await asyncio.create_subprocess_exec(
        *command,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    assert process.stdout is not None
    assert process.stderr is not None
    stderr_task = asyncio.create_task(process.stderr.read())
    progress_state: dict[str, str] = {}
    while True:
        line = await process.stdout.readline()
        if not line:
            break
        sample = parse_progress_line(line.decode(errors="replace"), progress_state)
        if sample is not None and sample.out_time_ms is not None:
            progress = progress_from_out_time_ms(sample.out_time_ms, duration_seconds)
            await _emit_progress(progress_callback, progress)
    returncode = await process.wait()
    stderr = (await stderr_task).decode(errors="replace")
    if returncode != 0:
        summary = stderr_summary(stderr)
        raise SegmentRenderError(summary or "ffmpeg render failed", stderr_summary=summary)
    await _emit_progress(progress_callback, 1.0)


def parse_ffmpeg_progress(text: str) -> tuple[FfmpegProgressSample, ...]:
    state: dict[str, str] = {}
    samples: list[FfmpegProgressSample] = []
    for line in text.splitlines():
        sample = parse_progress_line(line, state)
        if sample is not None:
            samples.append(sample)
    return tuple(samples)


def parse_progress_line(
    line: str,
    state: dict[str, str] | None = None,
) -> FfmpegProgressSample | None:
    target = state if state is not None else {}
    stripped = line.strip()
    if not stripped or "=" not in stripped:
        return None
    key, value = stripped.split("=", 1)
    target[key] = value
    if key != "progress":
        return None
    raw_out_time = target.get("out_time_ms")
    out_time_ms: int | None = None
    if raw_out_time is not None:
        try:
            out_time_ms = int(raw_out_time)
        except ValueError:
            out_time_ms = None
    return FfmpegProgressSample(out_time_ms=out_time_ms, progress=value)


def progress_from_out_time_ms(out_time_ms: int, duration_seconds: float | None) -> float:
    if duration_seconds is None or duration_seconds <= 0:
        return 0.0
    duration_us = duration_seconds * 1_000_000
    duration_ms = duration_seconds * 1_000
    if out_time_ms <= duration_ms * 2:
        progress = out_time_ms / duration_ms
    else:
        progress = out_time_ms / duration_us
    return max(0.0, min(1.0, progress))


def get_ffmpeg_version(*, ffmpeg_bin: str = "ffmpeg") -> str:
    result = subprocess.run(
        [ffmpeg_bin, "-version"],
        capture_output=True,
        check=False,
        text=True,
    )
    if result.returncode != 0:
        raise SegmentRenderError(
            stderr_summary(result.stderr) or "ffmpeg -version failed",
            stderr_summary=stderr_summary(result.stderr),
        )
    first_line = result.stdout.splitlines()[0] if result.stdout else "ffmpeg version unknown"
    return re.sub(r"\s+", " ", first_line.strip())


def stderr_summary(stderr: str, *, max_lines: int = 12) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-max_lines:] if line)


def _media_clips_for_track(timeline: TimelineState, track_id: str) -> tuple[TimelineMediaClip, ...]:
    for track in timeline.tracks:
        if track.track_id == track_id:
            return tuple(clip for clip in track.clips if isinstance(clip, TimelineMediaClip))
    return ()


def _subtitle_clips_for_track(timeline: TimelineState) -> tuple[SubtitleClip, ...]:
    for track in timeline.tracks:
        if track.track_id == "subtitles":
            return tuple(clip for clip in track.clips if isinstance(clip, SubtitleClip))
    return ()


def _subtitle_clips_for_segment(
    clips: Sequence[SubtitleClip],
    start: int,
    end: int,
) -> tuple[SubtitleClip, ...]:
    return tuple(
        clip
        for clip in clips
        if clip.timeline_start_frame < end and clip.timeline_end_frame > start
    )


def _covering_clip(
    clips: Sequence[TimelineMediaClip],
    start: int,
    end: int,
) -> TimelineMediaClip | None:
    for clip in clips:
        if clip.timeline_start_frame <= start and clip.timeline_end_frame >= end:
            return clip
    return None


def _trim_clip(clip: TimelineMediaClip, start: int, end: int) -> SegmentClip:
    clipped_start = max(start, clip.timeline_start_frame)
    clipped_end = min(end, clip.timeline_end_frame)
    source_offset_start = round((clipped_start - clip.timeline_start_frame) * clip.playback_rate)
    source_offset_end = round((clipped_end - clip.timeline_start_frame) * clip.playback_rate)
    return SegmentClip(
        timeline_clip_id=clip.timeline_clip_id,
        track_id=clip.track_id,
        asset_id=clip.asset_id,
        clip_id=clip.clip_id,
        timeline_start_frame=clipped_start,
        timeline_end_frame=clipped_end,
        source_start_frame=clip.source_start_frame + source_offset_start,
        source_end_frame=clip.source_start_frame + source_offset_end,
        playback_rate=clip.playback_rate,
        effect_chain=_effect_chain(clip.effects),
    )


def _effect_chain(effects: Sequence[Mapping[str, Any]]) -> tuple[str, ...]:
    return tuple(
        json.dumps(effect, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
        for effect in effects
    )


def _can_merge(left: TimelineSegment, right: TimelineSegment) -> bool:
    if left.overlays or right.overlays:
        return False
    if left.timeline_end_frame != right.timeline_start_frame:
        return False
    return (
        left.fps == right.fps
        and left.base.asset_id == right.base.asset_id
        and left.base.clip_id == right.base.clip_id
        and left.base.source_end_frame == right.base.source_start_frame
        and left.base.playback_rate == right.base.playback_rate
        and left.base.effect_chain == right.base.effect_chain
    )


def _segment_cache_payload(
    segment: TimelineSegment,
    *,
    sources: Mapping[str, MediaSource],
    subtitle_templates: SubtitleTemplateMap | None,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "segment_id": segment.segment_id,
        "timeline_range": [segment.timeline_start_frame, segment.timeline_end_frame],
        "fps": segment.fps,
        "base": _clip_cache_payload(segment.base, sources=sources),
        "overlays": [_clip_cache_payload(clip, sources=sources) for clip in segment.overlays],
    }
    if segment.subtitles:
        payload["subtitles"] = subtitle_cache_payload(
            segment.subtitles,
            segment_start_frame=segment.timeline_start_frame,
            segment_end_frame=segment.timeline_end_frame,
            subtitle_templates=_require_subtitle_templates(segment, subtitle_templates),
        )
    return payload


def _clip_cache_payload(clip: SegmentClip, *, sources: Mapping[str, MediaSource]) -> dict[str, Any]:
    source = _require_source(clip.asset_id, sources)
    return {
        "track_id": clip.track_id,
        "timeline_clip_id": clip.timeline_clip_id,
        "asset_id": clip.asset_id,
        "asset_hash": source.asset_hash,
        "clip_id": clip.clip_id,
        "source_range": [clip.source_start_frame, clip.source_end_frame],
        "timeline_range": [clip.timeline_start_frame, clip.timeline_end_frame],
        "playback_rate": clip.playback_rate,
        "filter_chain": list(clip.effect_chain),
        "kind": source.kind,
    }


def _require_source(asset_id: str, sources: Mapping[str, MediaSource]) -> MediaSource:
    source = sources.get(asset_id)
    if source is None:
        raise SegmentRenderError(f"missing render source for asset: {asset_id}")
    return source


def _require_subtitle_templates(
    segment: TimelineSegment,
    subtitle_templates: SubtitleTemplateMap | None,
) -> SubtitleTemplateMap:
    if not segment.subtitles:
        return {}
    resolved_templates = resolve_subtitle_templates(subtitle_templates)
    missing = sorted(
        {
            clip.style_template_id
            for clip in segment.subtitles
            if clip.style_template_id not in resolved_templates
        }
    )
    if missing:
        raise SegmentRenderError(f"missing subtitle style template: {', '.join(missing)}")
    return resolved_templates


def _visual_filter(clip: SegmentClip, profile: RenderProfile) -> str:
    filters = [
        f"fps={profile.fps}",
        (f"scale={profile.width}:{profile.height}:force_original_aspect_ratio=increase"),
        f"crop={profile.width}:{profile.height}",
        "setsar=1",
        "format=yuv420p",
    ]
    if clip.playback_rate != 1.0:
        filters.insert(0, f"setpts=PTS/{clip.playback_rate:.8g}")
    return ",".join(filters)


def _append_subtitles_filter(filter_chain: str, ass_path: Path) -> str:
    return f"{filter_chain},subtitles=filename={_escape_filter_path(ass_path)}"


def _overlay_filtergraph(segment: TimelineSegment, profile: RenderProfile) -> tuple[str, str]:
    parts = [f"[0:v]{_visual_filter(segment.base, profile)}[v0]"]
    current = "v0"
    for index, overlay in enumerate(segment.overlays, start=1):
        label = f"ov{index}"
        output = f"v{index}"
        parts.append(f"[{index}:v]{_visual_filter(overlay, profile)}[{label}]")
        parts.append(f"[{current}][{label}]overlay=0:0:shortest=1[{output}]")
        current = output
    return ";".join(parts), f"[{current}]"


def _append_subtitles_filtergraph(
    filtergraph: str,
    video_label: str,
    ass_path: Path,
) -> tuple[str, str]:
    output = "[vsub]"
    return (
        f"{filtergraph};{video_label}subtitles=filename={_escape_filter_path(ass_path)}{output}",
        output,
    )


def _escape_filter_path(path: Path) -> str:
    return str(path).replace("\\", "\\\\").replace(":", r"\:").replace("'", r"\'")


def _seconds_arg(frames: int, fps: int) -> str:
    return f"{frames / fps:.6f}"


async def _emit_progress(callback: ProgressCallback | None, value: float) -> None:
    if callback is None:
        return
    normalized = max(0.0, min(1.0, value))
    result = callback(normalized)
    if inspect.isawaitable(result):
        await result
