from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import pytest
from apps.worker.annotation_jobs import build_annotation_handler
from apps.worker.job_registry import JobHandlerRegistry
from apps.worker.job_runner import JobRunner
from pydantic import BaseModel, ConfigDict
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from annotation.pipelines import video as video_pipeline
from annotation.shot_split import Shot, ShotSplitResult
from contracts.annotation import AnnotationDocument
from contracts.events import AssetImported, AssetLinked, JobEnqueued, ProjectCreated
from contracts.provider import ProviderDescriptor
from providers import EMBEDDING_TEXT, VLM_ANNOTATION, ProviderGateway, ProviderRegistry
from providers.mock import MockProvider
from storage import schema
from storage.db import create_workspace_engine
from storage.object_store import ObjectStore
from storage.repositories import JobsRepository
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths

FFMPEG_AVAILABLE = shutil.which("ffmpeg") is not None and shutil.which("ffprobe") is not None
NOW = "2026-07-05T00:00:00+00:00"


class EmptyConfig(BaseModel):
    model_config = ConfigDict(extra="forbid")


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
async def test_annotation_job_builds_document_projection_and_deep_merge(tmp_path: Path) -> None:
    engine, paths = _workspace_with_asset(tmp_path)
    gateway = _gateway(vlm_count=4, embedding_count=4)
    await _run_annotation_job(engine, paths, gateway, pass_="cheap")

    cheap_doc = _document(engine)
    assert cheap_doc.generator.pass_ == "cheap"
    assert _asset(engine)["annotation_status"] == "completed"
    assert _fts_search(engine, "product")

    await _run_annotation_job(engine, paths, gateway, pass_="deep")
    deep_doc = _document(engine)

    assert deep_doc.annotation_id == cheap_doc.annotation_id
    assert deep_doc.generator.pass_ == "deep"
    assert any(clip.extensions.vision_composition_v1 is not None for clip in deep_doc.clips)


@pytest.mark.skipif(not FFMPEG_AVAILABLE, reason="ffmpeg/ffprobe not installed")
@pytest.mark.ffmpeg
async def test_annotation_failure_marks_asset_failed_and_unusable(tmp_path: Path) -> None:
    engine, paths = _workspace_with_asset(tmp_path)
    gateway = _gateway(vlm_count=0, embedding_count=0)
    await _run_annotation_job(engine, paths, gateway, pass_="cheap")

    asset = _asset(engine)
    job = _job(engine)

    assert job["status"] == "failed"
    assert job["error_json"]["error_code"] == "annotation_failed"
    assert asset["annotation_status"] == "failed"
    assert asset["usable"] is False


async def test_annotation_budget_exceeded_fails_without_provider_call(tmp_path: Path) -> None:
    engine, paths = _workspace_with_asset(tmp_path, budget=0.1)
    _seed_provider_cost(engine, cost=0.2)
    gateway = _gateway(vlm_count=1, embedding_count=1)
    await _run_annotation_job(engine, paths, gateway, pass_="cheap")

    job = _job(engine)
    asset = _asset(engine)

    assert job["status"] == "failed"
    assert job["error_json"]["error_code"] == "budget_exceeded"
    assert asset["annotation_status"] == "failed"
    assert "CapabilityDegraded" in _event_types(engine)


async def test_video_pipeline_retries_once_when_vlm_json_is_invalid(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    source = tmp_path / "source.mp4"
    source.write_bytes(b"placeholder")
    monkeypatch.setattr(
        video_pipeline,
        "split_shots",
        lambda *_args, **_kwargs: ShotSplitResult(shots=(Shot("shot_1", 0, 5),)),
    )
    monkeypatch.setattr(video_pipeline, "detect_quality_events", lambda *_args, **_kwargs: ())
    monkeypatch.setattr(
        video_pipeline,
        "_keyframe_data_uris",
        lambda *_args, **_kwargs: ("data:image/jpeg;base64,AA==",),
    )
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock_vlm",
            display_name="mock_vlm",
            version="1",
            capabilities=[VLM_ANNOTATION],
            config_model=EmptyConfig,
            client_ref="tests.mock_vlm",
        ),
        MockProvider(
            provider_id="mock_vlm",
            scripts={
                VLM_ANNOTATION: [
                    {"normalized_output": {"content": "{not-json"}},
                    {
                        "normalized_output": {
                            "content": json.dumps(
                                {
                                    "summary": "retry succeeded",
                                    "keywords": ["product"],
                                }
                            )
                        }
                    },
                ]
            },
        ),
    )

    result = await video_pipeline.run_video_annotation(
        source,
        asset_id="asset_1",
        gateway=ProviderGateway(registry=registry),
    )

    assert result.document.clips[0].summary == "retry succeeded"
    assert len(result.document.generator.provider_refs) == 2


def test_annotation_enqueue_resets_failed_asset_for_retry(tmp_path: Path) -> None:
    engine, _paths = _workspace_with_asset(tmp_path)
    apply(
        [
            JobEnqueued(
                project_id="project_1",
                job_id="job_annotation_retry",
                payload={
                    "kind": "annotation",
                    "asset_id": "asset_1",
                    "idempotency_key": "retry",
                    "job_payload": {"asset_id": "asset_1", "pass": "cheap"},
                },
            )
        ],
        engine=engine,
        base_version=None,
        actor="agent",
    )

    asset = _asset(engine)

    assert asset["annotation_status"] == "pending"
    assert asset["index_status"] == "partial"
    assert asset["usable"] is False


async def _run_annotation_job(
    engine: Engine,
    paths: WorkspacePaths,
    gateway: ProviderGateway,
    *,
    pass_: str,
) -> None:
    job_id = f"job_annotation_{pass_}"
    with engine.begin() as connection:
        JobsRepository(connection).insert(
            {
                "job_id": job_id,
                "kind": "annotation",
                "status": "pending",
                "project_id": "project_1",
                "case_id": None,
                "requested_by_case_id": None,
                "asset_id": "asset_1",
                "idempotency_key": f"annotation_{pass_}",
                "payload_json": {"asset_id": "asset_1", "pass": pass_},
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
    registry = JobHandlerRegistry()
    registry.register("annotation", build_annotation_handler(engine, paths, gateway=gateway))
    await JobRunner(engine=engine, registry=registry).run_once()


def _workspace_with_asset(
    tmp_path: Path,
    *,
    budget: float | None = None,
) -> tuple[Engine, WorkspacePaths]:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    video = _make_video(tmp_path) if FFMPEG_AVAILABLE else tmp_path / "missing.mp4"
    object_ref = ObjectStore(paths).put_file(video) if FFMPEG_AVAILABLE else None
    events = [
        ProjectCreated(
            project_id="project_1",
            name="Project",
            payload={"defaults": {"annotation_budget_cny": budget}},
        ),
        AssetImported(
            project_id="project_1",
            asset_id="asset_1",
            payload={
                "storage_mode": "copy",
                "object_hash": object_ref.object_hash if object_ref is not None else None,
                "object_size": object_ref.size if object_ref is not None else None,
                "kind": "video",
                "source": "upload",
                "filename": "source.mp4",
                "hash": object_ref.object_hash if object_ref is not None else "hash",
                "mtime": 1,
                "size": object_ref.size if object_ref is not None else 1,
                "probe": {"duration_sec": 1.0, "fps": 30, "width": 160, "height": 120},
                "ingest_status": "annotating",
                "annotation_status": "failed",
                "annotation_pass": "none",
                "index_status": "none",
                "usable": False,
                "failure": {"error_code": "old", "message": "old", "retryable": True},
            },
        ),
        AssetLinked(project_id="project_1", asset_id="asset_1"),
    ]
    result = apply(events, engine=engine, base_version=None, actor="user")
    assert result.status == "applied"
    return engine, paths


def _gateway(*, vlm_count: int, embedding_count: int) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        ProviderDescriptor(
            provider_id="mock_vlm",
            display_name="mock_vlm",
            version="1",
            capabilities=[VLM_ANNOTATION],
            config_model=EmptyConfig,
            client_ref="tests.mock_vlm",
        ),
        MockProvider(
            provider_id="mock_vlm",
            scripts={
                VLM_ANNOTATION: [
                    {
                        "normalized_output": {
                            "content": json.dumps(
                                {
                                    "summary": "product closeup on desk",
                                    "keywords": ["product", "desk"],
                                    "subject_type": "product",
                                    "scene_type": "desk",
                                    "shot_type": "closeup",
                                    "composition": {
                                        "safe_area": "ok",
                                        "subtitle_occlusion_risk": "low",
                                        "subject_position": "center",
                                    },
                                }
                            )
                        }
                    }
                    for _ in range(vlm_count)
                ]
            },
        ),
    )
    registry.register(
        ProviderDescriptor(
            provider_id="mock_embedding",
            display_name="mock_embedding",
            version="1",
            capabilities=[EMBEDDING_TEXT],
            config_model=EmptyConfig,
            client_ref="tests.mock_embedding",
        ),
        MockProvider(
            provider_id="mock_embedding",
            scripts={
                EMBEDDING_TEXT: [
                    {"normalized_output": {"embedding": [0.1, 0.2, 0.3]}}
                    for _ in range(embedding_count)
                ]
            },
        ),
    )
    return ProviderGateway(registry=registry)


def _seed_provider_cost(engine: Engine, *, cost: float) -> None:
    with engine.begin() as connection:
        JobsRepository(connection).insert(
            {
                "job_id": "job_old",
                "kind": "annotation",
                "status": "succeeded",
                "project_id": "project_1",
                "case_id": None,
                "requested_by_case_id": None,
                "asset_id": "asset_1",
                "idempotency_key": "old",
                "payload_json": {},
                "result_json": {},
                "error_json": None,
                "attempts": 0,
                "max_retries": 0,
                "next_run_at": NOW,
                "progress": None,
                "worker_id": None,
                "heartbeat_at": None,
                "created_at": NOW,
                "started_at": None,
                "finished_at": NOW,
            }
        )
        connection.execute(
            schema.provider_calls.insert().values(
                call_id="pc_old",
                provider_id="mock",
                capability="vlm.annotation",
                model="mock",
                case_id=None,
                job_id="job_old",
                latency_ms=1,
                usage_json=json.dumps({}),
                cost_estimate=cost,
                status="succeeded",
            )
        )


def _asset(engine: Engine) -> dict[str, object]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == "asset_1")
        ).one()
    values = dict(row._mapping)
    values["failure"] = load_json(values["failure"])
    return values


def _job(engine: Engine) -> dict[str, object]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.jobs).where(schema.jobs.c.job_id.like("job_annotation_%"))
        ).first()
    values = dict(row._mapping)
    values["error_json"] = load_json(values["error_json"])
    return values


def _document(engine: Engine) -> AnnotationDocument:
    with engine.connect() as connection:
        row = connection.execute(select(schema.annotations_table.c.document_json)).one()
    return AnnotationDocument.model_validate(load_json(row[0]))


def _fts_search(engine: Engine, query: str) -> bool:
    with engine.connect() as connection:
        rows = connection.exec_driver_sql(
            "SELECT clip_id FROM clip_fts WHERE clip_fts MATCH ?",
            (query,),
        ).all()
    return bool(rows)


def _event_types(engine: Engine) -> list[str]:
    with engine.connect() as connection:
        rows = connection.execute(select(schema.event_log.c.event_type)).all()
    return [str(row[0]) for row in rows]


def _make_video(tmp_path: Path) -> Path:
    video = tmp_path / "source.mp4"
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
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return video


async def test_audio_kind_asset_fails_with_not_implemented_boundary(tmp_path: Path) -> None:
    engine, paths = _workspace_with_asset(tmp_path)
    audio_path = tmp_path / "voice.wav"
    audio_path.write_bytes(b"RIFFfakewav")
    with engine.begin() as connection:
        connection.execute(
            schema.assets.update()
            .where(schema.assets.c.asset_id == "asset_1")
            .values(kind="audio", storage_mode="reference", reference_path=str(audio_path))
        )
    gateway = _gateway(vlm_count=0, embedding_count=0)

    await _run_annotation_job(engine, paths, gateway, pass_="cheap")

    with engine.connect() as connection:
        asset = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == "asset_1")
        ).first()
        job = connection.execute(
            select(schema.jobs).where(schema.jobs.c.job_id == "job_annotation_cheap")
        ).first()
    assert asset is not None and job is not None
    assert asset._mapping["annotation_status"] == "failed"
    assert not asset._mapping["usable"]
    assert job._mapping["status"] == "failed"
    error = json.loads(job._mapping["error_json"])
    assert error["error_code"] == "annotation_pipeline_not_implemented"
    assert error["retryable"] is False


def test_worker_helpers_none_paths_and_invalid_pass(tmp_path: Path) -> None:
    from apps.worker.annotation_jobs import (
        _annotation_pass,
        _existing_document,
        _project_id_for_asset,
    )
    from apps.worker.job_runner import JobExecutionError

    from contracts.jobs import Job

    engine, _paths = _workspace_with_asset(tmp_path)
    assert _project_id_for_asset(engine, "asset_ghost") is None
    assert _existing_document(engine, "asset_ghost") is None

    job = Job(
        job_id="job_bad",
        kind="annotation",
        status="pending",
        project_id="project_1",
        idempotency_key="k",
        payload_json={"asset_id": "asset_1", "pass": "ultra"},
        attempts=0,
        max_retries=0,
        next_run_at=NOW,
        created_at=NOW,
    )
    with pytest.raises(JobExecutionError):
        _annotation_pass(job)
