from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest

from media.waveform import WaveformError, compute_waveform_peaks, downsample_peaks

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None


def test_downsample_peaks_empty_input_returns_empty() -> None:
    assert downsample_peaks(np.array([], dtype=np.float32)) == []


def test_downsample_peaks_zero_buckets_returns_empty() -> None:
    assert downsample_peaks(np.array([0.1, 0.2], dtype=np.float32), buckets=0) == []


def test_downsample_peaks_reports_min_and_max_per_bucket() -> None:
    samples = np.array([-1.0, 0.5, 0.25, 1.0], dtype=np.float32)

    peaks = downsample_peaks(samples, buckets=2)

    assert peaks == [[-1.0, 0.5], [0.25, 1.0]]


def test_downsample_peaks_caps_buckets_to_sample_count() -> None:
    samples = np.array([0.2, -0.4, 0.6], dtype=np.float32)

    peaks = downsample_peaks(samples, buckets=512)

    assert len(peaks) == 3
    assert peaks[0] == pytest.approx([0.2, 0.2])
    assert peaks[1] == pytest.approx([-0.4, -0.4])
    assert peaks[2] == pytest.approx([0.6, 0.6])


def test_downsample_peaks_exact_bucket_count() -> None:
    samples = np.linspace(-1.0, 1.0, num=2048, dtype=np.float32)

    peaks = downsample_peaks(samples, buckets=512)

    assert len(peaks) == 512
    assert peaks[0][0] == pytest.approx(-1.0, abs=1e-3)
    assert peaks[-1][1] == pytest.approx(1.0, abs=1e-3)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_compute_waveform_peaks_from_sine_wav(tmp_path: Path) -> None:
    wav = tmp_path / "sine.wav"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=440:duration=1",
            str(wav),
        ],
        check=True,
        capture_output=True,
    )

    peaks = compute_waveform_peaks(wav, buckets=64)

    assert len(peaks) == 64
    assert all(low <= high for low, high in peaks)
    assert max(high for _, high in peaks) > 0.1


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_compute_waveform_peaks_raises_on_non_audio(tmp_path: Path) -> None:
    junk = tmp_path / "not_audio.bin"
    junk.write_bytes(b"\x00\x01\x02not-audio")

    with pytest.raises(WaveformError):
        compute_waveform_peaks(junk)
