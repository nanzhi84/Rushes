from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from media.shots import (
    Shot,
    ShotSplitConfig,
    _analysis_clip_command,
    _tmp_dir,
    split_shots,
)
from storage.workspace_paths import WorkspacePaths

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


def test_analysis_clip_command_preserves_time_base_and_scales() -> None:
    command = _analysis_clip_command(
        Path("/src.mov"), Path("/out.mp4"), ["-hwaccel", "videotoolbox"]
    )
    # 保时基：passthrough 不丢/重排帧；只降分辨率、不改 fps（无 -r / 无 fps 滤镜）。
    assert command[command.index("-vsync") + 1] == "passthrough"
    assert "scale=-2:180" in command
    assert "-r" not in command
    assert not any("fps=" in arg for arg in command)
    # 硬解参数在 -i 之前。
    assert command.index("-hwaccel") < command.index("-i")


def test_tmp_dir_uses_workspace_tmp_when_paths_given(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "ws")
    resolved = _tmp_dir(paths)
    assert resolved == str(paths.tmp_dir)
    assert paths.tmp_dir.exists()  # 目录被创建
    assert _tmp_dir(None) is None


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
def test_split_shots_preserves_cut_time_within_tolerance(tmp_path: Path) -> None:
    video = _make_two_scene_video(tmp_path)  # 0.5s 红 + 0.5s 蓝 @30fps，切点在 ~0.5s
    paths = WorkspacePaths.from_root(tmp_path / "ws").initialize()

    shots = split_shots(
        video, config=ShotSplitConfig(content_threshold=12.0, min_scene_len=3), paths=paths
    )

    assert len(shots) >= 2
    # 经 180p 硬解降采样预处理后，红→蓝切点时间与原片误差 ≤0.2s（时间轴保真）。
    assert abs(shots[0].end_sec - 0.5) <= 0.2
    # 分析用小片写在 workspace tmp 且已随 finally 清理，无残留目录。
    assert not list(paths.tmp_dir.glob("rushes_shots_*"))


def test_prepare_analysis_clip_uses_original_without_hwaccel(tmp_path: Path, monkeypatch) -> None:
    import media.shots as shots_module

    monkeypatch.setattr(shots_module, "hwaccel_decode_args", lambda *a, **k: [])
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"x")

    analysis_path, cleanup = shots_module._prepare_analysis_clip(source)

    assert analysis_path == source  # 无硬解：直接在原片上分析
    cleanup()  # noop 不报错


def test_prepare_analysis_clip_falls_back_when_prepass_fails(tmp_path: Path, monkeypatch) -> None:
    import media.shots as shots_module

    monkeypatch.setattr(
        shots_module, "hwaccel_decode_args", lambda *a, **k: ["-hwaccel", "videotoolbox"]
    )
    monkeypatch.setattr(
        shots_module,
        "run_media_command",
        lambda *a, **k: subprocess.CompletedProcess(a[0], 1, "", "boom"),
    )
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"x")

    analysis_path, cleanup = shots_module._prepare_analysis_clip(source)

    assert analysis_path == source  # 预处理失败回落原片
    cleanup()


def test_prepare_analysis_clip_falls_back_when_ffmpeg_missing(tmp_path: Path, monkeypatch) -> None:
    import media.shots as shots_module

    monkeypatch.setattr(
        shots_module, "hwaccel_decode_args", lambda *a, **k: ["-hwaccel", "videotoolbox"]
    )

    def boom(*args, **kwargs):
        raise OSError("no ffmpeg")

    monkeypatch.setattr(shots_module, "run_media_command", boom)
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"x")

    analysis_path, cleanup = shots_module._prepare_analysis_clip(source)

    assert analysis_path == source  # ffmpeg 起不来也回落原片
    cleanup()


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
