"""Subtitle timeline clip contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, model_validator


class SubtitleBinding(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["voiceover", "original_audio", "manual"]
    utterance_id: str | None = None


class SubtitleClip(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_clip_id: str
    track_id: Literal["subtitles"] = "subtitles"
    text: str
    timeline_start_frame: int
    timeline_end_frame: int
    style_template_id: str
    binding: SubtitleBinding
    safe_area_check: Literal["ok", "overflow", "occlusion_risk"]

    @model_validator(mode="after")
    def validate_frame_range(self) -> "SubtitleClip":
        if self.timeline_start_frame >= self.timeline_end_frame:
            raise ValueError(
                "subtitle frame range must satisfy timeline_start_frame < timeline_end_frame"
            )
        return self
