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


def revalidate_draft_references(
    engine: Engine,
    draft_id: str,
    *,
    apply_events: Callable[..., Any],
) -> InvalidationResult:
    """media 属 IMPL 层不得 import agent_harness（§15）；Reducer 的 apply 由调用方注入。"""
    rows = _reference_asset_rows(engine, draft_id)
    invalidated: list[str] = []
    for row in rows:
        asset_id = str(row["asset_id"])
        if _is_reference_invalid(row):
            event = AssetInvalidated(
                draft_id=draft_id,
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


def _reference_asset_rows(engine: Engine, draft_id: str) -> list[dict[str, Any]]:
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
                    schema.draft_asset_links,
                    schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.draft_asset_links.c.draft_id == draft_id)
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
    row_hash = row.get("hash")
    if isinstance(row_hash, str) and row_hash.startswith("pending:"):
        # canonical sha256 未就绪：推迟失效判定，挂起期一律不判失效。
        # 权衡：hash job 是最低优先级、批量导入空窗可达分钟级，期间 iCloud/同步工具只 touch
        # mtime（内容没变）就会被 size/mtime 一变即失效的旧逻辑永久杀掉（usable=False 无恢复路径）。
        # 宁可挂起期漏判，等 canonical hash 就绪（hash job 以当刻快照刷新三列）后再恢复完整检测。
        return False
    return _sha256(path) != row_hash


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(CHUNK_SIZE), b""):
            digest.update(chunk)
    return digest.hexdigest()
