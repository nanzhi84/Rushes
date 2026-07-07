"""Timeline patch request and resolved patch contracts."""

from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class TimelinePatchReference(BaseModel):
    model_config = ConfigDict(extra="forbid")

    timeline_version: int | None = None
    preview_id: str | None = None


class TimeRangeSec(BaseModel):
    model_config = ConfigDict(extra="forbid")

    time_range_sec: tuple[float, float]

    @model_validator(mode="after")
    def validate_range(self) -> "TimeRangeSec":
        start, end = self.time_range_sec
        if start >= end:
            raise ValueError("time_range_sec must satisfy start < end")
        return self


class DeleteRangeOp(TimeRangeSec):
    kind: Literal["delete_range"]
    scope: Literal[
        "all_tracks",
        "visual",
        "audio",
        "subtitles",
        "visual_base",
        "visual_overlay",
        "original_audio",
        "voiceover",
        "bgm",
    ]
    ripple: bool


ClipRole = Literal["a_roll", "b_roll", "image"]
ClipInsertTrack = Literal["visual_base", "visual_overlay"]


class ReplaceClipOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["replace_clip"]
    timeline_clip_id: str
    asset_id: str
    source_start_s: float
    source_end_s: float
    role: ClipRole

    @model_validator(mode="after")
    def validate_source_range(self) -> "ReplaceClipOp":
        if self.source_start_s >= self.source_end_s:
            raise ValueError("source_start_s must be < source_end_s")
        return self


class ReorderBlocksOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["reorder_blocks"]
    block_id_order: list[str]


class TrimClipOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["trim_clip"]
    timeline_clip_id: str
    edge: Literal["head", "tail"]
    delta_sec: float


class InsertClipOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["insert_clip"]
    asset_id: str
    source_start_s: float
    source_end_s: float
    role: ClipRole
    track_id: ClipInsertTrack | None = None
    position_s: float | None = None

    @model_validator(mode="after")
    def validate_source_range(self) -> "InsertClipOp":
        if self.source_start_s >= self.source_end_s:
            raise ValueError("source_start_s must be < source_end_s")
        return self


class AllRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["all"] = "all"


class PatchTimeRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["time_range"] = "time_range"
    time_range_sec: tuple[float, float]

    @model_validator(mode="after")
    def validate_range(self) -> "PatchTimeRange":
        start, end = self.time_range_sec
        if start >= end:
            raise ValueError("time_range_sec must satisfy start < end")
        return self


class ClipIdsRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["clip_ids"] = "clip_ids"
    clip_ids: list[str]


type TimeOrAllRange = Annotated[AllRange | PatchTimeRange, Field(discriminator="kind")]
type ClipIdsOrAllRange = Annotated[AllRange | ClipIdsRange, Field(discriminator="kind")]


class GenerateSubtitlesOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["generate_subtitles"]
    source: Literal["voiceover", "original_audio"]
    style_template_id: str
    range: TimeOrAllRange = Field(default_factory=AllRange)


class SetSubtitleStyleOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["set_subtitle_style"]
    style_template_id: str
    range: ClipIdsOrAllRange


class EditSubtitleTextOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["edit_subtitle_text"]
    timeline_clip_id: str
    text: str


class RemoveTrackClipsOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["remove_track_clips"]
    track_id: Literal[
        "visual_base",
        "visual_overlay",
        "original_audio",
        "voiceover",
        "bgm",
        "subtitles",
    ]
    range: TimeOrAllRange


class AddBgmOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["add_bgm"]
    asset_id: str
    gain_db: float
    duck: bool


class AdjustGainOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["adjust_gain"]
    track_id: Literal["original_audio", "voiceover", "bgm"]
    gain_db: float


class SetPlaybackRateOp(BaseModel):
    model_config = ConfigDict(extra="forbid")

    kind: Literal["set_playback_rate"]
    timeline_clip_id: str
    rate: float


type TimelinePatchOp = Annotated[
    DeleteRangeOp
    | ReplaceClipOp
    | ReorderBlocksOp
    | TrimClipOp
    | InsertClipOp
    | GenerateSubtitlesOp
    | SetSubtitleStyleOp
    | EditSubtitleTextOp
    | RemoveTrackClipsOp
    | AddBgmOp
    | AdjustGainOp
    | SetPlaybackRateOp,
    Field(discriminator="kind"),
]


class TimelinePatchRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["TimelinePatchRequest.v1"] = Field(
        default="TimelinePatchRequest.v1", alias="schema"
    )
    case_id: str
    reference: TimelinePatchReference = Field(default_factory=TimelinePatchReference)
    op: TimelinePatchOp
    reason: str


class ResolvedRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    start_frame: int
    end_frame: int
    affected_clip_ids: list[str] = Field(default_factory=list)

    @model_validator(mode="after")
    def validate_frame_range(self) -> "ResolvedRange":
        if self.start_frame >= self.end_frame:
            raise ValueError("resolved range must satisfy start_frame < end_frame")
        return self


class ResolvedTimelinePatch(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["ResolvedTimelinePatch.v1"] = Field(
        default="ResolvedTimelinePatch.v1", alias="schema"
    )
    patch_id: str
    request_ref: TimelinePatchRequest
    resolved: ResolvedRange
    produced_timeline_version: int
    metadata: dict[str, Any] = Field(default_factory=dict)
