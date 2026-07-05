"""Shared ranking types for retrieval indexes."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class RankedClip:
    clip_id: str
    rank: int
    score: float


@dataclass(frozen=True, slots=True)
class FusedClip:
    clip_id: str
    rrf: float
    bm25_rank: int | None = None
    vector_rank: int | None = None
