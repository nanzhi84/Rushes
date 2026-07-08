"""Cheap shot-boundary detection via PySceneDetect for the local index (Spec C).

Migrated from ``packages/annotation/shot_split.py`` PySceneDetect main path.
The TransNetV2 adapter and ``CapabilityDegraded`` fallback are intentionally
dropped: when no cuts are detected we return a single whole-video shot, and
shots are reported as second boundaries (not frames) so the index JSON stays
resolution/fps agnostic.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from media.hwaccel import hwaccel_decode_args

# 分析用小片的目标高度：分镜只要秒级摘要边界，180p 足够，且解码/分析都廉价。
_ANALYSIS_HEIGHT = 180
# 预处理 ffmpeg 超时（避免卡死的 ffmpeg 挂住 worker——本包无 timeout 的子进程是已知隐患）。
_PREPASS_TIMEOUT_S = 120


class ShotSplitError(RuntimeError):
    """Raised when shot splitting cannot inspect a video."""


@dataclass(frozen=True, slots=True)
class Shot:
    shot_id: str
    start_sec: float
    end_sec: float


@dataclass(frozen=True, slots=True)
class ShotSplitConfig:
    content_threshold: float = 27.0
    min_scene_len: int = 15


def split_shots(
    video_path: str | Path,
    *,
    config: ShotSplitConfig | None = None,
) -> tuple[Shot, ...]:
    """Split ``video_path`` into second-boundary shots via PySceneDetect."""

    cfg = config or ShotSplitConfig()
    path = Path(video_path).expanduser().resolve(strict=True)
    return _pyscenedetect_shots(path, cfg)


def _pyscenedetect_shots(path: Path, config: ShotSplitConfig) -> tuple[Shot, ...]:
    try:
        from scenedetect import SceneManager, open_video
        from scenedetect.detectors import ContentDetector
    except ImportError as exc:  # pragma: no cover - scenedetect is a hard dependency
        raise ShotSplitError("scenedetect is required for shot splitting") from exc

    # opencv 直接软解 1080p60 HEVC 全帧要 ~34 CPU 秒/文件（吃满所有核 → 导入风扇狂叫）。先用
    # ffmpeg 硬解(videotoolbox)降采样成 180p 小片再分析，约 6 CPU 秒且分镜边界一致；无硬解/预处理
    # 失败回落原片直接分析（保持旧行为）。fps 保持不变，故 min_scene_len(按帧) 语义不变。
    analysis_path, cleanup = _prepare_analysis_clip(path)
    try:
        video = open_video(str(analysis_path))
        scene_manager = SceneManager()
        scene_manager.add_detector(
            ContentDetector(
                threshold=config.content_threshold,
                min_scene_len=config.min_scene_len,
            )
        )
        scene_manager.detect_scenes(video)
        scenes = scene_manager.get_scene_list()
        if not scenes:
            return (Shot(shot_id="shot_0001", start_sec=0.0, end_sec=_video_duration_sec(video)),)
        shots: list[Shot] = []
        for index, (start, end) in enumerate(scenes, start=1):
            start_sec = max(0.0, _timecode_seconds(start))
            end_sec = max(start_sec, _timecode_seconds(end))
            shots.append(Shot(shot_id=f"shot_{index:04d}", start_sec=start_sec, end_sec=end_sec))
        return tuple(shots)
    finally:
        cleanup()


def _prepare_analysis_clip(path: Path) -> tuple[Path, Callable[[], None]]:
    """用 ffmpeg 硬解把源片降采样成 180p 小片供分析；失败/无硬解回落原片。

    返回 ``(分析用路径, 清理回调)``。fps 保持原样（只降分辨率），因此 shot 的秒边界与
    ``min_scene_len``（按帧）语义都不受影响。
    """

    decode_args = hwaccel_decode_args()
    if not decode_args:
        # 无硬解：软解预处理并不比 opencv 直接软解省，直接在原片上分析（保持旧行为）。
        return path, _noop
    workdir = Path(tempfile.mkdtemp(prefix="rushes_shots_"))
    analysis_path = workdir / "analysis.mp4"
    command = [
        "ffmpeg",
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        *decode_args,
        "-i",
        str(path),
        "-vf",
        f"scale=-2:{_ANALYSIS_HEIGHT}",
        "-an",
        "-c:v",
        "libx264",
        "-preset",
        "ultrafast",
        "-crf",
        "30",
        str(analysis_path),
    ]
    try:
        result = subprocess.run(
            command, capture_output=True, check=False, text=True, timeout=_PREPASS_TIMEOUT_S
        )
    except (OSError, subprocess.SubprocessError):
        shutil.rmtree(workdir, ignore_errors=True)
        return path, _noop
    if result.returncode != 0 or not analysis_path.exists():
        shutil.rmtree(workdir, ignore_errors=True)
        return path, _noop
    return analysis_path, lambda: shutil.rmtree(workdir, ignore_errors=True)


def _noop() -> None:
    return None


def _video_duration_sec(video: object) -> float:
    duration = getattr(video, "duration", None)
    if duration is None:
        return 0.0
    return max(0.0, _timecode_seconds(duration))


def _timecode_seconds(timecode: object) -> float:
    # PySceneDetect 新版把 get_seconds() 标记弃用，改用 .seconds 属性；两者都兜住。
    seconds = getattr(timecode, "seconds", None)
    if seconds is not None:
        return float(seconds)
    getter = getattr(timecode, "get_seconds", None)
    if callable(getter):
        return float(getter())
    return 0.0
