"""JPEG thumbnail extraction through ffmpeg pipes (Spec C local index).

Reuses the ``_extract_frame_data_uri`` scaling idea from the media_tools VLM
frame extractor, but returns raw JPEG bytes destined for the object store
instead of a base64 data URI.
"""

from __future__ import annotations

from pathlib import Path

from media.hwaccel import hwaccel_decode_args

from .process import run_media_command

DEFAULT_MAX_SIZE = 768


class ThumbnailError(RuntimeError):
    """Raised when ffmpeg cannot render a thumbnail."""


def extract_video_thumbnail(
    video_path: str | Path,
    *,
    seconds: float = 1.0,
    max_size: int = DEFAULT_MAX_SIZE,
    ffmpeg_bin: str = "ffmpeg",
) -> bytes:
    """Return a single JPEG cover frame decoded at ``seconds`` into the video."""

    path = Path(video_path).expanduser().resolve(strict=True)
    decode_args = hwaccel_decode_args(ffmpeg_bin)
    if decode_args:
        # 视频抽帧先走 videotoolbox 硬解（1080p60 HEVC 软解一帧也要拉起全核）；硬解失败回落软解。
        jpeg = _try_ffmpeg_jpeg(
            _thumbnail_command(
                ffmpeg_bin,
                path,
                seconds=max(0.0, seconds),
                max_size=max_size,
                decode_args=decode_args,
            )
        )
        if jpeg is not None:
            return jpeg
    return _run_ffmpeg_jpeg(
        _thumbnail_command(ffmpeg_bin, path, seconds=max(0.0, seconds), max_size=max_size)
    )


def render_image_thumbnail(
    image_path: str | Path,
    *,
    max_size: int = DEFAULT_MAX_SIZE,
    ffmpeg_bin: str = "ffmpeg",
) -> bytes:
    """Return a downscaled JPEG thumbnail for a still image."""

    path = Path(image_path).expanduser().resolve(strict=True)
    command = _thumbnail_command(ffmpeg_bin, path, seconds=None, max_size=max_size)
    return _run_ffmpeg_jpeg(command)


def _thumbnail_command(
    ffmpeg_bin: str,
    path: Path,
    *,
    seconds: float | None,
    max_size: int,
    decode_args: list[str] | None = None,
) -> list[str]:
    command = [ffmpeg_bin, "-hide_banner", "-loglevel", "error", *(decode_args or [])]
    if seconds is not None:
        command += ["-ss", f"{seconds:.6f}"]
    command += [
        "-i",
        str(path),
        "-frames:v",
        "1",
        "-vf",
        f"scale={max_size}:{max_size}:force_original_aspect_ratio=decrease",
        "-f",
        "image2pipe",
        "-vcodec",
        "mjpeg",
        "pipe:1",
    ]
    return command


def _run_ffmpeg_jpeg(command: list[str]) -> bytes:
    result = run_media_command(command, text=False, timeout=60)
    if result.returncode != 0:
        raise ThumbnailError(_stderr_summary(result.stderr) or "ffmpeg thumbnail failed")
    if not result.stdout:
        raise ThumbnailError("ffmpeg produced no thumbnail bytes")
    return result.stdout


def _try_ffmpeg_jpeg(command: list[str]) -> bytes | None:
    """跑 ffmpeg 抽帧，成功返回 JPEG 字节，任何失败返回 None（供硬解→软解回落）。"""

    result = run_media_command(command, text=False, timeout=60)
    if result.returncode != 0 or not result.stdout:
        return None
    return result.stdout


def _stderr_summary(stderr: bytes) -> str:
    text = stderr.decode(errors="replace")
    return "\n".join(line for line in text.strip().splitlines()[-8:] if line)
