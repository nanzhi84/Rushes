from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from media.shots import Shot, ShotSplitConfig, split_shots

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_split_shots_detects_two_scenes(tmp_path: Path) -> None:
    video = _make_two_scene_video(tmp_path)

    shots = split_shots(video, config=ShotSplitConfig(content_threshold=12.0, min_scene_len=3))

    assert len(shots) >= 2
    assert shots[0].start_sec == pytest.approx(0.0)
    assert all(shot.end_sec >= shot.start_sec for shot in shots)
    assert all(isinstance(shot, Shot) for shot in shots)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_split_shots_returns_single_shot_without_cuts(tmp_path: Path) -> None:
    video = tmp_path / "flat.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "color=c=gray:duration=1:size=160x120:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
    )

    shots = split_shots(video)

    assert len(shots) == 1
    assert shots[0].shot_id == "shot_0001"
    assert shots[0].start_sec == pytest.approx(0.0)
    assert shots[0].end_sec > 0.0


def test_split_shots_requires_existing_file(tmp_path: Path) -> None:
    with pytest.raises(FileNotFoundError):
        split_shots(tmp_path / "missing.mp4")


def _make_two_scene_video(tmp_path: Path) -> Path:
    video = tmp_path / "two_scenes.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "color=c=red:duration=0.5:size=160x120:rate=30",
            "-f",
            "lavfi",
            "-i",
            "color=c=blue:duration=0.5:size=160x120:rate=30",
            "-filter_complex",
            "[0:v][1:v]concat=n=2:v=1:a=0,format=yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
    )
    return video
