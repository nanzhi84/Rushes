"""Probe parsing branch coverage without real media."""

from __future__ import annotations

from pathlib import Path

import pytest

from media.probe import (
    MediaProbeError,
    _duration,
    _first_stream,
    _float_rate,
    _probe_from_ffprobe,
    _rate,
    probe_media,
)


def test_probe_media_rejects_missing_binary(tmp_path: Path) -> None:
    target = tmp_path / "clip.mp4"
    target.write_bytes(b"not a real video")
    with pytest.raises((MediaProbeError, FileNotFoundError)):
        probe_media(target, ffprobe_bin="definitely-not-ffprobe-bin")


def test_probe_from_ffprobe_handles_missing_streams() -> None:
    probe = _probe_from_ffprobe({"format": {"duration": "2.5"}})
    assert probe.has_audio is False
    assert probe.duration_sec == pytest.approx(2.5)


def test_first_stream_skips_non_dict_and_wrong_type() -> None:
    streams = ["junk", {"codec_type": "audio"}, {"codec_type": "video", "width": 10}]
    video = _first_stream(streams, "video")
    assert video is not None and video["width"] == 10
    assert _first_stream([], "video") is None


def test_duration_prefers_format_then_stream() -> None:
    assert _duration({"format": {"duration": "3.0"}}, None, None) == pytest.approx(3.0)
    assert _duration({}, {"duration": "1.5"}, None) == pytest.approx(1.5)
    assert _duration({}, None, {"duration": "0.7"}) == pytest.approx(0.7)
    assert _duration({}, None, None) == 0.0


def test_rate_parsing_variants() -> None:
    assert _rate("30000/1001") == pytest.approx(29.97, rel=1e-3)
    assert _rate("25") == pytest.approx(25.0)
    assert _rate("0/0") is None
    assert _rate(None) is None
    assert _float_rate(None) is None
