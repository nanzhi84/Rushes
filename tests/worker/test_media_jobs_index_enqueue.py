from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from apps.worker.media_jobs import build_proxy_handler
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.asset import StorageMode
from contracts.events import AssetImported, ProjectCreated
from contracts.jobs import Job
from storage import schema
from storage.db import create_workspace_engine
from storage.object_store import ObjectStore
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-06T00:00:00+00:00"
FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None


def _ingest(tmp_path: Path, *, kind: str, source: Path) -> tuple[Engine, WorkspacePaths, str]:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    ref = ObjectStore(paths).put_file(source)
    events: list[Any] = [
        ProjectCreated(project_id="project_1", name="Project"),
        AssetImported(
            project_id="project_1",
            asset_id="asset_1",
            payload={
                "storage_mode": StorageMode.COPY.value,
                "object_hash": ref.object_hash,
                "object_size": ref.size,
                "kind": kind,
                "filename": source.name,
                "hash": ref.object_hash,
                "size": ref.size,
                "mtime": 1,
                "usable": True,
            },
        ),
    ]
    assert apply(events, engine=engine, base_version=None, actor="job").status == "applied"
    return engine, paths, "asset_1"


def _proxy_job(asset_id: str) -> Job:
    return Job(
        job_id="job_proxy_1",
        kind="proxy",
        project_id="project_1",
        asset_id=asset_id,
        idempotency_key=f"asset:{asset_id}:probe_proxy",
        payload_json={"asset_id": asset_id},
        created_at=NOW,
    )


def _index_jobs(engine: Engine) -> list[dict[str, Any]]:
    with engine.connect() as connection:
        rows = connection.execute(select(schema.jobs).where(schema.jobs.c.kind == "index")).all()
    return [dict(row._mapping) for row in rows]


def _make_font(path: Path) -> None:
    from fontTools.fontBuilder import FontBuilder
    from fontTools.pens.ttGlyphPen import TTGlyphPen
    from fontTools.ttLib.tables._g_l_y_f import Glyph

    fb = FontBuilder(1000, isTTF=True)
    fb.setupGlyphOrder([".notdef", "A"])
    fb.setupCharacterMap({0x41: "A"})
    pen = TTGlyphPen(None)
    pen.moveTo((0, 0))
    pen.lineTo((0, 500))
    pen.lineTo((500, 500))
    pen.closePath()
    fb.setupGlyf({".notdef": Glyph(), "A": pen.glyph()})
    fb.setupHorizontalMetrics({".notdef": (500, 0), "A": (500, 0)})
    fb.setupHorizontalHeader(ascent=800, descent=-200)
    fb.setupNameTable({"familyName": "RushesFont", "styleName": "Regular"})
    fb.setupOS2()
    fb.setupPost()
    fb.save(str(path))


async def test_proxy_handler_font_skips_proxy_and_enqueues_index(tmp_path: Path) -> None:
    source = tmp_path / "font.ttf"
    _make_font(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="font", source=source)

    result = await build_proxy_handler(engine, paths)(_proxy_job(asset_id))

    assert result.result_json["index_enqueued"] is True
    index_jobs = _index_jobs(engine)
    assert len(index_jobs) == 1
    assert index_jobs[0]["idempotency_key"] == f"asset:{asset_id}:index"
    with engine.connect() as connection:
        ingest_status = connection.execute(
            select(schema.assets.c.ingest_status).where(schema.assets.c.asset_id == asset_id)
        ).scalar_one()
    assert ingest_status == "imported"  # font has no proxy: no probe/proxy state change


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")
async def test_proxy_handler_video_enqueues_index_after_proxy(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=160x120:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(source),
        ],
        check=True,
        capture_output=True,
    )
    engine, paths, asset_id = _ingest(tmp_path, kind="video", source=source)

    result = await build_proxy_handler(engine, paths)(_proxy_job(asset_id))

    assert isinstance(result.result_json["proxy_object_hash"], str)
    assert len(_index_jobs(engine)) == 1
