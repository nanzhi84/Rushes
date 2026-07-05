from __future__ import annotations

import hashlib
import shutil
import subprocess
from pathlib import Path
from typing import Any

import httpx
import pytest
from apps.api.main import create_app
from apps.worker.job_registry import build_default_job_registry
from apps.worker.job_runner import JobRunner
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import (
    AnnotationFailed,
    AssetImported,
    AssetLinked,
    CaseCreated,
    ProjectCreated,
)
from storage import schema
from storage.db import begin_immediate
from storage.object_store import ObjectStore
from storage.repositories import EventLogRepository, JobsRepository
from storage.repositories._json import dump_json, load_json
from storage.workspace_paths import WorkspacePaths, resolve_asset_path

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_reference_import_lists_material_but_not_tree_or_object_copy(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    source = _media_file(tmp_path, b"raw-media")
    assert _create_project(client).status_code == 201

    imported = client.post(
        "/api/projects/project_1/materials/import-local",
        headers=AUTH,
        json={"path": str(source)},
    )
    materials = client.get("/api/projects/project_1/materials", headers=AUTH)
    tree = client.get("/api/project-tree", headers=AUTH)

    assert imported.status_code == 200
    assert materials.status_code == 200
    asset = materials.json()["assets"][0]
    assert asset["storage_mode"] == "reference"
    assert asset["filename"] == "raw.mp4"
    assert asset["proxy_ready"] is False
    assert asset["asset_id"] not in tree.text
    with _engine(app).connect() as connection:
        row = connection.execute(select(schema.assets)).one()._mapping
        object_count = connection.execute(
            select(func.count()).select_from(schema.objects)
        ).scalar_one()
    assert row["object_hash"] is None
    assert row["reference_path"] == str(source)
    assert object_count == 0


def test_reference_source_change_invalidates_on_materials_list(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    source = _media_file(tmp_path, b"before")
    assert _create_project(client).status_code == 201
    assert (
        client.post(
            "/api/projects/project_1/materials/import-local",
            headers=AUTH,
            json={"path": str(source)},
        ).status_code
        == 200
    )
    source.write_bytes(b"after-change")

    response = client.get("/api/projects/project_1/materials", headers=AUTH)

    assert response.status_code == 200
    asset = response.json()["assets"][0]
    assert asset["usable"] is False
    assert asset["invalid"] is True
    assert response.json()["invalidated_asset_ids"] == [asset["asset_id"]]
    assert "AssetInvalidated" in _event_types(app)


def test_import_url_route_creates_project_decision_and_answer_enqueues_project_job(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_project(client).status_code == 201

    created = client.post(
        "/api/projects/project_1/materials/import-url",
        headers=AUTH,
        json={"url": "https://example.test/clip.mp4", "filename": "clip.mp4"},
    )
    decision_id = created.json()["decision_id"]
    answered = client.post(
        f"/api/decisions/{decision_id}/answer",
        headers=AUTH,
        json={
            "project_id": "project_1",
            "answer": {
                "option_id": "approve",
                "answered_via": "button",
                "payload": {"approved": True},
            },
        },
    )

    assert created.status_code == 200
    assert answered.status_code == 200
    assert answered.json()["replays_enqueued"] == 1
    with _engine(app).connect() as connection:
        job = connection.execute(select(schema.jobs)).one()._mapping
    assert job["kind"] == "import_url"
    assert job["project_id"] == "project_1"
    assert job["case_id"] is None


def test_project_pending_decisions_route_lists_project_scope_decisions(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_project(client).status_code == 201

    created = client.post(
        "/api/projects/project_1/materials/import-url",
        headers=AUTH,
        json={"url": "https://example.test/clip.mp4", "filename": "clip.mp4"},
    )
    listed = client.get("/api/projects/project_1/decisions/pending", headers=AUTH)
    missing = client.get("/api/projects/missing/decisions/pending", headers=AUTH)

    assert created.status_code == 200
    assert listed.status_code == 200
    decisions = listed.json()["decisions"]
    assert [decision["decision_id"] for decision in decisions] == [created.json()["decision_id"]]
    assert decisions[0]["scope_type"] == "project"
    assert decisions[0]["case_id"] is None
    assert missing.status_code == 404


def test_retry_annotation_route_requeues_failed_asset_and_resets_status(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_failed_annotation_asset(_engine(app))

    retried = client.post(
        "/api/projects/project_1/materials/asset_1/retry-annotation",
        headers=AUTH,
        json={},
    )
    missing = client.post(
        "/api/projects/project_1/materials/missing/retry-annotation",
        headers=AUTH,
        json={},
    )

    assert retried.status_code == 200
    assert retried.json()["job_id"] is not None
    assert missing.status_code == 404
    with _engine(app).connect() as connection:
        asset = (
            connection.execute(select(schema.assets).where(schema.assets.c.asset_id == "asset_1"))
            .one()
            ._mapping
        )
        job = connection.execute(select(schema.jobs)).one()._mapping
    assert asset["annotation_status"] == "pending"
    assert asset["failure"] is None
    assert job["kind"] == "annotation"
    assert "JobEnqueued" in _event_types(app)


def test_cost_routes_and_project_page_aggregate_provider_calls(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_project_case(_engine(app))
    _apply_events(
        _engine(app),
        CaseCreated(
            project_id="project_1",
            case_id="case_2",
            payload={"name": "Case 2", "brief": {"goal": "second"}},
        ),
        ProjectCreated(project_id="project_2", name="Other"),
        CaseCreated(
            project_id="project_2",
            case_id="case_other",
            payload={"name": "Other Case", "brief": {"goal": "other"}},
        ),
    )
    _insert_provider_cost_rows(_engine(app))

    case_costs = client.get("/api/projects/project_1/cases/case_1/costs", headers=AUTH)
    project_page = client.get("/api/projects/project_1", headers=AUTH)
    missing_case = client.get("/api/projects/project_1/cases/missing/costs", headers=AUTH)

    assert case_costs.status_code == 200
    assert case_costs.json()["costs"]["provider_call_count"] == 1
    assert case_costs.json()["costs"]["total_cost_estimate"] == 0.25
    assert project_page.status_code == 200
    project_costs = project_page.json()["costs"]
    assert project_costs["provider_call_count"] == 3
    assert project_costs["total_cost_estimate"] == 2.0
    assert project_costs["by_capability"] == {"llm.chat": 0.25, "vlm.annotation": 1.75}
    assert missing_case.status_code == 404


async def test_import_url_job_downloads_only_that_url_and_enqueues_proxy(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _apply_events(engine, ProjectCreated(project_id="project_1", name="Project"))
    seen_paths: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_paths.append(request.url.path)
        return httpx.Response(
            200,
            headers={"content-type": "video/mp4", "content-length": "5"},
            content=b"video",
        )

    _insert_import_url_job(engine)
    runner = JobRunner(
        engine=engine,
        registry=build_default_job_registry(
            engine=engine,
            workspace_paths=_state(app).workspace_paths,
            http_transport=httpx.MockTransport(handler),
        ),
    )

    result = await runner.run_once()

    assert result.status == "succeeded"
    assert seen_paths == ["/clip.mp4"]
    with engine.connect() as connection:
        asset = connection.execute(select(schema.assets)).one()._mapping
        proxy_jobs = connection.execute(
            select(func.count()).select_from(schema.jobs).where(schema.jobs.c.kind == "proxy")
        ).scalar_one()
    assert asset["storage_mode"] == "copy"
    assert asset["source"] == "url"
    assert proxy_jobs == 1


async def test_import_url_job_rejects_html_content_type(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _apply_events(engine, ProjectCreated(project_id="project_1", name="Project"))

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/html"}, text="<html></html>")

    _insert_import_url_job(engine)
    runner = JobRunner(
        engine=engine,
        registry=build_default_job_registry(
            engine=engine,
            workspace_paths=_state(app).workspace_paths,
            http_transport=httpx.MockTransport(handler),
        ),
    )

    result = await runner.run_once()

    assert result.status == "failed"
    with engine.connect() as connection:
        asset_count = connection.execute(
            select(func.count()).select_from(schema.assets)
        ).scalar_one()
    assert asset_count == 0


def test_select_and_disable_only_mutate_case_scope(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    source = _media_file(tmp_path, b"raw")
    assert _create_project(client).status_code == 201
    assert _create_case(client).status_code == 201
    imported = client.post(
        "/api/projects/project_1/materials/import-local",
        headers=AUTH,
        json={"path": str(source)},
    )
    asset_id = imported.json()["asset_id"]

    selected = client.post(
        "/api/projects/project_1/cases/case_1/assets/select",
        headers=AUTH,
        json={"asset_id": asset_id},
    )
    disabled = client.post(
        "/api/projects/project_1/cases/case_1/assets/disable",
        headers=AUTH,
        json={"asset_id": asset_id},
    )

    assert selected.status_code == 200
    assert disabled.status_code == 200
    case = disabled.json()["case"]
    assert case["selected_asset_ids"] == []
    assert case["disabled_asset_ids"] == [asset_id]
    with _engine(app).connect() as connection:
        link_count = connection.execute(
            select(func.count()).select_from(schema.project_asset_links)
        ).scalar_one()
    assert link_count == 1


def test_upload_parts_complete_merges_and_records_hash(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_project(client).status_code == 201
    init = client.post(
        "/api/uploads/init",
        headers=AUTH,
        json={"project_id": "project_1", "filename": "upload.mp4"},
    )
    upload_id = init.json()["upload_id"]
    part_headers = {**AUTH, "Content-Type": "application/octet-stream"}
    assert (
        client.put(
            f"/api/uploads/{upload_id}/parts/1", headers=part_headers, content=b"hello"
        ).status_code
        == 200
    )
    assert (
        client.put(
            f"/api/uploads/{upload_id}/parts/2", headers=part_headers, content=b" world"
        ).status_code
        == 200
    )

    complete = client.post(f"/api/uploads/{upload_id}/complete", headers=AUTH, json={})

    assert complete.status_code == 200
    expected_hash = hashlib.sha256(b"hello world").hexdigest()
    with _engine(app).connect() as connection:
        asset = connection.execute(select(schema.assets)).one()._mapping
        object_row = connection.execute(
            select(schema.objects).where(schema.objects.c.hash == expected_hash)
        ).one_or_none()
    assert asset["hash"] == expected_hash
    assert asset["object_hash"] == expected_hash
    assert object_row is not None


def test_resolve_asset_path_supports_reference_and_copy(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = _engine_for_paths(paths)
    reference = tmp_path / "source.mp4"
    reference.write_bytes(b"reference")
    copy_ref = ObjectStore(paths).put_bytes(b"copy")
    _apply_events(
        engine,
        AssetImported(
            project_id="project_1",
            asset_id="asset_ref",
            payload={
                "storage_mode": "reference",
                "reference_path": str(reference),
                "filename": "source.mp4",
                "hash": hashlib.sha256(b"reference").hexdigest(),
                "size": reference.stat().st_size,
                "mtime": reference.stat().st_mtime_ns,
            },
        ),
        AssetImported(
            project_id="project_1",
            asset_id="asset_copy",
            payload={
                "storage_mode": "copy",
                "object_hash": copy_ref.object_hash,
                "object_size": copy_ref.size,
                "filename": "copy.mp4",
                "hash": copy_ref.object_hash,
                "size": copy_ref.size,
                "mtime": 1,
            },
        ),
    )

    with engine.connect() as connection:
        assert resolve_asset_path("asset_ref", connection=connection, paths=paths) == reference
        assert (
            resolve_asset_path("asset_copy", connection=connection, paths=paths).read_bytes()
            == b"copy"
        )


def test_media_proxy_supports_http_range_206(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    proxy_ref = ObjectStore(_state(app).workspace_paths).put_bytes(b"0123456789")
    _apply_events(
        _engine(app),
        ProjectCreated(project_id="project_1", name="Project"),
        AssetImported(
            project_id="project_1",
            asset_id="asset_proxy",
            payload={
                "storage_mode": "copy",
                "object_hash": proxy_ref.object_hash,
                "object_size": proxy_ref.size,
                "filename": "proxy.mp4",
                "hash": proxy_ref.object_hash,
                "size": proxy_ref.size,
                "mtime": 1,
                "proxy_object_hash": proxy_ref.object_hash,
                "proxy_object_size": proxy_ref.size,
            },
        ),
    )

    response = client.get(
        "/api/media/asset_proxy/proxy",
        headers={**AUTH, "Range": "bytes=2-5"},
    )

    assert response.status_code == 206
    assert response.content == b"2345"
    assert response.headers["content-range"] == "bytes 2-5/10"
    assert response.headers["accept-ranges"] == "bytes"


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
@pytest.mark.ffmpeg
async def test_proxy_job_probes_and_generates_proxy_with_ffmpeg(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    video = tmp_path / "allowed" / "fixture.mp4"
    video.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "testsrc=duration=1:size=128x128:rate=30",
            "-pix_fmt",
            "yuv420p",
            str(video),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    assert _create_project(client).status_code == 201
    assert (
        client.post(
            "/api/projects/project_1/materials/import-local",
            headers=AUTH,
            json={"path": str(video)},
        ).status_code
        == 200
    )
    runner = JobRunner(engine=_engine(app))

    result = await runner.run_once()

    assert result.status == "succeeded"
    with _engine(app).connect() as connection:
        asset = connection.execute(select(schema.assets)).one()._mapping
    assert load_json(asset["probe"])["duration_sec"] > 0
    assert asset["proxy_object_hash"] is not None
    assert _state(app).workspace_paths.object_path(asset["proxy_object_hash"]).exists()
    assert {"AssetProbed", "ProxyGenerated"} <= set(_event_types(app))


def _app(tmp_path: Path) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path / "allowed"],
        startup_port=8000,
    )


def _client(app: FastAPI) -> TestClient:
    return TestClient(app, base_url=BASE_URL)


def _state(app: FastAPI) -> Any:
    return app.state.api_state


def _engine(app: FastAPI) -> Engine:
    return _state(app).engine


def _engine_for_paths(paths: WorkspacePaths) -> Engine:
    from storage.db import create_workspace_engine

    engine = create_workspace_engine(paths)
    with engine.begin() as connection:
        schema.create_all(connection)
    _apply_events(engine, ProjectCreated(project_id="project_1", name="Project"))
    return engine


def _media_file(tmp_path: Path, data: bytes) -> Path:
    path = tmp_path / "allowed" / "raw.mp4"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return path


def _create_project(client: TestClient):
    return client.post(
        "/api/projects",
        headers=AUTH,
        json={"project_id": "project_1", "name": "Project"},
    )


def _create_case(client: TestClient):
    return client.post(
        "/api/projects/project_1/cases",
        headers=AUTH,
        json={"case_id": "case_1", "name": "Case", "brief": {"goal": "test"}},
    )


def _seed_project_case(engine: Engine) -> None:
    _apply_events(
        engine,
        ProjectCreated(project_id="project_1", name="Project"),
        CaseCreated(
            project_id="project_1",
            case_id="case_1",
            payload={"name": "Case", "brief": {"goal": "test"}},
        ),
    )


def _seed_failed_annotation_asset(engine: Engine) -> None:
    _apply_events(
        engine,
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
    )


def _insert_provider_cost_rows(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.jobs.insert().values(
                job_id="job_project",
                kind="annotation",
                status="succeeded",
                project_id="project_1",
                case_id=None,
                requested_by_case_id=None,
                asset_id=None,
                idempotency_key="job_project",
                payload_json=dump_json({}),
                result_json=None,
                error_json=None,
                attempts=0,
                max_retries=0,
                next_run_at="2026-07-04T00:00:00+00:00",
                progress=None,
                worker_id=None,
                heartbeat_at=None,
                created_at="2026-07-04T00:00:00+00:00",
                started_at=None,
                finished_at="2026-07-04T00:00:01+00:00",
            )
        )
        rows = [
            {
                "call_id": "call_case_1",
                "provider_id": "fast",
                "capability": "llm.chat",
                "model": "planner",
                "case_id": "case_1",
                "job_id": None,
                "latency_ms": 10,
                "usage_json": dump_json({}),
                "cost_estimate": 0.25,
                "status": "succeeded",
            },
            {
                "call_id": "call_case_2",
                "provider_id": "slow",
                "capability": "vlm.annotation",
                "model": "vlm",
                "case_id": "case_2",
                "job_id": None,
                "latency_ms": 20,
                "usage_json": dump_json({}),
                "cost_estimate": 0.5,
                "status": "succeeded",
            },
            {
                "call_id": "call_project_job",
                "provider_id": "slow",
                "capability": "vlm.annotation",
                "model": "vlm",
                "case_id": None,
                "job_id": "job_project",
                "latency_ms": 30,
                "usage_json": dump_json({}),
                "cost_estimate": 1.25,
                "status": "succeeded",
            },
            {
                "call_id": "call_other",
                "provider_id": "other",
                "capability": "llm.chat",
                "model": "planner",
                "case_id": "case_other",
                "job_id": None,
                "latency_ms": 40,
                "usage_json": dump_json({}),
                "cost_estimate": 9.0,
                "status": "succeeded",
            },
        ]
        connection.execute(schema.provider_calls.insert(), rows)


def _apply_events(engine: Engine, *events: Any) -> None:
    result = apply(events, engine=engine, base_version=None, actor="user")
    assert result.status == "applied"


def _event_types(app: FastAPI) -> list[str]:
    with _engine(app).connect() as connection:
        rows = EventLogRepository(connection).read_after(0, limit=500)
    return [row.event_type for row in rows]


def _insert_import_url_job(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        JobsRepository(connection).insert(
            {
                "job_id": "job_import_url",
                "kind": "import_url",
                "status": "pending",
                "project_id": "project_1",
                "case_id": None,
                "requested_by_case_id": None,
                "asset_id": None,
                "idempotency_key": "url:clip",
                "payload_json": {
                    "asset_id": "asset_url",
                    "project_id": "project_1",
                    "url": "https://example.test/clip.mp4",
                    "filename": "clip.mp4",
                    "kind": "video",
                },
                "result_json": None,
                "error_json": None,
                "attempts": 0,
                "max_retries": 0,
                "next_run_at": "2026-07-04T00:00:00+00:00",
                "progress": None,
                "worker_id": None,
                "heartbeat_at": None,
                "created_at": "2026-07-04T00:00:00+00:00",
                "started_at": None,
                "finished_at": None,
            }
        )
