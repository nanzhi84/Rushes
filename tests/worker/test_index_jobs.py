from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from apps.worker import index_jobs
from apps.worker.index_jobs import build_index_handler
from apps.worker.media_jobs import _index_job_event
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.asset import StorageMode
from contracts.events import AssetImported, ProjectCreated
from contracts.jobs import Job
from contracts.transcript import VadSegment
from media.vad import SileroModelMissing, VadResult
from storage import schema
from storage.db import create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-06T00:00:00+00:00"
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


def _ingest(tmp_path: Path, *, kind: str, source: Path) -> tuple[Engine, WorkspacePaths, str]:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    ref = ObjectStore(paths).put_file(source)
    asset_id = "asset_1"
    events: list[Any] = [
        ProjectCreated(project_id="project_1", name="Project"),
        AssetImported(
            project_id="project_1",
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
        job_id="job_index_1",
        kind="index",
        project_id="project_1",
        asset_id=asset_id,
        idempotency_key=f"asset:{asset_id}:index",
        payload_json={"asset_id": asset_id},
        created_at=NOW,
    )


@ffmpeg_only
async def test_index_video_writes_shots_and_thumbnail(tmp_path: Path) -> None:
    source = tmp_path / "clip.mp4"
    _make_video(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="video", source=source)

    result = await build_index_handler(engine, paths)(_job(asset_id))

    assert result.result_json["index_status"] == "indexed"
    row = _asset_row(engine, asset_id)
    assert row["ingest_status"] == "indexed"
    assert isinstance(row["thumbnail_object_hash"], str)
    assert paths.object_path(row["thumbnail_object_hash"]).exists()
    index_json = load_json(row["index_json"])
    assert "shots" in index_json
    assert index_json["duration_sec"] > 0


@ffmpeg_only
async def test_index_audio_writes_peaks_and_empty_vad(tmp_path: Path) -> None:
    source = tmp_path / "audio.wav"
    _make_audio(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="audio", source=source)

    await build_index_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    assert row["ingest_status"] == "indexed"
    assert row["thumbnail_object_hash"] is None
    index_json = load_json(row["index_json"])
    assert len(index_json["peaks"]) > 0
    assert index_json["vad"] == []  # no Silero model in test workspace -> graceful empty


@ffmpeg_only
async def test_index_image_writes_thumbnail(tmp_path: Path) -> None:
    source = tmp_path / "still.png"
    _make_image(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="image", source=source)

    await build_index_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    assert row["ingest_status"] == "indexed"
    assert isinstance(row["thumbnail_object_hash"], str)


async def test_index_font_writes_metadata(tmp_path: Path) -> None:
    source = tmp_path / "font.ttf"
    _make_font(source)
    engine, paths, asset_id = _ingest(tmp_path, kind="font", source=source)

    await build_index_handler(engine, paths)(_job(asset_id))

    row = _asset_row(engine, asset_id)
    assert row["ingest_status"] == "indexed"
    assert row["thumbnail_object_hash"] is None
    index_json = load_json(row["index_json"])
    assert index_json["font_meta"]["family"] == "RushesFont"


async def test_index_failure_emits_index_failed(tmp_path: Path) -> None:
    junk = tmp_path / "broken.mp4"
    junk.write_bytes(b"not a real video")
    engine, paths, asset_id = _ingest(tmp_path, kind="video", source=junk)

    result = await build_index_handler(engine, paths)(_job(asset_id))

    assert result.result_json["index_status"] == "failed"
    row = _asset_row(engine, asset_id)
    # 索引失败不影响素材可用，也不推进 ingest_status。
    assert row["ingest_status"] != "indexed"
    failure = load_json(row["failure"])
    assert failure["error_code"] == "index_failed"


def test_index_job_event_idempotency_key() -> None:
    event = _index_job_event(project_id="project_1", asset_id="asset_9")

    assert event.payload["kind"] == "index"
    assert event.payload["idempotency_key"] == "asset:asset_9:index"
    assert event.job_id.startswith("job_")


def test_run_vad_returns_empty_when_model_absent(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.delenv("RUSHES_SILERO_VAD_MODEL", raising=False)
    paths = WorkspacePaths.from_root(tmp_path / "ws").initialize()

    assert index_jobs._run_vad(tmp_path / "any.wav", paths) == []


def test_run_vad_degrades_when_model_missing_after_transcode(tmp_path: Path, monkeypatch) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "ws").initialize()
    monkeypatch.setattr(index_jobs, "_vad_model_present", lambda _paths: True)
    monkeypatch.setattr(
        index_jobs,
        "_transcode_wav_16k_mono",
        lambda source, dest, **_kwargs: Path(dest).write_bytes(b""),
    )

    def _raise(*_args: Any, **_kwargs: Any) -> VadResult:
        raise SileroModelMissing("missing")

    monkeypatch.setattr(index_jobs, "run_silero_vad", _raise)

    assert index_jobs._run_vad(tmp_path / "any.wav", paths) == []


def test_run_vad_maps_segments(tmp_path: Path, monkeypatch) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "ws").initialize()
    monkeypatch.setattr(index_jobs, "_vad_model_present", lambda _paths: True)
    monkeypatch.setattr(
        index_jobs,
        "_transcode_wav_16k_mono",
        lambda source, dest, **_kwargs: Path(dest).write_bytes(b""),
    )
    segments = [VadSegment(start_ms=0, end_ms=500, kind="speech")]
    monkeypatch.setattr(
        index_jobs,
        "run_silero_vad",
        lambda *_args, **_kwargs: VadResult(segments=segments, speech_ratio=1.0),
    )

    vad = index_jobs._run_vad(tmp_path / "any.wav", paths)

    assert vad == [{"start_ms": 0, "end_ms": 500, "kind": "speech"}]


def test_vad_model_present_honors_env(tmp_path: Path, monkeypatch) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "ws").initialize()
    model = tmp_path / "silero.onnx"
    monkeypatch.setenv("RUSHES_SILERO_VAD_MODEL", str(model))
    assert index_jobs._vad_model_present(paths) is False
    model.write_bytes(b"stub")
    assert index_jobs._vad_model_present(paths) is True
