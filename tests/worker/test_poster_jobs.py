from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from apps.worker.poster_jobs import build_poster_handler
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.asset import StorageMode
from contracts.events import AssetImported, DraftCreated
from contracts.jobs import Job
from storage import schema
from storage.db import create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-07T00:00:00+00:00"
FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None
ffmpeg_only = pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg not installed")


def _make_video(path: Path) -> None:
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
            str(path),
        ],
        check=True,
        capture_output=True,
    )


def _make_audio(path: Path) -> None:
    subprocess.run(
        ["ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", str(path)],
        check=True,
        capture_output=True,
    )


def _make_image(path: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=160x120:rate=1",
            "-frames:v",
            "1",
            str(path),
        ],
        check=True,
        capture_output=True,
    )


def _ingest(tmp_path: Path, *, kind: str, source: Path) -> tuple[Engine, WorkspacePaths, str]:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    ref = ObjectStore(paths).put_file(source)
    asset_id = "asset_1"
    events: list[Any] = [
        DraftCreated(draft_id="draft_1", payload={"name": "Draft"}),
        AssetImported(
            draft_id="draft_1",
            asset_id=asset_id,
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
        job_id="job_poster_1",
        kind="poster",
        draft_id="draft_1",
        asset_id=asset_id,
        idempotency_key=f"asset:{asset_id}:poster",
        payload_json={"asset_id": asset_id},
        created_at=NOW,
    )


@ffmpeg_only
async def test_poster_video_writes_thumbnail_and_duration(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    _make_video(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="video", source=source)

    result = await build_poster_handler(engine, paths)(_job(asset_id))

    assert result.result_json["poster_status"] == "ready"
    row = _asset_row(engine, asset_id)
    # 缩略图秒出：thumbnail_object_hash 落库、对象存在。
    assert isinstance(row["thumbnail_object_hash"], str)
    assert paths.object_path(row["thumbnail_object_hash"]).exists()
    # 时长秒出：probe 已写，且 poster 不把状态跳到 indexed。
    probe = load_json(row["probe"])
    assert probe["duration_sec"] > 0
    assert row["ingest_status"] != "indexed"


@ffmpeg_only
async def test_poster_audio_writes_duration_no_thumbnail(tmp_path: Path) -> None:
    source = tmp_path / "audio.wav"
    _make_audio(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="audio", source=source)

    await build_poster_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    assert row["thumbnail_object_hash"] is None
    probe = load_json(row["probe"])
    assert probe["duration_sec"] > 0


@ffmpeg_only
async def test_poster_image_writes_thumbnail(tmp_path: Path) -> None:
    source = tmp_path / "still.png"
    _make_image(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="image", source=source)

    await build_poster_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    assert isinstance(row["thumbnail_object_hash"], str)
    assert paths.object_path(row["thumbnail_object_hash"]).exists()


async def test_poster_degrades_without_blocking_on_broken_media(tmp_path: Path) -> None:
    junk = tmp_path / "broken.mp4"
    junk.write_bytes(b"not a real video")
    engine, paths, asset_id = _ingest(tmp_path, kind="video", source=junk)

    result = await build_poster_handler(engine, paths)(_job(asset_id))

    # 封面/时长产不出只降级，绝不发 JobFailed 把素材标记不可用。
    assert result.result_json["poster_status"] == "skipped"
    row = _asset_row(engine, asset_id)
    assert bool(row["usable"]) is True
    assert row["thumbnail_object_hash"] is None
