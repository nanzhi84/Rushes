from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

import media.thumbnails as thumbnails_module
from media.thumbnails import (
    ThumbnailError,
    _thumbnail_command,
    extract_video_thumbnail,
    render_image_thumbnail,
)

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None

JPEG_MAGIC = b"\xff\xd8\xff"


def test_thumbnail_command_includes_seek_and_scale() -> None:
    command = _thumbnail_command("ffmpeg", Path("/x.mp4"), seconds=1.5, max_size=512)

    assert "-ss" in command
    assert "1.500000" in command
    assert "scale=512:512:force_original_aspect_ratio=decrease" in command
    assert command[-1] == "pipe:1"


def test_thumbnail_command_includes_hwaccel_decode_before_input() -> None:
    command = _thumbnail_command(
        "ffmpeg",
        Path("/x.mp4"),
        seconds=1.0,
        max_size=256,
        decode_args=["-hwaccel", "videotoolbox"],
    )

    assert "-hwaccel" in command
    assert command.index("-hwaccel") < command.index("-i")


def test_extract_video_thumbnail_falls_back_to_software_when_hwaccel_fails(
    tmp_path: Path, monkeypatch
) -> None:
    video = tmp_path / "clip.mp4"
    video.write_bytes(b"x")  # subprocess 被 mock，仅需文件存在（strict resolve）。
    monkeypatch.setattr(
        thumbnails_module, "hwaccel_decode_args", lambda *a, **k: ["-hwaccel", "videotoolbox"]
    )

    def fake_run(command, **kwargs):
        if "-hwaccel" in command:  # 硬解命令失败
            return subprocess.CompletedProcess(command, 1, b"", b"hw decode failed")
        return subprocess.CompletedProcess(command, 0, JPEG_MAGIC + b"soft", b"")

    monkeypatch.setattr(thumbnails_module, "run_media_command", fake_run)

    data = extract_video_thumbnail(video, seconds=0.1)

    assert data == JPEG_MAGIC + b"soft"  # 回落软解产物


def test_thumbnail_command_omits_seek_for_images() -> None:
    command = _thumbnail_command("ffmpeg", Path("/x.png"), seconds=None, max_size=256)

    assert "-ss" not in command
    assert "scale=256:256:force_original_aspect_ratio=decrease" in command


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_extract_video_thumbnail_returns_jpeg(tmp_path: Path) -> None:
    video = tmp_path / "clip.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=320x240:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
    )

    data = extract_video_thumbnail(video, seconds=0.1, max_size=128)

    assert data.startswith(JPEG_MAGIC)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_render_image_thumbnail_returns_jpeg(tmp_path: Path) -> None:
    image = tmp_path / "still.png"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=320x240:rate=1",
            "-frames:v",
            "1",
            str(image),
        ],
        check=True,
        capture_output=True,
    )

    data = render_image_thumbnail(image, max_size=64)

    assert data.startswith(JPEG_MAGIC)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_extract_video_thumbnail_raises_on_non_media(tmp_path: Path) -> None:
    junk = tmp_path / "not_media.txt"
    junk.write_text("hello", encoding="utf-8")

    with pytest.raises(ThumbnailError):
        extract_video_thumbnail(junk)
