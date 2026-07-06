"""Cheap shot-boundary detection via PySceneDetect for the local index (Spec C).

Migrated from ``packages/annotation/shot_split.py`` PySceneDetect main path.
The TransNetV2 adapter and ``CapabilityDegraded`` fallback are intentionally
dropped: when no cuts are detected we return a single whole-video shot, and
shots are reported as second boundaries (not frames) so the index JSON stays
resolution/fps agnostic.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path


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

    video = open_video(str(path))
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
