from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

import pytest
from apps.worker.hash_jobs import build_hash_handler
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import AssetImported, DraftCreated
from contracts.jobs import Job
from storage import schema
from storage.db import create_workspace_engine
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-10T00:00:00+00:00"


def _ingest_reference(tmp_path: Path, source: Path) -> tuple[Engine, WorkspacePaths, str]:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    stat = source.stat()
    asset_id = "asset_1"
    events: list[Any] = [
        DraftCreated(draft_id="draft_1", payload={"name": "Draft"}),
        AssetImported(
            draft_id="draft_1",
            asset_id=asset_id,
            payload={
                "storage_mode": "reference",
                "reference_path": str(source),
                "kind": "video",
                "filename": source.name,
                "hash": f"pending:{stat.st_size}:{stat.st_mtime_ns}",
                "size": stat.st_size,
                "mtime": stat.st_mtime_ns,
                "usable": True,
            },
        ),
    ]
    result = apply(events, engine=engine, base_version=None, actor="job")
    assert result.status == "applied"
    return engine, paths, asset_id


def _asset_row(engine: Engine, asset_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == asset_id)
        ).one()
    return dict(row._mapping)


def _job(asset_id: str) -> Job:
    return Job(
        job_id="job_hash_1",
        kind="hash",
        draft_id="draft_1",
        asset_id=asset_id,
        idempotency_key=f"asset:{asset_id}:hash",
        payload_json={"asset_id": asset_id},
        created_at=NOW,
    )


async def test_hash_job_computes_canonical_sha256(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"reference-bytes")
    engine, paths, asset_id = _ingest_reference(tmp_path, source)

    result = await build_hash_handler(engine, paths)(_job(asset_id))

    expected = hashlib.sha256(b"reference-bytes").hexdigest()
    assert result.result_json["hash"] == expected
    # AssetHashComputed 物化后 pending 占位被真 sha256 覆盖。
    assert _asset_row(engine, asset_id)["hash"] == expected


async def test_hash_job_raises_when_file_missing(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"gone")
    engine, paths, asset_id = _ingest_reference(tmp_path, source)
    source.unlink()

    # 文件读失败抛异常，交给 job 重试机制（runner 视为 retryable）。
    with pytest.raises(FileNotFoundError):
        await build_hash_handler(engine, paths)(_job(asset_id))
