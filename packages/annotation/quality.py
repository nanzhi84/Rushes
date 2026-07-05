"""OpenCV quality analysis for annotation clips."""

from __future__ import annotations

from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal

from contracts.annotation import AnnotationClip, QualityEvent

from .shot_split import Shot

QualitySeverity = Literal["hard", "soft"]


class QualityDetectionError(RuntimeError):
    """Raised when OpenCV cannot read video frames."""


@dataclass(frozen=True, slots=True)
class QualityConfig:
    sample_stride: int = 3
    blur_hard_threshold: float = 40.0
    blur_soft_threshold: float = 100.0
    shake_hard_threshold: float = 42.0
    shake_soft_threshold: float = 25.0
    event_padding_frames: int = 1


@dataclass(frozen=True, slots=True)
class CleanSpan:
    clip_id: str
    start_frame: int
    end_frame: int


def detect_quality_events(
    video_path: str | Path,
    shots: Sequence[Shot],
    *,
    config: QualityConfig | None = None,
) -> tuple[QualityEvent, ...]:
    """Detect blur and shake events over sampled frames in each shot."""

    cfg = config or QualityConfig()
    if cfg.sample_stride <= 0:
        raise ValueError("sample_stride must be positive")
    try:
        import cv2
        import numpy as np
    except ImportError as exc:
        raise QualityDetectionError("opencv-python-headless and numpy are required") from exc

    path = Path(video_path).expanduser().resolve(strict=True)
    capture = cv2.VideoCapture(str(path))
    if not capture.isOpened():
        raise QualityDetectionError(f"cannot open video for quality analysis: {path}")
    events: list[QualityEvent] = []
    event_index = 1
    try:
        for shot in shots:
            sampled = _sample_grayscale_frames(capture, shot, cfg.sample_stride, cv2)
            if not sampled:
                continue
            blur_frames: list[tuple[int, QualitySeverity]] = []
            for frame_number, gray in sampled:
                variance = float(cv2.Laplacian(gray, cv2.CV_64F).var())
                severity = _severity(
                    variance,
                    hard_threshold=cfg.blur_hard_threshold,
                    soft_threshold=cfg.blur_soft_threshold,
                    lower_is_worse=True,
                )
                if severity is not None:
                    blur_frames.append((frame_number, severity))
            for severity, start, end in _events_from_flagged_frames(
                blur_frames,
                shot=shot,
                stride=cfg.sample_stride,
                padding=cfg.event_padding_frames,
            ):
                events.append(
                    QualityEvent(
                        event_id=f"q_{event_index:04d}",
                        kind="blur",
                        severity=severity,
                        start_frame=start,
                        end_frame=end,
                    )
                )
                event_index += 1

            shake_frames: list[tuple[int, QualitySeverity]] = []
            previous: Any | None = None
            for frame_number, gray in sampled:
                if previous is not None:
                    diff = _optical_flow_motion_score(previous, gray, cv2=cv2, np=np)
                    severity = _severity(
                        diff,
                        hard_threshold=cfg.shake_hard_threshold,
                        soft_threshold=cfg.shake_soft_threshold,
                        lower_is_worse=False,
                    )
                    if severity is not None:
                        shake_frames.append((frame_number, severity))
                previous = gray
            for severity, start, end in _events_from_flagged_frames(
                shake_frames,
                shot=shot,
                stride=cfg.sample_stride,
                padding=cfg.event_padding_frames,
            ):
                events.append(
                    QualityEvent(
                        event_id=f"q_{event_index:04d}",
                        kind="shake",
                        severity=severity,
                        start_frame=start,
                        end_frame=end,
                    )
                )
                event_index += 1
    finally:
        capture.release()
    return tuple(events)


def _optical_flow_motion_score(previous: Any, current: Any, *, cv2: Any, np: Any) -> float:
    flow = cv2.calcOpticalFlowFarneback(
        previous,
        current,
        None,
        0.5,
        3,
        15,
        3,
        5,
        1.2,
        0,
    )
    magnitude = np.sqrt(flow[..., 0] * flow[..., 0] + flow[..., 1] * flow[..., 1])
    return float(np.mean(magnitude))


def clean_spans(
    clips: Sequence[AnnotationClip | Shot | Any],
    quality_events: Iterable[QualityEvent],
) -> tuple[CleanSpan, ...]:
    """Subtract hard quality event ranges from clip/shot ranges."""

    hard_events = [event for event in quality_events if event.severity == "hard"]
    spans: list[CleanSpan] = []
    for clip in clips:
        clip_id, start_frame, end_frame = _clip_range(clip)
        remaining = [(start_frame, end_frame)]
        for event in hard_events:
            remaining = _subtract_interval(
                remaining,
                max(start_frame, event.start_frame),
                min(end_frame, event.end_frame),
            )
        for start, end in remaining:
            if start < end:
                spans.append(CleanSpan(clip_id=clip_id, start_frame=start, end_frame=end))
    return tuple(spans)


def _sample_grayscale_frames(
    capture: Any,
    shot: Shot,
    stride: int,
    cv2: Any,
) -> list[tuple[int, Any]]:
    frames: list[tuple[int, Any]] = []
    capture.set(cv2.CAP_PROP_POS_FRAMES, shot.start_frame)
    frame_number = shot.start_frame
    while frame_number < shot.end_frame:
        ok, frame = capture.read()
        if not ok:
            break
        if (frame_number - shot.start_frame) % stride == 0:
            frames.append((frame_number, cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY)))
        frame_number += 1
    return frames


def _severity(
    value: float,
    *,
    hard_threshold: float,
    soft_threshold: float,
    lower_is_worse: bool,
) -> QualitySeverity | None:
    if lower_is_worse:
        if value < hard_threshold:
            return "hard"
        if value < soft_threshold:
            return "soft"
        return None
    if value > hard_threshold:
        return "hard"
    if value > soft_threshold:
        return "soft"
    return None


def _events_from_flagged_frames(
    frames: Sequence[tuple[int, QualitySeverity]],
    *,
    shot: Shot,
    stride: int,
    padding: int,
) -> list[tuple[QualitySeverity, int, int]]:
    if not frames:
        return []
    ranges: list[tuple[QualitySeverity, int, int]] = []
    current_severity, start = frames[0][1], frames[0][0]
    previous = frames[0][0]
    for frame, severity in frames[1:]:
        contiguous = frame <= previous + stride + padding
        if severity == current_severity and contiguous:
            previous = frame
            continue
        ranges.append(
            (
                current_severity,
                max(shot.start_frame, start - padding),
                min(shot.end_frame, previous + stride + padding),
            )
        )
        current_severity, start, previous = severity, frame, frame
    ranges.append(
        (
            current_severity,
            max(shot.start_frame, start - padding),
            min(shot.end_frame, previous + stride + padding),
        )
    )
    return ranges


def _clip_range(clip: AnnotationClip | Shot | Any) -> tuple[str, int, int]:
    if isinstance(clip, AnnotationClip):
        return clip.clip_id, clip.source_start_frame, clip.source_end_frame
    if isinstance(clip, Shot):
        return clip.shot_id, clip.start_frame, clip.end_frame
    clip_id = getattr(clip, "clip_id", None) or getattr(clip, "shot_id", None)
    start = getattr(clip, "source_start_frame", None)
    if start is None:
        start = getattr(clip, "start_frame", None)
    end = getattr(clip, "source_end_frame", None)
    if end is None:
        end = getattr(clip, "end_frame", None)
    if not isinstance(clip_id, str) or not isinstance(start, int) or not isinstance(end, int):
        raise TypeError("clip must expose clip_id/source_start_frame/source_end_frame")
    return clip_id, start, end


def _subtract_interval(
    intervals: Sequence[tuple[int, int]],
    start: int,
    end: int,
) -> list[tuple[int, int]]:
    if start >= end:
        return list(intervals)
    result: list[tuple[int, int]] = []
    for current_start, current_end in intervals:
        if end <= current_start or start >= current_end:
            result.append((current_start, current_end))
            continue
        if current_start < start:
            result.append((current_start, start))
        if end < current_end:
            result.append((end, current_end))
    return result
