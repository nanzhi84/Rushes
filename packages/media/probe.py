"""ffprobe + PyAV media probing for AssetProbe contracts."""

from __future__ import annotations

import importlib
import json
import subprocess
from fractions import Fraction
from pathlib import Path
from typing import Any, cast

from contracts.asset import AssetProbe


class MediaProbeError(RuntimeError):
    """Raised when ffprobe/PyAV cannot read media metadata."""


def probe_media(path: str | Path, *, ffprobe_bin: str = "ffprobe") -> AssetProbe:
    source = Path(path).expanduser().resolve(strict=True)
    payload = _run_ffprobe(source, ffprobe_bin=ffprobe_bin)
    probe = _probe_from_ffprobe(payload)
    return _correct_with_pyav(source, probe)


def _run_ffprobe(path: Path, *, ffprobe_bin: str) -> dict[str, Any]:
    command = [
        ffprobe_bin,
        "-v",
        "error",
        "-print_format",
        "json",
        "-show_format",
        "-show_streams",
        str(path),
    ]
    result = subprocess.run(command, capture_output=True, check=False, text=True)
    if result.returncode != 0:
        raise MediaProbeError(_stderr_summary(result.stderr) or "ffprobe failed")
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise MediaProbeError("ffprobe did not return valid JSON") from exc
    if not isinstance(payload, dict):
        raise MediaProbeError("ffprobe JSON root must be an object")
    return payload


def _probe_from_ffprobe(payload: dict[str, Any]) -> AssetProbe:
    streams = payload.get("streams", [])
    if not isinstance(streams, list):
        streams = []
    video_stream = _first_stream(streams, "video")
    audio_stream = _first_stream(streams, "audio")
    duration = _duration(payload, video_stream, audio_stream)
    return AssetProbe(
        duration_sec=max(0.0, duration),
        fps=_rate(video_stream.get("avg_frame_rate") if video_stream is not None else None),
        width=_optional_int(video_stream.get("width") if video_stream is not None else None),
        height=_optional_int(video_stream.get("height") if video_stream is not None else None),
        has_audio=audio_stream is not None,
    )


def _correct_with_pyav(path: Path, probe: AssetProbe) -> AssetProbe:
    try:
        av = cast(Any, importlib.import_module("av"))
    except ImportError:
        return probe

    try:
        container = av.open(str(path), mode="r")
    except Exception:
        return probe
    try:
        video_stream = next(
            (stream for stream in container.streams if stream.type == "video"),
            None,
        )
        if video_stream is None:
            return probe
        fps = _float_rate(getattr(video_stream, "average_rate", None)) or probe.fps
        decoded_frames = _decoded_video_frames(container)
        duration = probe.duration_sec
        if fps is not None and decoded_frames > 0:
            duration = decoded_frames / fps
        elif getattr(container, "duration", None) is not None:
            duration = float(container.duration * av.time_base)
        return probe.model_copy(
            update={
                "duration_sec": max(0.0, duration),
                "fps": fps,
                "width": int(getattr(video_stream, "width", 0) or probe.width or 0) or None,
                "height": int(getattr(video_stream, "height", 0) or probe.height or 0) or None,
            }
        )
    finally:
        container.close()


def _decoded_video_frames(container: Any) -> int:
    count = 0
    try:
        for _frame in container.decode(video=0):
            count += 1
    except Exception:
        return 0
    return count


def _first_stream(streams: list[Any], codec_type: str) -> dict[str, Any] | None:
    for stream in streams:
        if isinstance(stream, dict) and stream.get("codec_type") == codec_type:
            return stream
    return None


def _duration(
    payload: dict[str, Any],
    video_stream: dict[str, Any] | None,
    audio_stream: dict[str, Any] | None,
) -> float:
    candidates: list[Any] = []
    format_payload = payload.get("format")
    if isinstance(format_payload, dict):
        candidates.append(format_payload.get("duration"))
    if video_stream is not None:
        candidates.append(video_stream.get("duration"))
    if audio_stream is not None:
        candidates.append(audio_stream.get("duration"))
    for candidate in candidates:
        try:
            if candidate is not None:
                return float(candidate)
        except (TypeError, ValueError):
            continue
    return 0.0


def _rate(value: Any) -> float | None:
    if not isinstance(value, str) or value in {"", "0/0", "N/A"}:
        return None
    try:
        rate = float(Fraction(value))
    except (ValueError, ZeroDivisionError):
        return None
    return rate if rate > 0 else None


def _float_rate(value: Any) -> float | None:
    if value is None:
        return None
    try:
        rate = float(value)
    except (TypeError, ValueError, ZeroDivisionError):
        return None
    return rate if rate > 0 else None


def _optional_int(value: Any) -> int | None:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed > 0 else None


def _stderr_summary(stderr: str) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-8:] if line)
