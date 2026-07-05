"""Reciprocal-rank fusion for keyword and vector search results."""

from __future__ import annotations

from collections.abc import Iterable

from .types import FusedClip, RankedClip


def fuse_rankings(
    keyword: Iterable[RankedClip],
    vector: Iterable[RankedClip],
    *,
    k: int = 60,
) -> list[FusedClip]:
    scores: dict[str, float] = {}
    bm25_ranks: dict[str, int] = {}
    vector_ranks: dict[str, int] = {}
    for item in keyword:
        scores[item.clip_id] = scores.get(item.clip_id, 0.0) + 1.0 / (k + item.rank)
        bm25_ranks[item.clip_id] = item.rank
    for item in vector:
        scores[item.clip_id] = scores.get(item.clip_id, 0.0) + 1.0 / (k + item.rank)
        vector_ranks[item.clip_id] = item.rank
    return [
        FusedClip(
            clip_id=clip_id,
            rrf=score,
            bm25_rank=bm25_ranks.get(clip_id),
            vector_rank=vector_ranks.get(clip_id),
        )
        for clip_id, score in sorted(
            scores.items(),
            key=lambda item: (-item[1], bm25_ranks.get(item[0], 10**9), item[0]),
        )
    ]
