"""Shot splitting with PySceneDetect and optional TransNetV2 refinement."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol
from uuid import uuid4

from contracts.events import CapabilityDegraded


class ShotSplitError(RuntimeError):
    """Raised when shot splitting cannot inspect a video."""


@dataclass(frozen=True, slots=True)
class Shot:
    shot_id: str
    start_frame: int
    end_frame: int


@dataclass(frozen=True, slots=True)
class ShotSplitConfig:
    content_threshold: float = 27.0
    min_scene_len: int = 15
    transnetv2_onnx_path: Path | None = None
    case_id: str | None = None


@dataclass(frozen=True, slots=True)
class ShotSplitResult:
    shots: tuple[Shot, ...]
    events: tuple[dict[str, Any], ...] = ()


class TransNetV2Adapter(Protocol):
    def refine(self, video_path: Path, shots: Sequence[Shot]) -> tuple[Shot, ...]:
        """Return refined shots for ``video_path``."""


class OnnxTransNetV2Adapter:
    """Adapter boundary for a future TransNetV2 ONNX implementation.

    The PRD requires the weight path to be configurable. This adapter only
    declares the boundary in M2; when weights are absent the pipeline emits a
    degradation event and keeps the PySceneDetect coarse cuts.
    """

    def __init__(self, weights_path: Path) -> None:
        self._weights_path = weights_path

    @property
    def is_available(self) -> bool:
        return self._weights_path.exists()

    def refine(self, video_path: Path, shots: Sequence[Shot]) -> tuple[Shot, ...]:
        del video_path
        if not self.is_available:
            raise FileNotFoundError(str(self._weights_path))
        return tuple(shots)


def split_shots(
    video_path: str | Path,
    *,
    config: ShotSplitConfig | None = None,
    transnet_adapter: TransNetV2Adapter | None = None,
) -> ShotSplitResult:
    """Split ``video_path`` into frame ranges.

    CI and local tests exercise the PySceneDetect path. TransNetV2 is explicitly
    optional and produces CapabilityDegraded event data when configured weights
    are missing.
    """

    cfg = config or ShotSplitConfig()
    path = Path(video_path).expanduser().resolve(strict=True)
    shots = _pyscenedetect_shots(path, cfg)
    events: list[dict[str, Any]] = []
    adapter = transnet_adapter
    if adapter is None and cfg.transnetv2_onnx_path is not None:
        adapter = OnnxTransNetV2Adapter(cfg.transnetv2_onnx_path)
    if adapter is None:
        return ShotSplitResult(shots=shots, events=tuple(events))
    try:
        shots = adapter.refine(path, shots)
    except FileNotFoundError as exc:
        events.append(
            _capability_degraded(
                case_id=cfg.case_id,
                reason="TransNetV2 weights are not available; using PySceneDetect coarse cuts",
                payload={"weights_path": str(exc), "video_path": str(path)},
            )
        )
    return ShotSplitResult(shots=shots, events=tuple(events))


def _pyscenedetect_shots(path: Path, config: ShotSplitConfig) -> tuple[Shot, ...]:
    try:
        from scenedetect import SceneManager, open_video
        from scenedetect.detectors import ContentDetector
    except ImportError as exc:
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
        end_frame = max(1, _frame_count_with_cv2(path))
        return (Shot(shot_id="shot_0001", start_frame=0, end_frame=end_frame),)
    shots: list[Shot] = []
    for index, (start, end) in enumerate(scenes, start=1):
        start_frame = int(start.get_frames())
        end_frame = int(end.get_frames())
        if end_frame <= start_frame:
            end_frame = start_frame + 1
        shots.append(
            Shot(
                shot_id=f"shot_{index:04d}",
                start_frame=start_frame,
                end_frame=end_frame,
            )
        )
    return tuple(shots)


def _frame_count_with_cv2(path: Path) -> int:
    try:
        import cv2
    except ImportError:
        return 1
    capture = cv2.VideoCapture(str(path))
    try:
        count = int(capture.get(cv2.CAP_PROP_FRAME_COUNT))
    finally:
        capture.release()
    return count if count > 0 else 1


def _capability_degraded(
    *,
    case_id: str | None,
    reason: str,
    payload: dict[str, Any],
) -> dict[str, Any]:
    return CapabilityDegraded(
        degradation_id=f"degraded_transnetv2_{uuid4().hex}",
        case_id=case_id,
        capability="shot_split.transnetv2",
        provider_id=None,
        reason=reason,
        fallback="pyscenedetect.content_detector",
        payload=payload,
    ).model_dump(mode="json")
