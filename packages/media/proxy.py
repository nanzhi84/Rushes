"""Low-bitrate proxy generation through ffmpeg."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path
from uuid import uuid4

from media.hwaccel import hwaccel_decode_args
from storage.object_store import ObjectRef, ObjectStore
from storage.workspace_paths import WorkspacePaths

_VIDEOTOOLBOX_ENCODER = "h264_videotoolbox"
_SOFTWARE_ENCODER = "libx264"
# 每个 ffmpeg_bin 只探测一次编码器，别每个 job 都探（探测要起一次 ffmpeg 子进程）。
_encoder_cache: dict[str, str] = {}


class MediaProxyError(RuntimeError):
    """Raised when ffmpeg cannot generate a proxy."""


def generate_proxy(
    source_path: str | Path,
    *,
    paths: WorkspacePaths,
    ffmpeg_bin: str = "ffmpeg",
    audio_only: bool = False,
) -> ObjectRef:
    paths.initialize()
    source = Path(source_path).expanduser().resolve(strict=True)
    suffix = ".mp3" if audio_only else ".mp4"
    tmp_path = paths.tmp_dir / f"proxy_{uuid4().hex}{suffix}"
    if audio_only:
        _run_or_raise(_audio_proxy_command(ffmpeg_bin, source, tmp_path), tmp_path)
    else:
        _run_video_proxy_with_fallback(ffmpeg_bin, source, tmp_path)
    try:
        return ObjectStore(paths).put_file(tmp_path)
    finally:
        tmp_path.unlink(missing_ok=True)


def _run_video_proxy_with_fallback(ffmpeg_bin: str, source: Path, destination: Path) -> None:
    encoder = _preferred_video_encoder(ffmpeg_bin)
    decode_args = hwaccel_decode_args(ffmpeg_bin)
    result = _run_ffmpeg(
        _video_proxy_command(
            ffmpeg_bin, source, destination, video_encoder=encoder, decode_args=decode_args
        )
    )
    if result.returncode == 0:
        return
    if decode_args or encoder == _VIDEOTOOLBOX_ENCODER:
        # 硬件路径（硬解/硬编）运行期失败：把本进程后续探测降级为软件编码，并整条回落
        # 「软解 + libx264」立即重试一次（输入编码不被 videotoolbox 支持时也走这条）。
        _encoder_cache[ffmpeg_bin] = _SOFTWARE_ENCODER
        result = _run_ffmpeg(
            _video_proxy_command(
                ffmpeg_bin, source, destination, video_encoder=_SOFTWARE_ENCODER, decode_args=[]
            )
        )
        if result.returncode == 0:
            return
    destination.unlink(missing_ok=True)
    raise MediaProxyError(_stderr_summary(result.stderr) or "ffmpeg proxy generation failed")


def _preferred_video_encoder(ffmpeg_bin: str) -> str:
    cached = _encoder_cache.get(ffmpeg_bin)
    if cached is not None:
        return cached
    encoder = _probe_video_encoder(ffmpeg_bin)
    _encoder_cache[ffmpeg_bin] = encoder
    return encoder


def _probe_video_encoder(ffmpeg_bin: str) -> str:
    # 仅 macOS 才考虑硬件编码；探测本机 ffmpeg 是否编入 h264_videotoolbox。
    if not _is_macos():
        return _SOFTWARE_ENCODER
    try:
        result = subprocess.run(
            [ffmpeg_bin, "-hide_banner", "-encoders"],
            capture_output=True,
            check=False,
            text=True,
        )
    except OSError:
        return _SOFTWARE_ENCODER
    if result.returncode == 0 and _VIDEOTOOLBOX_ENCODER in result.stdout:
        return _VIDEOTOOLBOX_ENCODER
    return _SOFTWARE_ENCODER


def _is_macos() -> bool:
    return sys.platform == "darwin"


def _run_ffmpeg(command: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, capture_output=True, check=False, text=True)


def _run_or_raise(command: list[str], destination: Path) -> None:
    result = _run_ffmpeg(command)
    if result.returncode != 0:
        destination.unlink(missing_ok=True)
        raise MediaProxyError(_stderr_summary(result.stderr) or "ffmpeg proxy generation failed")


def _video_proxy_command(
    ffmpeg_bin: str,
    source: Path,
    destination: Path,
    *,
    video_encoder: str,
    decode_args: list[str] | None = None,
) -> list[str]:
    return [
        ffmpeg_bin,
        "-y",
        *(decode_args or []),
        "-i",
        str(source),
        "-map",
        "0:v:0",
        "-map",
        "0:a?",
        "-vf",
        "scale=-2:540",
        *_video_encoder_args(video_encoder),
        "-movflags",
        "+faststart",
        "-c:a",
        "aac",
        "-b:a",
        "96k",
        str(destination),
    ]


def _video_encoder_args(video_encoder: str) -> list[str]:
    if video_encoder == _VIDEOTOOLBOX_ENCODER:
        # 硬件编码器不吃 x264 的 -preset/-crf，用码率控制得到相近体积的 540p 代理。
        return ["-c:v", _VIDEOTOOLBOX_ENCODER, "-b:v", "2000k"]
    return ["-c:v", _SOFTWARE_ENCODER, "-preset", "veryfast", "-crf", "30"]


def _audio_proxy_command(ffmpeg_bin: str, source: Path, destination: Path) -> list[str]:
    return [
        ffmpeg_bin,
        "-y",
        "-i",
        str(source),
        "-vn",
        "-c:a",
        "libmp3lame",
        "-b:a",
        "96k",
        str(destination),
    ]


def _stderr_summary(stderr: str) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-8:] if line)
