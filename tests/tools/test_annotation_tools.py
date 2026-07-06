from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from agent_harness.reducer import apply
from annotation.projection import (
    AnnotationProjection,
    ClipProjectionRow,
    persist_annotation_projection,
)
from contracts.annotation import (
    AnnotationClip,
    AnnotationDocument,
    AnnotationGenerator,
    QualityEvent,
)
from contracts.events import AnnotationFailed, AssetImported, AssetLinked, ProjectCreated
from contracts.project import ProjectState
from storage import schema
from storage.db import create_workspace_engine
from tools import ToolExecutionContext
from tools.annotation import enqueue, inspect, retry, status
from tools.specs import (
    AnnotationEnqueueInput,
    AnnotationInspectInput,
    AnnotationRetryInput,
    AnnotationStatusInput,
)


def test_annotation_enqueue_returns_job_enqueued() -> None:
    result = enqueue(AnnotationEnqueueInput(asset_id="asset_1"), _context())

    assert result.status == "running"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "annotation"
    assert result.events[0]["payload"]["job_payload"]["pass"] == "cheap"


def test_annotation_status_and_inspect_read_projection(tmp_path: Path) -> None:
    engine = _engine_with_failed_asset_and_annotation(tmp_path)
    with engine.connect() as connection:
        status_result = status(
            AnnotationStatusInput(project_id="project_1"),
            _context(connection=connection),
        )
        inspect_result = inspect(
            AnnotationInspectInput(project_id="project_1", asset_id="asset_1"),
            _context(connection=connection),
        )

    assert status_result.status == "succeeded"
    assert status_result.data["summary"]["failed"] == 1
    assert inspect_result.status == "succeeded"
    assert inspect_result.data["quality_events"][0]["kind"] == "blur"
    assert inspect_result.data["failure"]["error_code"] == "annotation_failed"


def test_annotation_retry_requires_failed_asset_and_requeues(tmp_path: Path) -> None:
    engine = _engine_with_failed_asset_and_annotation(tmp_path)
    with engine.connect() as connection:
        result = retry(
            AnnotationRetryInput(project_id="project_1", asset_id="asset_1"),
            _context(connection=connection),
        )

    assert result.status == "running"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["job_payload"]["pass"] == "cheap"


def test_annotation_retry_fails_for_non_failed_asset(tmp_path: Path) -> None:
    engine = _engine_with_failed_asset_and_annotation(tmp_path)
    with engine.begin() as connection:
        connection.execute(
            schema.assets.update()
            .where(schema.assets.c.asset_id == "asset_1")
            .values(annotation_status="completed")
        )
    with engine.connect() as connection:
        result = retry(
            AnnotationRetryInput(project_id="project_1", asset_id="asset_1"),
            _context(connection=connection),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "annotation_not_retryable"


def _engine_with_failed_asset_and_annotation(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    result = apply(
        [
            ProjectCreated(project_id="project_1", name="Project"),
            AssetImported(
                project_id="project_1",
                asset_id="asset_1",
                payload={
                    "storage_mode": "reference",
                    "reference_path": "/tmp/source.mp4",
                    "kind": "video",
                    "source": "local_path",
                    "filename": "source.mp4",
                    "hash": "hash",
                    "mtime": 1,
                    "size": 1,
                    "ingest_status": "failed",
                    "annotation_status": "failed",
                    "annotation_pass": "cheap",
                    "index_status": "partial",
                    "usable": False,
                    "failure": {
                        "error_code": "annotation_failed",
                        "message": "failed",
                        "retryable": True,
                    },
                },
            ),
            AssetLinked(project_id="project_1", asset_id="asset_1"),
            AnnotationFailed(
                project_id="project_1",
                asset_id="asset_1",
                payload={
                    "failure": {
                        "error_code": "annotation_failed",
                        "message": "failed",
                        "retryable": True,
                    }
                },
            ),
        ],
        engine=engine,
        base_version=None,
        actor="job",
    )
    assert result.status == "applied"
    document = AnnotationDocument(
        annotation_id="ann_asset_1",
        asset_id="asset_1",
        asset_kind="video",
        status="completed",
        generator=AnnotationGenerator(
            pipeline_version="annotation.video.v1",
            pass_="cheap",
            provider_refs=[],
        ),
        clips=[
            AnnotationClip(
                clip_id="clip_1",
                source_start_frame=0,
                source_end_frame=100,
                role="b_roll_candidate",
                summary="usable",
            )
        ],
        quality_events=[
            QualityEvent(
                event_id="q_1",
                kind="blur",
                severity="hard",
                start_frame=10,
                end_frame=20,
            )
        ],
        created_at="2026-07-05T00:00:00+00:00",
    )
    projection = AnnotationProjection(
        clips=(
            ClipProjectionRow(
                clip_id="clip_1",
                annotation_id="ann_asset_1",
                asset_id="asset_1",
                start_frame=0,
                end_frame=100,
                role="b_roll_candidate",
                summary="usable",
                keywords_json=json.dumps([]),
                quality_score=None,
                usable=True,
                embedding=None,
                retrieval_sentence="usable",
                ocr_text="",
            ),
        ),
        signals=(),
    )
    with engine.begin() as connection:
        persist_annotation_projection(connection, document, projection)
    return engine


def _context(*, connection: Any | None = None) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        project_state=ProjectState.model_validate(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "asset_links": [],
                "case_ids": [],
                "memory_ids": [],
                "created_at": "2026-07-05T00:00:00+00:00",
                "updated_at": "2026-07-05T00:00:00+00:00",
            }
        ),
        readonly_connection=connection,
    )
