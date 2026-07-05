"""FTS5 keyword retrieval over annotation clip projections."""

from __future__ import annotations

import re
from collections.abc import Collection

from sqlalchemy.engine import Connection

from .types import RankedClip

_ASCII_TOKEN_RE = re.compile(r"[A-Za-z0-9_]+")
_CJK_RE = re.compile(r"[\u3400-\u9fff]+")


def search_bm25(
    connection: Connection,
    brief: str,
    *,
    limit: int,
    clip_ids: Collection[str] | None = None,
) -> list[RankedClip]:
    """Return FTS5 BM25 ranks for a slot brief.

    The current SQLite unicode tokenizer is not a real Chinese segmenter. The query builder keeps
    contiguous CJK phrases, adds simple two-character shingles, and falls back to single characters
    so short Chinese briefs still match common annotation text without a new dependency.
    """

    query = build_match_query(brief)
    if not query:
        return []
    params: list[object] = [query]
    scope_sql = ""
    if clip_ids is not None:
        ordered_ids = sorted(set(clip_ids))
        if not ordered_ids:
            return []
        placeholders = ",".join("?" for _ in ordered_ids)
        scope_sql = f" AND clip_id IN ({placeholders})"
        params.extend(ordered_ids)
    params.append(limit)
    rows = connection.exec_driver_sql(
        (
            "SELECT clip_id, bm25(clip_fts) AS score "
            "FROM clip_fts "
            "WHERE clip_fts MATCH ?"
            f"{scope_sql} "
            "ORDER BY score ASC "
            "LIMIT ?"
        ),
        tuple(params),
    ).all()
    return [
        RankedClip(clip_id=str(row[0]), rank=index + 1, score=float(row[1]))
        for index, row in enumerate(rows)
    ]


def build_match_query(brief: str) -> str:
    tokens = _query_tokens(brief)
    return " OR ".join(_quote_fts_token(token) for token in tokens)


def _query_tokens(brief: str) -> tuple[str, ...]:
    tokens: list[str] = []
    for match in _ASCII_TOKEN_RE.finditer(brief):
        tokens.append(match.group(0).lower())
    for match in _CJK_RE.finditer(brief):
        chunk = match.group(0)
        tokens.append(chunk)
        if len(chunk) > 1:
            tokens.extend(chunk[index : index + 2] for index in range(len(chunk) - 1))
            tokens.extend(chunk)
    seen: set[str] = set()
    unique: list[str] = []
    for token in tokens:
        clean = token.strip()
        if not clean or clean in seen:
            continue
        seen.add(clean)
        unique.append(clean)
    return tuple(unique)


def _quote_fts_token(token: str) -> str:
    escaped = token.replace('"', '""')
    return f'"{escaped}"'
