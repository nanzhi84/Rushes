from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

import media.probe as probe
from contracts.asset import AssetKind
from media.probe import asset_needs_proxy, probe_stream_codec

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None
ffmpeg_only = pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")


def test_image_and_font_never_need_proxy() -> None:
    # 图片走缩略图/原图直连、字体无媒体代理：不探测文件即判否。
    assert asset_needs_proxy(AssetKind.IMAGE, "/nonexistent.png") is False
    assert asset_needs_proxy(AssetKind.FONT, "/nonexistent.ttf") is False
    assert asset_needs_proxy("image", "/nonexistent.png") is False


def test_video_playable_codecs_skip_proxy(monkeypatch) -> None:
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "hevc")
    assert asset_needs_proxy(AssetKind.VIDEO, "/clip.mov") is False
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "h264")
    assert asset_needs_proxy("video", "/clip.mp4") is False


def test_video_unplayable_or_unknown_codec_needs_proxy(monkeypatch) -> None:
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "prores")
    assert asset_needs_proxy(AssetKind.VIDEO, "/clip.mov") is True
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: None)
    assert asset_needs_proxy(AssetKind.VIDEO, "/clip.mov") is True  # 探测失败按需代理兜底


def test_audio_playable_vs_unplayable(monkeypatch) -> None:
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "mp3")
    assert asset_needs_proxy(AssetKind.AUDIO, "/song.mp3") is False
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "pcm_s16le")
    assert asset_needs_proxy(AssetKind.AUDIO, "/voice.wav") is False
    monkeypatch.setattr(probe, "probe_stream_codec", lambda *a, **k: "amr_nb")
    assert asset_needs_proxy(AssetKind.AUDIO, "/voice.amr") is True


def test_unknown_kind_defaults_to_needs_proxy() -> None:
    assert asset_needs_proxy("something_else", "/x.bin") is True


def test_probe_stream_codec_returns_none_for_missing_file() -> None:
    assert probe_stream_codec("/definitely/missing.mp4", stream_type="v") is None


def test_probe_stream_codec_returns_none_for_junk(tmp_path: Path) -> None:
    junk = tmp_path / "junk.mp4"
    junk.write_bytes(b"not a real video")
    assert probe_stream_codec(junk, stream_type="v") is None


def test_probe_stream_codec_survives_missing_binary(tmp_path: Path) -> None:
    target = tmp_path / "clip.mp4"
    target.write_bytes(b"x")
    assert probe_stream_codec(target, stream_type="v", ffprobe_bin="definitely-not-ffprobe") is None


@ffmpeg_only
@pytest.mark.ffmpeg
def test_probe_stream_codec_reads_real_h264(tmp_path: Path) -> None:
    video = tmp_path / "clip.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=128x128:rate=30",
            "-pix_fmt",
            "yuv420p",
            "-c:v",
            "libx264",
            str(video),
        ],
        check=True,
        capture_output=True,
    )
    assert probe_stream_codec(video, stream_type="v") == "h264"
    assert asset_needs_proxy(AssetKind.VIDEO, video) is False
