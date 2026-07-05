from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from media.probe import probe_media
from media.proxy import generate_proxy
from storage.workspace_paths import WorkspacePaths

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_probe_media_reads_lavfi_fixture(tmp_path: Path) -> None:
    video = _make_test_video(tmp_path)

    probe = probe_media(video)

    assert probe.duration_sec > 0
    assert probe.fps is not None
    assert probe.width == 128
    assert probe.height == 128


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
def test_generate_proxy_writes_object_store_proxy(tmp_path: Path) -> None:
    video = _make_test_video(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    proxy_ref = generate_proxy(video, paths=paths)

    proxy_path = paths.object_path(proxy_ref.object_hash)
    assert proxy_path.exists()
    assert proxy_path.stat().st_size == proxy_ref.size
    assert proxy_ref.size > 0


def _make_test_video(tmp_path: Path) -> Path:
    video = tmp_path / "fixture.mp4"
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
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video
