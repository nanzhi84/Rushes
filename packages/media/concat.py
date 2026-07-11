"""Concat rendered visual segments, then mix timeline audio."""

from __future__ import annotations

import inspect
import json
import math
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from uuid import uuid4

from contracts.timeline import TimelineMediaClip, TimelineState
from storage.workspace_paths import WorkspacePaths

from .process import communicate_media_command
from .segment_render import (
    MediaSource,
    ProgressCallback,
    RenderedSegment,
    RenderProfile,
    SegmentRenderError,
    run_ffmpeg_command,
)


@dataclass(frozen=True, slots=True)
class _AudioInput:
    input_index: int
    label: str
    clip: TimelineMediaClip


async def concatenate_and_mix(
    timeline: TimelineState,
    *,
    rendered_segments: Sequence[RenderedSegment],
    sources: Mapping[str, MediaSource],
    paths: WorkspacePaths,
    profile: RenderProfile,
    output_path: Path,
    ffmpeg_bin: str = "ffmpeg",
    progress_callback: ProgressCallback | None = None,
) -> None:
    if not rendered_segments:
        raise SegmentRenderError("timeline has no rendered visual segments")
    paths.initialize()
    audio_clips = _audio_clips(timeline)
    if not audio_clips:
        await _concat_video(
            rendered_segments,
            paths=paths,
            output_path=output_path,
            ffmpeg_bin=ffmpeg_bin,
            progress_callback=progress_callback,
        )
        return

    visual_path = paths.tmp_dir / f"concat_visual_{uuid4().hex}.mp4"
    try:
        await _concat_video(
            rendered_segments,
            paths=paths,
            output_path=visual_path,
            ffmpeg_bin=ffmpeg_bin,
            progress_callback=_scale_progress(progress_callback, 0.0, 0.25),
        )
        await _mix_audio(
            timeline,
            visual_path=visual_path,
            audio_clips=audio_clips,
            sources=sources,
            paths=paths,
            profile=profile,
            output_path=output_path,
            ffmpeg_bin=ffmpeg_bin,
            progress_callback=_scale_progress(progress_callback, 0.25, 1.0),
        )
    finally:
        visual_path.unlink(missing_ok=True)


async def _concat_video(
    rendered_segments: Sequence[RenderedSegment],
    *,
    paths: WorkspacePaths,
    output_path: Path,
    ffmpeg_bin: str,
    progress_callback: ProgressCallback | None,
) -> None:
    concat_file = paths.tmp_dir / f"concat_{uuid4().hex}.txt"
    concat_file.write_text(_concat_manifest(rendered_segments), encoding="utf-8")
    duration = sum(segment.segment.duration_seconds for segment in rendered_segments)
    command = [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "concat",
        "-safe",
        "0",
        "-i",
        str(concat_file),
        "-c",
        "copy",
        "-movflags",
        "+faststart",
        "-progress",
        "pipe:1",
        "-nostats",
        str(output_path),
    ]
    try:
        await run_ffmpeg_command(
            command,
            duration_seconds=duration,
            progress_callback=progress_callback,
        )
    finally:
        concat_file.unlink(missing_ok=True)


async def _mix_audio(
    timeline: TimelineState,
    *,
    visual_path: Path,
    audio_clips: Sequence[TimelineMediaClip],
    sources: Mapping[str, MediaSource],
    paths: WorkspacePaths,
    profile: RenderProfile,
    output_path: Path,
    ffmpeg_bin: str,
    progress_callback: ProgressCallback | None,
) -> None:
    base_command, audio_inputs = _audio_input_command(
        ffmpeg_bin,
        visual_path=visual_path,
        timeline=timeline,
        audio_clips=audio_clips,
        sources=sources,
    )
    first_filter, _ = _audio_filtergraph(
        timeline,
        audio_inputs,
        profile=profile,
        loudnorm_measured=None,
        loudnorm_print_format="json",
    )
    pass1 = [
        *base_command,
        "-filter_complex",
        first_filter,
        "-map",
        "[mixout]",
        "-f",
        "null",
        "-",
    ]
    measured = await _run_loudnorm_pass(pass1)
    await _emit(progress_callback, 0.45)

    final_filter, _ = _audio_filtergraph(
        timeline,
        audio_inputs,
        profile=profile,
        loudnorm_measured=measured,
        loudnorm_print_format="summary",
    )
    command = [
        *base_command,
        "-filter_complex",
        final_filter,
        "-map",
        "0:v:0",
        "-map",
        "[mixout]",
        "-c:v",
        "copy",
        "-c:a",
        "aac",
        "-b:a",
        profile.audio_bitrate,
        "-shortest",
        "-movflags",
        "+faststart",
        "-progress",
        "pipe:1",
        "-nostats",
        str(output_path),
    ]
    await run_ffmpeg_command(
        command,
        duration_seconds=timeline.duration_frames / timeline.fps,
        progress_callback=_scale_progress(progress_callback, 0.45, 1.0),
    )


def _audio_filtergraph(
    timeline: TimelineState,
    audio_inputs: Sequence[_AudioInput],
    *,
    profile: RenderProfile,
    loudnorm_measured: Mapping[str, str] | None,
    loudnorm_print_format: str,
) -> tuple[str, str]:
    duration_seconds = timeline.duration_frames / timeline.fps
    parts: list[str] = []
    speech_labels: list[str] = []
    bgm_labels: list[str] = []
    for audio_input in audio_inputs:
        gain = _gain_linear(audio_input.clip.gain_db)
        delay_ms = max(
            0,
            round(audio_input.clip.timeline_start_frame / timeline.fps * 1000),
        )
        parts.append(
            f"[{audio_input.input_index}:a:0]"
            f"volume={gain:.8g},"
            f"adelay={delay_ms}:all=1,"
            f"apad,"
            f"atrim=0:{duration_seconds:.6f},"
            "asetpts=PTS-STARTPTS"
            f"[{audio_input.label}]"
        )
        if audio_input.clip.track_id == "bgm":
            bgm_labels.append(f"[{audio_input.label}]")
        else:
            speech_labels.append(f"[{audio_input.label}]")

    speech_bus = _mix_bus(parts, speech_labels, "speech")
    bgm_bus = _mix_bus(parts, bgm_labels, "bgm")
    if bgm_bus is not None and speech_bus is not None:
        # ffmpeg 滤镜流标签一次性消费：speech 既做 sidechain key 又参与 amix，
        # 必须先 asplit 复制（真实渲染实测踩坑：matches no streams）。
        parts.append(f"{speech_bus}asplit=2[speech_key][speech_mix]")
        parts.append(
            f"{bgm_bus}[speech_key]"
            "sidechaincompress=threshold=0.05:ratio=8:attack=20:release=250"
            "[ducked_bgm]"
        )
        parts.append("[speech_mix][ducked_bgm]amix=inputs=2:duration=longest:normalize=0[premix]")
        premix = "[premix]"
    elif speech_bus is not None:
        premix = speech_bus
    elif bgm_bus is not None:
        premix = bgm_bus
    else:
        raise SegmentRenderError("audio mix requested without audio labels")

    loudnorm = _loudnorm_filter(profile, loudnorm_measured, loudnorm_print_format)
    parts.append(f"{premix}{loudnorm}[mixout]")
    return ";".join(parts), "[mixout]"


def _mix_bus(parts: list[str], labels: Sequence[str], output_name: str) -> str | None:
    if not labels:
        return None
    if len(labels) == 1:
        parts.append(f"{labels[0]}anull[{output_name}]")
        return f"[{output_name}]"
    joined = "".join(labels)
    parts.append(f"{joined}amix=inputs={len(labels)}:duration=longest:normalize=0[{output_name}]")
    return f"[{output_name}]"


def _loudnorm_filter(
    profile: RenderProfile,
    measured: Mapping[str, str] | None,
    print_format: str,
) -> str:
    base = f"loudnorm=I={profile.loudnorm_i}:TP={profile.loudnorm_tp}:LRA={profile.loudnorm_lra}"
    if measured is None:
        return f"{base}:print_format={print_format}"
    required = {
        "input_i": "measured_I",
        "input_tp": "measured_TP",
        "input_lra": "measured_LRA",
        "input_thresh": "measured_thresh",
        "target_offset": "offset",
    }
    if not all(source_key in measured for source_key in required):
        return f"{base}:print_format={print_format}"
    measured_args = ":".join(
        f"{target_key}={measured[source_key]}" for source_key, target_key in required.items()
    )
    return f"{base}:{measured_args}:linear=true:print_format={print_format}"


def _audio_input_command(
    ffmpeg_bin: str,
    *,
    visual_path: Path,
    timeline: TimelineState,
    audio_clips: Sequence[TimelineMediaClip],
    sources: Mapping[str, MediaSource],
) -> tuple[list[str], tuple[_AudioInput, ...]]:
    command = [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        str(visual_path),
    ]
    inputs: list[_AudioInput] = []
    for index, clip in enumerate(audio_clips, start=1):
        source = sources.get(clip.asset_id)
        if source is None:
            raise SegmentRenderError(f"missing audio source for asset: {clip.asset_id}")
        command.extend(
            [
                "-ss",
                _seconds_arg(clip.source_start_frame, timeline.fps),
                "-t",
                _seconds_arg(clip.timeline_end_frame - clip.timeline_start_frame, timeline.fps),
                "-i",
                str(source.path),
            ]
        )
        inputs.append(_AudioInput(input_index=index, label=f"a{index}", clip=clip))
    return command, tuple(inputs)


async def _run_loudnorm_pass(command: Sequence[str]) -> dict[str, str] | None:
    try:
        result = await communicate_media_command(command)
    except TimeoutError as exc:
        raise SegmentRenderError(
            "ffmpeg loudnorm analysis timed out",
            stderr_summary="timeout",
        ) from exc
    stderr = result.stderr.decode(errors="replace")
    if result.returncode != 0:
        raise SegmentRenderError(
            _stderr_summary(stderr) or "ffmpeg loudnorm analysis failed",
            stderr_summary=_stderr_summary(stderr),
        )
    return _parse_loudnorm_json(stderr)


def _parse_loudnorm_json(stderr: str) -> dict[str, str] | None:
    match = re.search(r"\{[\s\S]*?\"target_offset\"[\s\S]*?\}", stderr)
    if match is None:
        return None
    try:
        payload = json.loads(match.group(0))
    except json.JSONDecodeError:
        return None
    if not isinstance(payload, dict):
        return None
    return {str(key): str(value) for key, value in payload.items()}


def _concat_manifest(rendered_segments: Sequence[RenderedSegment]) -> str:
    return "".join(f"file '{_concat_path(segment.path)}'\n" for segment in rendered_segments)


def _concat_path(path: Path) -> str:
    return str(path.resolve()).replace("'", "'\\''")


def _audio_clips(timeline: TimelineState) -> tuple[TimelineMediaClip, ...]:
    clips: list[TimelineMediaClip] = []
    for track in timeline.tracks:
        if track.track_id not in {"original_audio", "voiceover", "bgm"}:
            continue
        clips.extend(clip for clip in track.clips if isinstance(clip, TimelineMediaClip))
    return tuple(sorted(clips, key=lambda clip: (clip.timeline_start_frame, clip.track_id)))


def _gain_linear(gain_db: float) -> float:
    return math.pow(10.0, gain_db / 20.0)


def _seconds_arg(frames: int, fps: int) -> str:
    return f"{frames / fps:.6f}"


def _scale_progress(
    callback: ProgressCallback | None,
    start: float,
    end: float,
) -> ProgressCallback | None:
    if callback is None:
        return None

    async def _wrapped(value: float) -> None:
        await _emit(callback, start + (end - start) * value)

    return _wrapped


async def _emit(callback: ProgressCallback | None, value: float) -> None:
    if callback is None:
        return
    result = callback(max(0.0, min(1.0, value)))
    if inspect.isawaitable(result):
        await result


def _stderr_summary(stderr: str, *, max_lines: int = 12) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-max_lines:] if line)
