"""Audio waveform peaks via ffmpeg PCM decode + numpy min/max downsampling."""

from __future__ import annotations

import subprocess
from pathlib import Path

import numpy as np
from numpy.typing import NDArray

DEFAULT_BUCKETS = 512
DEFAULT_SAMPLE_RATE = 8000


class WaveformError(RuntimeError):
    """Raised when ffmpeg cannot decode audio for waveform peaks."""


def downsample_peaks(
    samples: NDArray[np.floating],
    *,
    buckets: int = DEFAULT_BUCKETS,
) -> list[list[float]]:
    """Downsample normalized samples into ``[min, max]`` pairs per bucket.

    Returns at most ``buckets`` pairs; when there are fewer samples than
    buckets, each remaining sample gets its own bucket (never an empty group).
    """

    if buckets <= 0 or samples.size == 0:
        return []
    group_count = min(buckets, int(samples.size))
    groups = np.array_split(samples, group_count)
    return [[float(group.min()), float(group.max())] for group in groups]


def compute_waveform_peaks(
    audio_path: str | Path,
    *,
    buckets: int = DEFAULT_BUCKETS,
    sample_rate: int = DEFAULT_SAMPLE_RATE,
    ffmpeg_bin: str = "ffmpeg",
) -> list[list[float]]:
    """Decode ``audio_path`` to mono s16le PCM and return waveform peaks."""

    path = Path(audio_path).expanduser().resolve(strict=True)
    command = [
        ffmpeg_bin,
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        str(path),
        "-ac",
        "1",
        "-ar",
        str(sample_rate),
        "-f",
        "s16le",
        "-acodec",
        "pcm_s16le",
        "pipe:1",
    ]
    result = subprocess.run(command, capture_output=True, check=False)
    if result.returncode != 0:
        raise WaveformError(_stderr_summary(result.stderr) or "ffmpeg PCM decode failed")
    pcm = np.frombuffer(result.stdout, dtype=np.int16)
    samples = pcm.astype(np.float32) / 32768.0
    return downsample_peaks(samples, buckets=buckets)


def _stderr_summary(stderr: bytes) -> str:
    text = stderr.decode(errors="replace")
    return "\n".join(line for line in text.strip().splitlines()[-8:] if line)
