"""Case-level state contracts."""

from enum import StrEnum
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class AudioMode(StrEnum):
    KEEP_ORIGINAL = "keep_original"
    ROUGH_CUT = "rough_cut"
    UPLOADED_VOICEOVER = "uploaded_voiceover"
    TTS = "tts"
    SILENT = "silent"
    MIXED = "mixed"


class Brief(BaseModel):
    model_config = ConfigDict(extra="forbid")

    goal: str
    platform: str | None = None
    target_duration_sec: float | None = None
    style_notes: list[str] = Field(default_factory=list)
    confirmed_facts: list[str] = Field(default_factory=list)


class AudioPlan(BaseModel):
    model_config = ConfigDict(extra="forbid")

    mode: AudioMode
    source_asset_ids: list[str] = Field(default_factory=list)
    voiceover_asset_id: str | None = None
    transcript_id: str | None = None
    notes: list[str] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


class CutPlanSlot(BaseModel):
    model_config = ConfigDict(extra="forbid")

    slot_id: str
    brief: str
    target_duration_sec: tuple[float, float]
    narration_ref: dict[str, Any] | None = None

    @model_validator(mode="after")
    def validate_duration_range(self) -> "CutPlanSlot":
        start, end = self.target_duration_sec
        if start >= end:
            raise ValueError("target_duration_sec must be a half-open increasing range")
        return self


class RemovedRange(BaseModel):
    model_config = ConfigDict(extra="forbid")

    start_ms: int
    end_ms: int
    kind: str
    source: str

    @model_validator(mode="after")
    def validate_range(self) -> "RemovedRange":
        if self.start_ms >= self.end_ms:
            raise ValueError("removed range must satisfy start_ms < end_ms")
        return self


class CutPlan(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["CutPlan.v1"] = Field(default="CutPlan.v1", alias="schema")
    slots: list[CutPlanSlot] = Field(default_factory=list)
    removed_ranges: list[RemovedRange] = Field(default_factory=list)
    total_target_duration_sec: float


class SubtitlePostprocessPlan(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool
    style_template_id: str | None = None


class BgmPostprocessPlan(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool
    asset_id: str | None = None
    gain_db: float | None = None
    duck: bool | None = None


class PostprocessPlan(BaseModel):
    model_config = ConfigDict(extra="forbid")

    subtitle: SubtitlePostprocessPlan | None = None
    bgm: BgmPostprocessPlan | None = None


class RunningJobRef(BaseModel):
    model_config = ConfigDict(extra="forbid")

    job_id: str
    kind: str
    status: Literal["pending", "running", "succeeded", "failed", "cancelled"]
    progress: float | None = None


class LastError(BaseModel):
    model_config = ConfigDict(extra="forbid")

    error_code: str
    message: str
    retryable: bool = False
    details: dict[str, Any] = Field(default_factory=dict)


class CaseState(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str
    project_id: str
    name: str
    state_version: int = 0
    status: Literal["active", "closed", "trashed"] = "active"
    pending_decision_id: str | None = None
    running_jobs: list[RunningJobRef] = Field(default_factory=list)
    last_error: LastError | None = None
    brief: Brief
    content_plan: dict[str, Any] | None = None
    audio_plan: AudioPlan | None = None
    cut_plan: CutPlan | None = None
    # Retained for the timeline candidate materializer (patch_apply/materializer);
    # the offline retrieval write-path that populated it is removed. See Task 7.
    candidate_pack_id: str | None = None
    timeline_current_version: int | None = None
    timeline_validated: bool = False
    preview_current_id: str | None = None
    last_viewed_preview_id: str | None = None
    rough_cut_approved: bool = False
    rough_cut_approved_version: int | None = None
    postprocess_plan: PostprocessPlan | None = None
    export_current_id: str | None = None
    selected_asset_ids: list[str] = Field(default_factory=list)
    disabled_asset_ids: list[str] = Field(default_factory=list)
    scratch_memory: dict[str, Any] = Field(default_factory=dict)
    messages_tail_ref: str | None = None
