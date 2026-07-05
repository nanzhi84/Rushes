from __future__ import annotations

import hashlib
from pathlib import Path

import pytest
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import AssetImported, AssetLinked, ProjectCreated
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

    result = invalidation.revalidate_project_references(engine, "project_1", apply_events=apply)

    assert result.checked == 1
    assert result.invalidated_asset_ids == ()
    assert _asset_row(engine)["usable"] is True


def test_reference_revalidation_invalidates_when_hash_changes(tmp_path: Path) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"before")
    old_digest = _sha256(source)
    engine = _engine_with_reference(tmp_path, source, digest=old_digest, mtime=0)
    source.write_bytes(b"after!")

    result = invalidation.revalidate_project_references(engine, "project_1", apply_events=apply)

    assert result.invalidated_asset_ids == ("asset_1",)
    asset = _asset_row(engine)
    assert asset["usable"] is False
    failure = load_json(asset["failure"])
    assert failure["error_code"] == "reference_invalidated"


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
            ProjectCreated(project_id="project_1", name="Project"),
            AssetImported(
                project_id="project_1",
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
            AssetLinked(project_id="project_1", asset_id="asset_1"),
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
