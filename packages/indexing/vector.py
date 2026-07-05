"""Brute-force numpy vector retrieval over annotation_clip_projection embeddings."""

from __future__ import annotations

from collections.abc import Collection, Sequence
from typing import Protocol, cast

import numpy as np
from numpy.typing import NDArray
from sqlalchemy import select
from sqlalchemy.engine import Connection

from storage import schema

from .types import RankedClip


class VectorSearchAdapter(Protocol):
    def search(
        self,
        connection: Connection,
        query_vector: Sequence[float],
        *,
        limit: int,
        clip_ids: Collection[str] | None = None,
    ) -> list[RankedClip]:
        """Return vector ranks for one query vector."""


class NumpyVectorSearchAdapter:
    """Current local adapter; can be replaced by sqlite-vec later."""

    def search(
        self,
        connection: Connection,
        query_vector: Sequence[float],
        *,
        limit: int,
        clip_ids: Collection[str] | None = None,
    ) -> list[RankedClip]:
        if len(query_vector) == 0:
            return []
        rows = _embedding_rows(connection, clip_ids=clip_ids)
        if not rows:
            return []
        query = np.asarray(query_vector, dtype=np.float32)
        query_norm = float(np.linalg.norm(query))
        if query_norm == 0.0:
            return []
        clip_order: list[str] = []
        vectors: list[NDArray[np.float32]] = []
        for clip_id, blob in rows:
            vector = cast(NDArray[np.float32], np.frombuffer(blob, dtype=np.float32))
            if vector.shape != query.shape:
                continue
            clip_order.append(clip_id)
            vectors.append(vector)
        if not vectors:
            return []
        matrix = cast(NDArray[np.float32], np.vstack(vectors).astype(np.float32, copy=False))
        norms = np.linalg.norm(matrix, axis=1)
        nonzero = norms > 0.0
        if not np.any(nonzero):
            return []
        scores = np.full(matrix.shape[0], -1.0, dtype=np.float32)
        scores[nonzero] = (matrix[nonzero] @ query) / (norms[nonzero] * query_norm)
        valid_indexes = np.flatnonzero(nonzero)
        ranked_indexes = valid_indexes[np.argsort(-scores[valid_indexes], kind="stable")[:limit]]
        return [
            RankedClip(
                clip_id=clip_order[int(index)],
                rank=rank + 1,
                score=float(scores[int(index)]),
            )
            for rank, index in enumerate(ranked_indexes)
        ]


def search_cosine(
    connection: Connection,
    query_vector: Sequence[float],
    *,
    limit: int,
    clip_ids: Collection[str] | None = None,
    adapter: VectorSearchAdapter | None = None,
) -> list[RankedClip]:
    return (adapter or NumpyVectorSearchAdapter()).search(
        connection,
        query_vector,
        limit=limit,
        clip_ids=clip_ids,
    )


def _embedding_rows(
    connection: Connection,
    *,
    clip_ids: Collection[str] | None,
) -> list[tuple[str, bytes]]:
    statement = select(
        schema.annotation_clip_projection.c.clip_id,
        schema.annotation_clip_projection.c.embedding,
    ).where(schema.annotation_clip_projection.c.embedding.is_not(None))
    if clip_ids is not None:
        ordered_ids = sorted(set(clip_ids))
        if not ordered_ids:
            return []
        statement = statement.where(schema.annotation_clip_projection.c.clip_id.in_(ordered_ids))
    rows = connection.execute(statement).all()
    result: list[tuple[str, bytes]] = []
    for row in rows:
        blob = row._mapping["embedding"]
        if isinstance(blob, bytes):
            result.append((str(row._mapping["clip_id"]), blob))
        elif isinstance(blob, memoryview):
            result.append((str(row._mapping["clip_id"]), blob.tobytes()))
    return result
