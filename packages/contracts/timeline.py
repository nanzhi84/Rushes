"""Timeline state contracts."""

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from .subtitle import SubtitleClip

TrackId = Literal[
    "visual_base",
    "visual_overlay",
    "original_audio",
    "voiceover",
    "bgm",
    "subtitles",
]


class TimelineValidationReport(BaseModel):
    model_config = ConfigDict(extra="forbid")

    valid: bool
    checks: list[dict[str, Any]] = Field(default_factory=list)


class TimelineMediaClip(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_clip_id: str
    track_id: Literal["visual_base", "visual_overlay", "original_audio", "voiceover", "bgm"]
    asset_id: str
    clip_id: str | None = None
    role: Literal["a_roll", "b_roll", "image", "original_audio", "voiceover", "bgm"]
    timeline_start_frame: int
    timeline_end_frame: int
    source_start_frame: int
    source_end_frame: int
    playback_rate: float = 1.0
    lock_policy: Literal["free", "ripple_with_primary", "sync_to_audio", "pinned"] = "free"
    parent_block_id: str | None = None
    effects: list[dict[str, Any]] = Field(default_factory=list)
    gain_db: float = 0.0

    @model_validator(mode="after")
    def validate_frame_ranges(self) -> "TimelineMediaClip":
        if self.timeline_start_frame >= self.timeline_end_frame:
            raise ValueError(
                "timeline clip range must satisfy timeline_start_frame < timeline_end_frame"
            )
        if self.source_start_frame >= self.source_end_frame:
            raise ValueError("source clip range must satisfy source_start_frame < source_end_frame")
        return self


class TimelineTrack(BaseModel):
    model_config = ConfigDict(extra="forbid")

    track_id: TrackId
    track_type: Literal["primary_visual", "visual_overlay", "audio", "text"]
    clips: list[TimelineMediaClip | SubtitleClip] = Field(default_factory=list)


class TimelineState(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_id: str
    case_id: str
    version: int
    fps: int = 30
    duration_frames: int
    tracks: list[TimelineTrack]
    parent_version: int | None = None
    created_by_patch_id: str | None = None
    validation_report: TimelineValidationReport | None = None

    @model_validator(mode="after")
    def validate_tracks(self) -> "TimelineState":
        expected_types: dict[str, str] = {
            "visual_base": "primary_visual",
            "visual_overlay": "visual_overlay",
            "original_audio": "audio",
            "voiceover": "audio",
            "bgm": "audio",
            "subtitles": "text",
        }
        seen = [track.track_id for track in self.tracks]
        if set(seen) != set(expected_types) or len(seen) != len(expected_types):
            raise ValueError("TimelineState must contain exactly the six canonical tracks")

        for track in self.tracks:
            if track.track_type != expected_types[track.track_id]:
                expected = expected_types[track.track_id]
                raise ValueError(f"{track.track_id} must have track_type={expected}")
            for clip in track.clips:
                if clip.track_id != track.track_id:
                    raise ValueError("clip.track_id must match parent track_id")
                if track.track_id == "subtitles" and not isinstance(clip, SubtitleClip):
                    raise ValueError("subtitles track only accepts SubtitleClip")
                if track.track_id != "subtitles" and isinstance(clip, SubtitleClip):
                    raise ValueError("SubtitleClip is only valid on subtitles track")
        return self
