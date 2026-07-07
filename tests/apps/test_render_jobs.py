from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from apps.worker import render_jobs
from apps.worker.job_registry import build_default_job_registry
from apps.worker.job_runner import JobRunner
from apps.worker.render_jobs import _JobProgressReporter
from sqlalchemy import select

from agent_harness.reducer import apply
from contracts.events import JobEnqueued, PreviewRendered
from contracts.jobs import Job
from contracts.timeline import TimelineState
from media.segment_render import TimelineRenderOutput
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories import DraftsRepository, JobsRepository
from storage.repositories._json import dump_json, load_json
from storage.workspace_paths import WorkspacePaths
from timeline import store_timeline_version

NOW = "2026-07-05T00:00:00+00:00"


@pytest.mark.ffmpeg
@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
async def test_render_preview_job_writes_preview_and_current_pointer(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    source = tmp_path / "source.mp4"
    _make_fixture(source)
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft_assets(connection, source)
        store_timeline_version(connection, _timeline(), created_at=NOW)
    enqueued = JobEnqueued(
        job_id="job_render_preview",
        draft_id="draft_1",
        requested_by_draft_id="draft_1",
        payload={
            "kind": "render_preview",
            "idempotency_key": "draft:draft_1:render_preview:test",
            "job_payload": {
                "tool_name": "render.preview",
                "arguments": {},
                "tool_call_id": "tc_render",
                "turn_id": "turn_1",
            },
            "attempts": 0,
            "max_retries": 0,
        },
    )
    reducer_result = apply(
        (enqueued,),
        engine=engine,
        base_version=None,
        actor="agent",
        created_at=NOW,
    )
    assert reducer_result.status == "applied"

    runner = JobRunner(
        engine=engine,
        registry=build_default_job_registry(engine=engine, workspace_paths=paths),
        heartbeat_interval_seconds=0.01,
    )
    result = await runner.run_once()

    with engine.connect() as connection:
        draft_row = DraftsRepository(connection).get("draft_1")
        preview_row = connection.execute(select(schema.previews)).one()._mapping
    assert result.status == "succeeded"
    assert draft_row is not None
    assert draft_row["preview_current_id"] == preview_row["preview_id"]
    assert preview_row["timeline_version"] == 1
    assert paths.object_path(preview_row["object_hash"]).exists()


async def test_render_final_job_writes_export_and_current_pointer(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    source = tmp_path / "source.mp4"
    source.write_bytes(b"fake source")
    with engine.begin() as connection:
        schema.create_all(connection)
        _seed_draft_assets(connection, source)
        store_timeline_version(connection, _timeline(), created_at=NOW)
    preview_ref = ObjectStore(paths).put_bytes(b"preview")
    preview_result = apply(
        (
            PreviewRendered(
                draft_id="draft_1",
                timeline_version=1,
                artifact_id="preview_current",
                payload={"object_hash": preview_ref.object_hash},
            ),
        ),
        engine=engine,
        base_version=None,
        actor="job",
        created_at=NOW,
    )
    assert preview_result.status == "applied"

    async def fake_render_final_mp4(*args: Any, **kwargs: Any) -> Any:
        del args
        output_path = kwargs["output_path"]
        output_path.write_bytes(b"final mp4")
        return TimelineRenderOutput(output_path=output_path, rendered_segments=())

    monkeypatch.setattr(render_jobs, "render_final_mp4", fake_render_final_mp4)
    enqueued = JobEnqueued(
        job_id="job_render_final",
        draft_id="draft_1",
        requested_by_draft_id="draft_1",
        payload={
            "kind": "render_final",
            "idempotency_key": "draft:draft_1:render_final:test",
            "job_payload": {
                "tool_name": "render.final_mp4",
                "arguments": {},
                "tool_call_id": "tc_export",
                "turn_id": "turn_1",
            },
            "attempts": 0,
            "max_retries": 0,
        },
    )
    reducer_result = apply(
        (enqueued,),
        engine=engine,
        base_version=None,
        actor="agent",
        created_at=NOW,
    )
    assert reducer_result.status == "applied"

    runner = JobRunner(
        engine=engine,
        registry=build_default_job_registry(engine=engine, workspace_paths=paths),
        heartbeat_interval_seconds=0.01,
    )
    result = await runner.run_once()

    with engine.connect() as connection:
        draft_row = DraftsRepository(connection).get("draft_1")
        export_row = connection.execute(select(schema.exports)).one()._mapping
    assert result.status == "succeeded"
    assert draft_row is not None
    assert draft_row["export_current_id"] == export_row["export_id"]
    assert export_row["timeline_version"] == 1
    assert paths.object_path(export_row["object_hash"]).read_bytes() == b"final mp4"


async def test_job_progress_reporter_throttles_non_final_updates(tmp_path: Path) -> None:
    engine = _engine_with_draft(tmp_path)
    job = Job.model_validate(
        {
            "job_id": "job_progress",
            "kind": "render_preview",
            "status": "running",
            "draft_id": "draft_1",
            "requested_by_draft_id": "draft_1",
            "idempotency_key": "progress",
            "payload_json": {},
            "attempts": 0,
            "max_retries": 0,
            "created_at": NOW,
        }
    )
    with begin_immediate(engine) as connection:
        JobsRepository(connection).insert(
            {
                "job_id": "job_progress",
                "kind": "render_preview",
                "status": "running",
                "draft_id": "draft_1",
                "requested_by_draft_id": "draft_1",
                "asset_id": None,
                "idempotency_key": "progress",
                "payload_json": {},
                "result_json": None,
                "error_json": None,
                "attempts": 0,
                "max_retries": 0,
                "next_run_at": NOW,
                "progress": None,
                "worker_id": None,
                "heartbeat_at": None,
                "created_at": NOW,
                "started_at": None,
                "finished_at": None,
            }
        )
    reporter = _JobProgressReporter(engine, job, min_interval_seconds=1.0)

    await reporter.emit(0.2)
    await reporter.emit(0.4)
    await reporter.emit(1.0, force=True)

    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.event_log.c.payload_json).where(
                schema.event_log.c.event_type == "JobProgress"
            )
        ).all()
        job_row = connection.execute(
            select(schema.jobs).where(schema.jobs.c.job_id == "job_progress")
        ).one()

    payloads = [load_json(row._mapping["payload_json"]) for row in rows]
    assert len(payloads) == 2
    assert [payload["progress"] for payload in payloads] == [0.2, 1.0]
    assert job_row._mapping["progress"] == 1.0


def _seed_draft_assets(connection, source: Path) -> None:
    connection.execute(
        schema.drafts.insert().values(
            draft_id="draft_1",
            name="Draft",
            state_version=0,
            status="active",
            defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
            timeline_current_version=1,
            timeline_validated=True,
            running_jobs="[]",
            brief=dump_json({"goal": "test", "confirmed_facts": []}),
            scratch_memory="{}",
            created_at=NOW,
            updated_at=NOW,
        )
    )
    connection.execute(
        schema.assets.insert().values(
            asset_id="asset_1",
            storage_mode="reference",
            object_hash=None,
            reference_path=str(source),
            kind="video",
            source="local_path",
            filename="source.mp4",
            hash="hash_asset_1",
            mtime=source.stat().st_mtime_ns,
            size=source.stat().st_size,
            probe=dump_json({"duration_sec": 1.0, "fps": 30.0, "has_audio": False}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id="asset_1",
            linked_at=NOW,
            note="",
        )
    )


def _engine_with_draft(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                timeline_current_version=1,
                timeline_validated=True,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id="draft_1:v1",
                draft_id="draft_1",
                version=1,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(
                    {
                        "timeline_id": "draft_1:v1",
                        "draft_id": "draft_1",
                        "version": 1,
                        "fps": 30,
                        "duration_frames": 30,
                        "tracks": [
                            {
                                "track_id": "visual_base",
                                "track_type": "primary_visual",
                                "clips": [],
                            },
                            {
                                "track_id": "visual_overlay",
                                "track_type": "visual_overlay",
                                "clips": [],
                            },
                            {"track_id": "original_audio", "track_type": "audio", "clips": []},
                            {"track_id": "voiceover", "track_type": "audio", "clips": []},
                            {"track_id": "bgm", "track_type": "audio", "clips": []},
                            {"track_id": "subtitles", "track_type": "text", "clips": []},
                        ],
                        "parent_version": None,
                        "created_by_patch_id": None,
                        "validation_report": None,
                    }
                ),
                validation_report=None,
                created_at=NOW,
            )
        )
    return engine


def _make_fixture(path: Path) -> None:
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=160x160:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(path),
        ],
        check=True,
        capture_output=True,
        text=True,
    )


def _timeline() -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "draft_1:v1",
            "draft_id": "draft_1",
            "version": 1,
            "fps": 30,
            "duration_frames": 30,
            "tracks": [
                {
                    "track_id": "visual_base",
                    "track_type": "primary_visual",
                    "clips": [
                        {
                            "timeline_clip_id": "tc_1",
                            "track_id": "visual_base",
                            "asset_id": "asset_1",
                            "clip_id": "clip_1",
                            "role": "b_roll",
                            "timeline_start_frame": 0,
                            "timeline_end_frame": 30,
                            "source_start_frame": 0,
                            "source_end_frame": 30,
                        }
                    ],
                },
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
            "validation_report": {"valid": True, "checks": []},
        }
    )
