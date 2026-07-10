from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

from apps.worker.hash_jobs import build_hash_handler
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import AssetImported, DraftCreated
from contracts.jobs import Job
from media import invalidation
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


async def test_hash_job_snapshots_stat_with_hash(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"v0")
    engine, paths, asset_id = _ingest_reference(tmp_path, source)
    # 导入后、hash job 跑前文件被改写：新内容 + 新 mtime/size。
    source.write_bytes(b"v1-longer-bytes")
    new_stat = source.stat()

    await build_hash_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    # 三列同刻快照：hash 与 mtime/size 都描述改写后的新文件（而非导入时的旧值）。
    assert row["hash"] == hashlib.sha256(b"v1-longer-bytes").hexdigest()
    assert row["mtime"] == new_stat.st_mtime_ns
    assert row["size"] == new_stat.st_size
    # 未再变的文件失效检测判有效（快路径命中）；再改一次则判失效。
    assert invalidation._is_reference_invalid(row) is False
    source.write_bytes(b"v2-even-longer-bytes")
    assert invalidation._is_reference_invalid(_asset_row(engine, asset_id)) is True


async def test_hash_job_missing_file_degrades_without_failing(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    source.write_bytes(b"gone")
    engine, paths, asset_id = _ingest_reference(tmp_path, source)
    before = _asset_row(engine, asset_id)
    source.unlink()

    # best-effort：文件读失败只降级返回、job 成功，绝不抛异常/发 JobFailed 阴杀完全可用的素材。
    result = await build_hash_handler(engine, paths)(_job(asset_id))

    assert result.result_json["hash_status"] == "skipped"
    after = _asset_row(engine, asset_id)
    assert after["usable"] == before["usable"]  # 素材可用性不变
    assert after["hash"] == before["hash"]  # hash 仍是导入时的 pending 占位
