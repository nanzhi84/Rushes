"""Audio extraction helpers backed by ffmpeg."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from uuid import uuid4

from storage.workspace_paths import WorkspacePaths

from .process import run_media_command


class AudioExtractError(RuntimeError):
    """Raised when ffmpeg cannot extract a 16 kHz mono WAV."""


@dataclass(frozen=True, slots=True)
class ExtractedAudio:
    path: Path
    stderr_summary: str


def extract_audio_to_wav(
    source_path: str | Path,
    *,
    paths: WorkspacePaths,
    ffmpeg_bin: str = "ffmpeg",
    output_name: str | None = None,
) -> ExtractedAudio:
    """Extract one media asset's audio track to a 16 kHz mono WAV in workspace tmp."""

    source = Path(source_path).expanduser().resolve(strict=True)
    tmp_dir = paths.initialize().tmp_dir
    name = output_name or f"audio_{uuid4().hex}.wav"
    destination = tmp_dir / name
    command = [
        ffmpeg_bin,
        "-y",
        "-i",
        str(source),
        "-vn",
        "-ac",
        "1",
        "-ar",
        "16000",
        "-f",
        "wav",
        str(destination),
    ]
    result = run_media_command(command, text=True)
    if result.returncode != 0:
        destination.unlink(missing_ok=True)
        raise AudioExtractError(_stderr_summary(result.stderr) or "ffmpeg audio extraction failed")
    return ExtractedAudio(path=destination, stderr_summary=_stderr_summary(result.stderr))


def _stderr_summary(stderr: str) -> str:
    return "\n".join(line for line in stderr.strip().splitlines()[-8:] if line)
