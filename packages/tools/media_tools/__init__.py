"""Harness-internal frame inspection primitives."""

from .handlers import (
    FrameExtractionError,
    FrameQuestionAnswer,
    LabeledImage,
    ask_vlm_about_frames,
    extract_frame_data_uri,
    image_path_data_uri,
    multimodal_messages,
)

__all__ = [
    "FrameExtractionError",
    "FrameQuestionAnswer",
    "LabeledImage",
    "ask_vlm_about_frames",
    "extract_frame_data_uri",
    "image_path_data_uri",
    "multimodal_messages",
]
