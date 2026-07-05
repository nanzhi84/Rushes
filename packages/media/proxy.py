"""Low-bitrate proxy generation through ffmpeg."""

from __future__ import annotations

import subprocess
from pathlib import Path
from uuid import uuid4

from storage.object_store import ObjectRef, ObjectStore
from storage.workspace_paths import WorkspacePaths


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
    command = (
        _audio_proxy_command(ffmpeg_bin, source, tmp_path)
        if audio_only
        else _video_proxy_command(ffmpeg_bin, source, tmp_path)
    )
    result = subprocess.run(command, capture_output=True, check=False, text=True)
    if result.returncode != 0:
        tmp_path.unlink(missing_ok=True)
        raise MediaProxyError(_stderr_summary(result.stderr) or "ffmpeg proxy generation failed")
    try:
        return ObjectStore(paths).put_file(tmp_path)
    finally:
        tmp_path.unlink(missing_ok=True)


def _video_proxy_command(ffmpeg_bin: str, source: Path, destination: Path) -> list[str]:
    return [
        ffmpeg_bin,
        "-y",
        "-i",
        str(source),
        "-map",
        "0:v:0",
        "-map",
        "0:a?",
        "-vf",
        "scale=-2:540",
        "-c:v",
        "libx264",
        "-preset",
        "veryfast",
        "-crf",
        "30",
        "-movflags",
        "+faststart",
        "-c:a",
        "aac",
        "-b:a",
        "96k",
        str(destination),
    ]


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
