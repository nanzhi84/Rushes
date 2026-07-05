"""Reference asset invalidation checks."""

from __future__ import annotations

import hashlib
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Engine

from contracts.events import AssetInvalidated
from storage import schema
from storage.object_store import CHUNK_SIZE


@dataclass(frozen=True, slots=True)
class InvalidationResult:
    checked: int
    invalidated_asset_ids: tuple[str, ...]


def revalidate_project_references(
    engine: Engine,
    project_id: str,
    *,
    apply_events: Callable[..., Any],
) -> InvalidationResult:
    """media 属 IMPL 层不得 import agent_harness（§15）；Reducer 的 apply 由调用方注入。"""
    rows = _reference_asset_rows(engine, project_id)
    invalidated: list[str] = []
    for row in rows:
        asset_id = str(row["asset_id"])
        if _is_reference_invalid(row):
            event = AssetInvalidated(
                project_id=project_id,
                asset_id=asset_id,
                payload={
                    "failure": {
                        "error_code": "reference_invalidated",
                        "message": "reference file is missing or has changed",
                        "retryable": False,
                    }
                },
            )
            result = apply_events((event,), engine=engine, base_version=None, actor="system")
            if result.status == "applied":
                invalidated.append(asset_id)
    return InvalidationResult(checked=len(rows), invalidated_asset_ids=tuple(invalidated))


def _reference_asset_rows(engine: Engine, project_id: str) -> list[dict[str, Any]]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(
                schema.assets.c.asset_id,
                schema.assets.c.reference_path,
                schema.assets.c.hash,
                schema.assets.c.mtime,
                schema.assets.c.size,
            )
            .select_from(
                schema.assets.join(
                    schema.project_asset_links,
                    schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.project_asset_links.c.project_id == project_id)
            .where(schema.assets.c.storage_mode == "reference")
        ).all()
    return [dict(row._mapping) for row in rows]


def _is_reference_invalid(row: dict[str, Any]) -> bool:
    reference_path = row.get("reference_path")
    if not isinstance(reference_path, str) or reference_path == "":
        return True
    path = Path(reference_path).expanduser()
    try:
        stat = path.stat()
    except FileNotFoundError:
        return True
    unchanged_size = int(row.get("size") or 0) == stat.st_size
    unchanged_mtime = int(row.get("mtime") or 0) == stat.st_mtime_ns
    if unchanged_size and unchanged_mtime:
        return False
    return _sha256(path) != row.get("hash")


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(CHUNK_SIZE), b""):
            digest.update(chunk)
    return digest.hexdigest()
