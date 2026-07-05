"""Annotation document contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class AnnotationGenerator(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    pipeline_version: str
    pass_: Literal["cheap", "deep"] = Field(alias="pass")
    provider_refs: list[str] = Field(default_factory=list)


class VisionBasicExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    subject_type: str | None = None
    scene_type: str | None = None
    action: str | None = None
    contains_face: bool | None = None
    shot_type: str | None = None


class VisionCompositionExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    safe_area: Literal["ok", "overflow", "unknown"] = "unknown"
    subtitle_occlusion_risk: Literal["none", "low", "medium", "high", "unknown"] = "unknown"
    subject_position: str | None = None
    framing_notes: list[str] = Field(default_factory=list)


class AudioSpeechExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    has_speech: bool = False
    transcript_summary: str | None = None
    speech_rate_wpm: float | None = None
    pauses_ms: list[tuple[int, int]] = Field(default_factory=list)
    filler_words: list[str] = Field(default_factory=list)
    emotion: str | None = None

    @model_validator(mode="after")
    def validate_pauses(self) -> "AudioSpeechExtension":
        for start, end in self.pauses_ms:
            if start >= end:
                raise ValueError("pauses_ms ranges must satisfy start < end")
        return self


class AudioMusicExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    beat_times_ms: list[int] = Field(default_factory=list)
    energy: float | None = None
    mood: str | None = None
    cut_points_ms: list[int] = Field(default_factory=list)


class CommerceProductExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    product_category: str | None = None
    packaging_visible: bool | None = None
    selling_points: list[str] = Field(default_factory=list)
    brand_visible: bool | None = None


class OcrTextBox(BaseModel):
    model_config = ConfigDict(extra="forbid")

    text: str
    bbox: tuple[float, float, float, float] | None = None
    readability: Literal["good", "ok", "poor", "unknown"] = "unknown"
    occlusion_risk: Literal["none", "low", "medium", "high", "unknown"] = "unknown"


class TextOcrExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    texts: list[OcrTextBox] = Field(default_factory=list)
    full_text: str | None = None


class EditingAffordanceExtension(BaseModel):
    model_config = ConfigDict(extra="forbid")

    good_for: list[str] = Field(default_factory=list)
    transition_before: bool | None = None
    transition_after: bool | None = None
    climax_score: float | None = None
    ending_score: float | None = None
    buildup_score: float | None = None


class AnnotationExtensions(BaseModel):
    model_config = ConfigDict(extra="allow", populate_by_name=True)

    vision_basic_v1: VisionBasicExtension | None = Field(default=None, alias="vision.basic.v1")
    vision_composition_v1: VisionCompositionExtension | None = Field(
        default=None, alias="vision.composition.v1"
    )
    audio_speech_v1: AudioSpeechExtension | None = Field(default=None, alias="audio.speech.v1")
    audio_music_v1: AudioMusicExtension | None = Field(default=None, alias="audio.music.v1")
    commerce_product_v1: CommerceProductExtension | None = Field(
        default=None, alias="commerce.product.v1"
    )
    text_ocr_v1: TextOcrExtension | None = Field(default=None, alias="text.ocr.v1")
    editing_affordance_v1: EditingAffordanceExtension | None = Field(
        default=None, alias="editing.affordance.v1"
    )

    @model_validator(mode="after")
    def validate_unknown_extensions(self) -> "AnnotationExtensions":
        extra = self.__pydantic_extra__ or {}
        for namespace, value in extra.items():
            if not isinstance(value, dict):
                raise ValueError(f"unknown extension {namespace} must be a dict")
        return self


class AnnotationClip(BaseModel):
    model_config = ConfigDict(extra="forbid")

    clip_id: str
    source_start_frame: int
    source_end_frame: int
    role: Literal["a_roll_candidate", "b_roll_candidate", "image_candidate", "avoid"]
    summary: str
    keywords: list[str] = Field(default_factory=list)
    quality_score: float | None = None
    hard_quality_event_ids: list[str] = Field(default_factory=list)
    soft_quality_event_ids: list[str] = Field(default_factory=list)
    extensions: AnnotationExtensions = Field(default_factory=AnnotationExtensions)

    @model_validator(mode="after")
    def validate_frame_range(self) -> "AnnotationClip":
        if self.source_start_frame >= self.source_end_frame:
            raise ValueError("clip frame range must satisfy source_start_frame < source_end_frame")
        return self


class QualityEvent(BaseModel):
    model_config = ConfigDict(extra="forbid")

    event_id: str
    kind: Literal["blur", "shake", "camera_drop", "occlusion"]
    severity: Literal["hard", "soft"]
    start_frame: int
    end_frame: int

    @model_validator(mode="after")
    def validate_frame_range(self) -> "QualityEvent":
        if self.start_frame >= self.end_frame:
            raise ValueError("quality event range must satisfy start_frame < end_frame")
        return self


class AnnotationDocument(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["AnnotationDocument.v1"] = Field(
        default="AnnotationDocument.v1", alias="schema"
    )
    annotation_id: str
    asset_id: str
    asset_kind: Literal["video", "image", "audio", "voiceover", "bgm", "font", "subtitle_template"]
    status: Literal["pending", "analyzing", "completed", "failed"]
    generator: AnnotationGenerator
    clips: list[AnnotationClip] = Field(default_factory=list)
    asset_level_extensions: AnnotationExtensions = Field(default_factory=AnnotationExtensions)
    quality_events: list[QualityEvent] = Field(default_factory=list)
    evidence_frames: list[str] = Field(default_factory=list)
    created_at: str
