"""Annotation pipeline entry points."""

from .video import VideoAnnotationConfig, VideoAnnotationResult, run_video_annotation

__all__ = ["VideoAnnotationConfig", "VideoAnnotationResult", "run_video_annotation"]
