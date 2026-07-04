"""Transcript document contracts."""

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class TranscriptWord(BaseModel):
    model_config = ConfigDict(extra="forbid")

    w: str
    start_ms: int
    end_ms: int
    type: Literal["filler", "word", "punct"]

    @model_validator(mode="after")
    def validate_range(self) -> "TranscriptWord":
        if self.start_ms >= self.end_ms:
            raise ValueError("word range must satisfy start_ms < end_ms")
        return self


class TranscriptUtterance(BaseModel):
    model_config = ConfigDict(extra="forbid")

    utterance_id: str
    text: str
    start_ms: int
    end_ms: int
    words: list[TranscriptWord] = Field(default_factory=list)

    @model_validator(mode="after")
    def validate_range(self) -> "TranscriptUtterance":
        if self.start_ms >= self.end_ms:
            raise ValueError("utterance range must satisfy start_ms < end_ms")
        return self


class VadSegment(BaseModel):
    model_config = ConfigDict(extra="forbid")

    start_ms: int
    end_ms: int
    kind: Literal["silence", "speech"]

    @model_validator(mode="after")
    def validate_range(self) -> "VadSegment":
        if self.start_ms >= self.end_ms:
            raise ValueError("vad range must satisfy start_ms < end_ms")
        return self


class TranscriptDocument(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    schema_: Literal["TranscriptDocument.v1"] = Field(
        default="TranscriptDocument.v1", alias="schema"
    )
    transcript_id: str
    asset_id: str
    language: str
    provider_id: str
    raw_preserved: bool
    utterances: list[TranscriptUtterance] = Field(default_factory=list)
    vad_segments: list[VadSegment] = Field(default_factory=list)
    warnings: list[str] = Field(default_factory=list)
