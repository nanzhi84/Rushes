"""Annotation pipelines and projection helpers."""

from .quality import CleanSpan, QualityConfig, clean_spans, detect_quality_events
from .shot_split import Shot, ShotSplitConfig, ShotSplitResult, split_shots

__all__ = [
    "CleanSpan",
    "QualityConfig",
    "Shot",
    "ShotSplitConfig",
    "ShotSplitResult",
    "clean_spans",
    "detect_quality_events",
    "split_shots",
]
