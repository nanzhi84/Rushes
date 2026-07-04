"""Row conversion helpers."""

from __future__ import annotations

from typing import Any

from sqlalchemy import Row


def row_to_dict(row: Row[Any] | None) -> dict[str, Any] | None:
    if row is None:
        return None
    return dict(row._mapping)


def rows_to_dicts(rows: list[Row[Any]]) -> list[dict[str, Any]]:
    return [dict(row._mapping) for row in rows]
