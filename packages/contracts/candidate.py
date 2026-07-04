"""Candidate pack contracts."""

from typing import Any

from pydantic import BaseModel, ConfigDict, Field, model_validator


class CandidatePackSnapshot(BaseModel):
    model_config = ConfigDict(extra="forbid")

    generated_at: str
    asset_scope_hash: str
    annotation_versions: dict[str, str] = Field(default_factory=dict)


class CandidateScore(BaseModel):
    model_config = ConfigDict(extra="forbid")

    bm25_rank: int
    vector_rank: int
    rrf: float


class Candidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_id: str
    asset_id: str
    clip_id: str
    summary_line: str
    score: CandidateScore


class CandidateSlot(BaseModel):
    model_config = ConfigDict(extra="forbid")

    slot_id: str
    slot_brief: str
    target_duration_sec: tuple[float, float]
    candidates: list[Candidate] = Field(default_factory=list)

    @model_validator(mode="after")
    def validate_slot(self) -> "CandidateSlot":
        start, end = self.target_duration_sec
        if start >= end:
            raise ValueError("target_duration_sec must satisfy start < end")
        if len(self.candidates) > 8:
            raise ValueError("each candidate slot may contain at most 8 candidates")
        return self


class CandidatePack(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidate_pack_id: str
    case_id: str
    query_context: dict[str, Any] = Field(default_factory=dict)
    snapshot: CandidatePackSnapshot
    slots: list[CandidateSlot] = Field(default_factory=list)
