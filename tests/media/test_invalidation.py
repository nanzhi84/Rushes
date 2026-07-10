from __future__ import annotations

import hashlib
from pathlib import Path

import pytest
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import AssetImported, AssetLinked, DraftCreated
from media import invalidation
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import load_json


def test_reference_revalidation_uses_mtime_fast_path_without_hash(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"same")
    engine = _engine_with_reference(tmp_path, source)

    def fail_hash(path: Path) -> str:
        raise AssertionError(f"hash fallback should not run for unchanged metadata: {path}")

    monkeypatch.setattr(invalidation, "_sha256", fail_hash)

    result = invalidation.revalidate_draft_references(engine, "draft_1", apply_events=apply)

    assert result.checked == 1
    assert result.invalidated_asset_ids == ()
    assert _asset_row(engine)["usable"] is True


def test_reference_revalidation_invalidates_when_hash_changes(tmp_path: Path) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"before")
    old_digest = _sha256(source)
    engine = _engine_with_reference(tmp_path, source, digest=old_digest, mtime=0)
    source.write_bytes(b"after!")

    result = invalidation.revalidate_draft_references(engine, "draft_1", apply_events=apply)

    assert result.invalidated_asset_ids == ("asset_1",)
    asset = _asset_row(engine)
    assert asset["usable"] is False
    failure = load_json(asset["failure"])
    assert failure["error_code"] == "reference_invalidated"


def test_reference_revalidation_pending_hash_defers_on_change(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"before")
    # pending 占位：canonical sha256 未就绪，mtime 设 0 以便重写后 stat 判为变化。
    engine = _engine_with_reference(tmp_path, source, digest="pending:6:0", mtime=0)
    source.write_bytes(b"after!")

    def fail_hash(path: Path) -> str:
        raise AssertionError(f"pending hash must skip sha256 slow path: {path}")

    monkeypatch.setattr(invalidation, "_sha256", fail_hash)

    result = invalidation.revalidate_draft_references(engine, "draft_1", apply_events=apply)

    # canonical hash 未就绪：挂起期一律不判失效，等 hash job 补齐三列后再恢复检测——
    # 否则 iCloud/同步工具在几分钟空窗里只 touch mtime 就会永久误杀素材（无恢复路径）。
    assert result.invalidated_asset_ids == ()
    assert _asset_row(engine)["usable"] is True


def test_reference_revalidation_pending_hash_valid_when_unchanged(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"same")
    stat = source.stat()
    engine = _engine_with_reference(
        tmp_path, source, digest=f"pending:{stat.st_size}:{stat.st_mtime_ns}"
    )

    def fail_hash(path: Path) -> str:
        raise AssertionError(f"unchanged metadata must skip sha256 slow path: {path}")

    monkeypatch.setattr(invalidation, "_sha256", fail_hash)

    result = invalidation.revalidate_draft_references(engine, "draft_1", apply_events=apply)

    assert result.invalidated_asset_ids == ()
    assert _asset_row(engine)["usable"] is True


def _engine_with_reference(
    tmp_path: Path,
    source: Path,
    *,
    digest: str | None = None,
    mtime: int | None = None,
) -> Engine:
    engine = create_workspace_engine(tmp_path / "workspace")
    with engine.begin() as connection:
        schema.create_all(connection)
    result = apply(
        (
            DraftCreated(draft_id="draft_1", payload={"name": "Draft", "brief": {"goal": ""}}),
            AssetImported(
                draft_id="draft_1",
                asset_id="asset_1",
                payload={
                    "storage_mode": "reference",
                    "reference_path": str(source),
                    "filename": source.name,
                    "hash": digest or _sha256(source),
                    "size": source.stat().st_size,
                    "mtime": mtime if mtime is not None else source.stat().st_mtime_ns,
                    "usable": True,
                },
            ),
            AssetLinked(draft_id="draft_1", asset_id="asset_1"),
        ),
        engine=engine,
        base_version=None,
        actor="user",
    )
    assert result.status == "applied"
    return engine


def _asset_row(engine: Engine) -> dict[str, object]:
    with engine.connect() as connection:
        row = connection.execute(select(schema.assets)).one()
    return dict(row._mapping)


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()
