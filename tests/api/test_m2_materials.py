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
    AssetImported,
    AssetIndexReady,
    AssetLinked,
    DraftCreated,
    MaterialUnderstandingCompleted,
)
from storage import schema
from storage.db import begin_immediate
from storage.object_store import ObjectStore
from storage.repositories import (
    EventLogRepository,
    JobsRepository,
    MaterialSummariesRepository,
)
from storage.repositories._json import dump_json, load_json
from storage.workspace_paths import WorkspacePaths, resolve_asset_path

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_reference_import_lists_material_without_object_copy(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    source = _media_file(tmp_path, b"raw-media")
    assert _create_draft(client).status_code == 201

    imported = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"path": str(source)},
    )
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)

    assert imported.status_code == 200
    assert materials.status_code == 200
    asset = materials.json()["assets"][0]
    assert asset["storage_mode"] == "reference"
    assert asset["filename"] == "raw.mp4"
    assert asset["proxy_ready"] is False
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
    assert _create_draft(client).status_code == 201
    assert (
        client.post(
            "/api/drafts/draft_1/materials/import-local",
            headers=AUTH,
            json={"path": str(source)},
        ).status_code
        == 200
    )
    source.write_bytes(b"after-change")

    response = client.get("/api/drafts/draft_1/materials", headers=AUTH)

    assert response.status_code == 200
    asset = response.json()["assets"][0]
    assert asset["usable"] is False
    assert asset["invalid"] is True
    assert response.json()["invalidated_asset_ids"] == [asset["asset_id"]]
    assert "AssetInvalidated" in _event_types(app)


def test_import_url_route_creates_draft_decision_and_answer_enqueues_job(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201

    created = client.post(
        "/api/drafts/draft_1/materials/import-url",
        headers=AUTH,
        json={"url": "https://example.test/clip.mp4", "filename": "clip.mp4"},
    )
    decision_id = created.json()["decision_id"]
    answered = client.post(
        f"/api/decisions/{decision_id}/answer",
        headers=AUTH,
        json={
            "draft_id": "draft_1",
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
    assert job["draft_id"] == "draft_1"


def test_draft_pending_decisions_route_lists_draft_scope_decisions(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201

    created = client.post(
        "/api/drafts/draft_1/materials/import-url",
        headers=AUTH,
        json={"url": "https://example.test/clip.mp4", "filename": "clip.mp4"},
    )
    listed = client.get("/api/drafts/draft_1/decisions/pending", headers=AUTH)
    missing = client.get("/api/drafts/missing/decisions/pending", headers=AUTH)

    assert created.status_code == 200
    assert listed.status_code == 200
    decisions = listed.json()["decisions"]
    assert [decision["decision_id"] for decision in decisions] == [created.json()["decision_id"]]
    assert decisions[0]["scope_type"] == "draft"
    assert decisions[0]["draft_id"] == "draft_1"
    assert missing.status_code == 404


def test_cost_route_aggregates_provider_calls_for_draft(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _apply_events(_engine(app), DraftCreated(draft_id="draft_2", payload={"name": "Other"}))
    _insert_provider_cost_rows(_engine(app))

    draft_costs = client.get("/api/drafts/draft_1/costs", headers=AUTH)
    missing = client.get("/api/drafts/missing/costs", headers=AUTH)

    assert draft_costs.status_code == 200
    costs = draft_costs.json()["costs"]
    assert costs["provider_call_count"] == 2
    assert costs["total_cost_estimate"] == 2.0
    assert costs["by_capability"] == {"llm.chat": 0.25, "vlm.understanding": 1.75}
    assert missing.status_code == 404


async def test_import_url_job_downloads_only_that_url_and_enqueues_proxy(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_draft(engine)
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
    _seed_draft(engine)

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


def test_resolve_asset_path_supports_reference_and_copy(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    engine = _engine_for_paths(paths)
    reference = tmp_path / "source.mp4"
    reference.write_bytes(b"reference")
    copy_ref = ObjectStore(paths).put_bytes(b"copy")
    _apply_events(
        engine,
        AssetImported(
            draft_id="draft_1",
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
            draft_id="draft_1",
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
    _seed_draft(_engine(app))
    _apply_events(
        _engine(app),
        AssetImported(
            draft_id="draft_1",
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
    assert _create_draft(client).status_code == 201
    assert (
        client.post(
            "/api/drafts/draft_1/materials/import-local",
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


@pytest.mark.parametrize(
    ("filename", "expected_kind"),
    [("clip.mov", "video"), ("song.WAV", "audio"), ("pic.PNG", "image"), ("face.otf", "font")],
)
def test_import_local_kind_inferred_from_suffix(
    tmp_path: Path, filename: str, expected_kind: str
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    source = tmp_path / "allowed" / filename
    source.parent.mkdir(parents=True, exist_ok=True)
    source.write_bytes(b"raw-media")

    imported = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"path": str(source)},
    )

    assert imported.status_code == 200
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    asset = materials.json()["assets"][0]
    assert asset["kind"] == expected_kind


@pytest.mark.parametrize("filename", ["sub.srt", "weird.xyz", "noext"])
def test_import_local_unsupported_suffix_rejected(tmp_path: Path, filename: str) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    source = tmp_path / "allowed" / filename
    source.parent.mkdir(parents=True, exist_ok=True)
    source.write_bytes(b"raw-media")

    resp = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"path": str(source)},
    )

    assert resp.status_code == 400
    assert resp.json()["detail"]["error_code"] == "unsupported_material_type"


@pytest.mark.parametrize(
    ("url", "filename", "expected_kind"),
    [
        ("https://example.test/clip.mp4", None, "video"),
        ("https://example.test/track.MP3", None, "audio"),
        ("https://example.test/whatever", "poster.png", "image"),
    ],
)
def test_import_url_kind_inferred_from_suffix(
    tmp_path: Path, url: str, filename: str | None, expected_kind: str
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    body: dict[str, Any] = {"url": url}
    if filename is not None:
        body["filename"] = filename

    created = client.post(
        "/api/drafts/draft_1/materials/import-url",
        headers=AUTH,
        json=body,
    )

    assert created.status_code == 200
    with _engine(app).connect() as connection:
        row = connection.execute(select(schema.decisions)).one()._mapping
    pending = load_json(row["pending_tool_call"])
    assert pending["arguments"]["kind"] == expected_kind


@pytest.mark.parametrize(
    ("url", "filename"),
    [
        ("https://example.com/x.srt", None),
        ("https://example.com/x.xyz", None),
        ("https://example.com/", None),
        ("https://example.com/clip", None),
        ("https://example.com/clip.mp4", "override.srt"),
    ],
)
def test_import_url_unsupported_suffix_rejected(
    tmp_path: Path, url: str, filename: str | None
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    body: dict[str, Any] = {"url": url}
    if filename is not None:
        body["filename"] = filename

    resp = client.post(
        "/api/drafts/draft_1/materials/import-url",
        headers=AUTH,
        json=body,
    )

    assert resp.status_code == 400
    assert resp.json()["detail"]["error_code"] == "unsupported_material_type"
    with _engine(app).connect() as connection:
        decision_count = connection.execute(
            select(func.count()).select_from(schema.decisions)
        ).scalar_one()
    assert decision_count == 0


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
    _seed_draft(engine)
    return engine


def _media_file(tmp_path: Path, data: bytes) -> Path:
    path = tmp_path / "allowed" / "raw.mp4"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)
    return path


def _create_draft(client: TestClient):
    return client.post(
        "/api/drafts",
        headers=AUTH,
        json={"draft_id": "draft_1", "name": "草稿"},
    )


def _seed_draft(engine: Engine, draft_id: str = "draft_1") -> None:
    _apply_events(
        engine,
        DraftCreated(
            draft_id=draft_id,
            payload={"name": "草稿", "brief": {"goal": "test"}},
        ),
    )


def _insert_provider_cost_rows(engine: Engine) -> None:
    with begin_immediate(engine) as connection:
        rows = [
            {
                "call_id": "call_draft1_1",
                "provider_id": "fast",
                "capability": "llm.chat",
                "model": "planner",
                "draft_id": "draft_1",
                "job_id": None,
                "latency_ms": 10,
                "usage_json": dump_json({}),
                "cost_estimate": 0.25,
                "status": "succeeded",
            },
            {
                "call_id": "call_draft1_2",
                "provider_id": "slow",
                "capability": "vlm.understanding",
                "model": "vlm",
                "draft_id": "draft_1",
                "job_id": None,
                "latency_ms": 20,
                "usage_json": dump_json({}),
                "cost_estimate": 1.75,
                "status": "succeeded",
            },
            {
                "call_id": "call_draft2",
                "provider_id": "other",
                "capability": "llm.chat",
                "model": "planner",
                "draft_id": "draft_2",
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
                "draft_id": "draft_1",
                "requested_by_draft_id": None,
                "asset_id": None,
                "idempotency_key": "url:clip",
                "payload_json": {
                    "asset_id": "asset_url",
                    "draft_id": "draft_1",
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


def _seed_indexed_asset(
    app: FastAPI,
    *,
    asset_id: str,
    thumbnail_bytes: bytes | None,
    duration_sec: float,
    draft_id: str = "draft_1",
) -> str | None:
    paths = _state(app).workspace_paths
    object_ref = ObjectStore(paths).put_bytes(b"source-" + asset_id.encode())
    thumbnail_hash: str | None = None
    events: list[Any] = [
        AssetImported(
            draft_id=draft_id,
            asset_id=asset_id,
            payload={
                "storage_mode": "copy",
                "object_hash": object_ref.object_hash,
                "object_size": object_ref.size,
                "kind": "video",
                "filename": f"{asset_id}.mp4",
                "hash": object_ref.object_hash,
                "size": object_ref.size,
                "mtime": 1,
                "probe": {"duration_sec": duration_sec, "has_audio": False},
            },
        ),
        AssetLinked(draft_id=draft_id, asset_id=asset_id),
    ]
    if thumbnail_bytes is not None:
        thumbnail_hash = ObjectStore(paths).put_bytes(thumbnail_bytes).object_hash
        events.append(
            AssetIndexReady(
                draft_id=draft_id,
                asset_id=asset_id,
                payload={
                    "index_json": {"duration_sec": duration_sec, "shots": []},
                    "thumbnail_object_hash": thumbnail_hash,
                    "ingest_status": "indexed",
                },
            )
        )
        events.append(MaterialUnderstandingCompleted(draft_id=draft_id, asset_id=asset_id))
    _apply_events(_engine(app), *events)
    return thumbnail_hash


def test_media_thumbnail_serves_jpeg_and_404_when_missing(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    thumbnail_bytes = b"\xff\xd8\xff\xe0jpeg-body"
    _seed_indexed_asset(
        app,
        asset_id="asset_thumb",
        thumbnail_bytes=thumbnail_bytes,
        duration_sec=12.5,
    )
    _seed_indexed_asset(app, asset_id="asset_bare", thumbnail_bytes=None, duration_sec=3.0)

    ready = client.get("/api/media/asset_thumb/thumbnail", headers=AUTH)
    missing = client.get("/api/media/asset_bare/thumbnail", headers=AUTH)

    assert ready.status_code == 200
    assert ready.headers["content-type"] == "image/jpeg"
    assert ready.content == thumbnail_bytes
    assert missing.status_code == 404
    assert missing.json()["detail"]["reason"] == "thumbnail_not_ready"


def test_media_thumbnail_accepts_query_token_like_browser_img(tmp_path: Path) -> None:
    """浏览器 <img src> 设不了 Authorization header，media 族 GET 必须吃 query token。"""
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    thumbnail_bytes = b"\xff\xd8\xff\xe0jpeg-body"
    _seed_indexed_asset(
        app,
        asset_id="asset_thumb",
        thumbnail_bytes=thumbnail_bytes,
        duration_sec=12.5,
    )
    _seed_indexed_asset(app, asset_id="asset_bare", thumbnail_bytes=None, duration_sec=3.0)

    ready = client.get("/api/media/asset_thumb/thumbnail", params={"token": TOKEN})
    missing = client.get("/api/media/asset_bare/thumbnail", params={"token": TOKEN})
    no_token = client.get("/api/media/asset_thumb/thumbnail")
    bad_token = client.get("/api/media/asset_thumb/thumbnail", params={"token": "wrong"})

    assert ready.status_code == 200
    assert ready.content == thumbnail_bytes
    assert missing.status_code == 404
    assert missing.json()["detail"]["reason"] == "thumbnail_not_ready"
    assert no_token.status_code == 401
    assert no_token.json()["reason"] == "missing_token"
    assert bad_token.status_code == 401
    assert bad_token.json()["reason"] == "bad_token"


def test_media_head_accepts_query_token_like_player_probe(tmp_path: Path) -> None:
    """播放器加载媒体源前先 HEAD 探测 Content-Type：HEAD 必须与 GET 同权吃
    query token（且路由注册 HEAD），否则探测 401/405、预览黑屏。"""
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _seed_indexed_asset(
        app,
        asset_id="asset_thumb",
        thumbnail_bytes=b"\xff\xd8\xff\xe0jpeg-body",
        duration_sec=12.5,
    )

    head_ok = client.head("/api/media/asset_thumb/thumbnail", params={"token": TOKEN})
    head_no_token = client.head("/api/media/asset_thumb/thumbnail")

    assert head_ok.status_code == 200
    assert head_ok.content == b""
    assert head_no_token.status_code == 401


def test_query_token_not_accepted_outside_sse_and_media(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))

    response = client.get("/api/drafts/draft_1/materials", params={"token": TOKEN})

    assert response.status_code == 401
    assert response.json()["reason"] == "missing_token"


def _insert_ready_summary(
    engine: Engine,
    *,
    asset_id: str,
    version: int,
    summary_json: dict[str, Any],
) -> None:
    with begin_immediate(engine) as connection:
        MaterialSummariesRepository(connection).insert(
            {
                "summary_id": f"ms_{asset_id}_v{version}",
                "asset_id": asset_id,
                "version": version,
                "focus": None,
                "status": "ready",
                "summary_json": summary_json,
                "model": "qwen-max",
                "created_at": "2026-07-04T00:00:00+00:00",
            }
        )


def test_material_summary_route_returns_latest_ready_summary(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _seed_indexed_asset(app, asset_id="asset_sum", thumbnail_bytes=None, duration_sec=5.0)
    _insert_ready_summary(
        _engine(app),
        asset_id="asset_sum",
        version=1,
        summary_json={
            "asset_id": "asset_sum",
            "version": 1,
            "semantic_role": "footage",
            "overall": "整体描述",
            "segments": [
                {
                    "start_s": 0.0,
                    "end_s": 2.0,
                    "description": "开场",
                    "tags": ["hook"],
                    "quality": "good",
                }
            ],
            "generated_at": "2026-07-04T00:00:00+00:00",
            "model": "qwen-max",
        },
    )

    response = client.get("/api/drafts/draft_1/materials/asset_sum/summary", headers=AUTH)

    assert response.status_code == 200
    body = response.json()
    assert body["asset_id"] == "asset_sum"
    assert body["summary"]["semantic_role"] == "footage"
    assert body["summary"]["overall"] == "整体描述"
    assert body["summary"]["segments"][0]["description"] == "开场"


def test_material_summary_route_404_when_no_ready_summary(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _seed_indexed_asset(app, asset_id="asset_none", thumbnail_bytes=None, duration_sec=5.0)

    response = client.get("/api/drafts/draft_1/materials/asset_none/summary", headers=AUTH)

    assert response.status_code == 404
    assert response.json()["detail"]["reason"] == "summary_not_ready"


def test_material_summary_route_404_when_asset_not_linked_to_draft(tmp_path: Path) -> None:
    """跨草稿越权：asset 挂在 draft_1，从 draft_2 查摘要必须 asset_not_linked。"""
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _apply_events(_engine(app), DraftCreated(draft_id="draft_2", payload={"name": "Other"}))
    _seed_indexed_asset(app, asset_id="asset_sum", thumbnail_bytes=None, duration_sec=5.0)

    response = client.get("/api/drafts/draft_2/materials/asset_sum/summary", headers=AUTH)

    assert response.status_code == 404
    assert response.json()["detail"]["reason"] == "asset_not_linked"


def test_materials_payload_exposes_thumbnail_duration_and_understanding(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _seed_indexed_asset(
        app,
        asset_id="asset_thumb",
        thumbnail_bytes=b"\xff\xd8\xff\xe0jpeg",
        duration_sec=8.0,
    )
    _seed_indexed_asset(app, asset_id="asset_bare", thumbnail_bytes=None, duration_sec=3.0)

    response = client.get("/api/drafts/draft_1/materials", headers=AUTH)

    assert response.status_code == 200
    assets = {asset["asset_id"]: asset for asset in response.json()["assets"]}
    indexed = assets["asset_thumb"]
    assert indexed["thumbnail_ready"] is True
    assert indexed["duration_sec"] == 8.0
    assert indexed["understanding_status"] == "ready"
    bare = assets["asset_bare"]
    assert bare["thumbnail_ready"] is False
    assert bare["duration_sec"] == 3.0
    assert bare["understanding_status"] == "none"


def test_delete_material_unlinks_from_draft(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    _seed_draft(_engine(app))
    _seed_indexed_asset(app, asset_id="asset_del", thumbnail_bytes=None, duration_sec=4.0)

    deleted = client.request(
        "DELETE", "/api/drafts/draft_1/materials/asset_del", headers=AUTH, json={}
    )

    assert deleted.status_code == 200
    assert "AssetUnlinked" in _event_types(app)
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    assert materials.json()["assets"] == []
    # 断链不删物理 asset：全局 assets 表仍保留该行。
    with _engine(app).connect() as connection:
        asset_count = connection.execute(
            select(func.count()).select_from(schema.assets)
        ).scalar_one()
    assert asset_count == 1


def test_import_local_directory_recurses_with_rel_dir_and_skips_unsupported(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    root = tmp_path / "allowed" / "素材A"
    (root / "视频").mkdir(parents=True)
    (root / "音频" / "环境声").mkdir(parents=True)
    (root / "视频" / "a.mp4").write_bytes(b"video-a")
    (root / "音频" / "环境声" / "b.mp3").write_bytes(b"audio-b")
    (root / "封面.png").write_bytes(b"cover")
    (root / "notes.txt").write_bytes(b"skip-me")
    (root / ".hidden.mp4").write_bytes(b"hidden")

    imported = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(root)]},
    )

    assert imported.status_code == 200
    body = imported.json()
    assert len(body["asset_ids"]) == 3
    assert body["skipped"] == ["notes.txt"]

    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    rel_dirs = {asset["filename"]: asset["rel_dir"] for asset in materials.json()["assets"]}
    assert rel_dirs["a.mp4"] == "素材A/视频"
    assert rel_dirs["b.mp3"] == "素材A/音频/环境声"
    assert rel_dirs["封面.png"] == "素材A"
    assert ".hidden.mp4" not in rel_dirs


def test_import_local_batch_paths_mixes_files_and_directories(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    single = _media_file(tmp_path, b"single-file")
    folder = tmp_path / "allowed" / "b-roll"
    folder.mkdir(parents=True)
    (folder / "clip.mov").write_bytes(b"clip")

    imported = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(single), str(folder)]},
    )

    assert imported.status_code == 200
    body = imported.json()
    assert len(body["asset_ids"]) == 2
    assert body["asset_id"] == body["asset_ids"][0]

    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    rel_dirs = {asset["filename"]: asset["rel_dir"] for asset in materials.json()["assets"]}
    assert rel_dirs["raw.mp4"] is None
    assert rel_dirs["clip.mov"] == "b-roll"


def test_import_local_requires_at_least_one_path(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201

    response = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={},
    )

    assert response.status_code == 400
    assert response.json()["detail"]["error_code"] == "missing_path"


def test_import_local_directory_skips_symlink_escaping_fs_roots(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    outside = tmp_path / "outside" / "secret.mp4"
    outside.parent.mkdir(parents=True)
    outside.write_bytes(b"secret-bytes")
    root = tmp_path / "allowed" / "linked"
    root.mkdir(parents=True)
    (root / "ok.mp4").write_bytes(b"ok")
    (root / "escape.mp4").symlink_to(outside)

    imported = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(root)]},
    )

    assert imported.status_code == 200
    body = imported.json()
    assert len(body["asset_ids"]) == 1
    assert any("escape.mp4" in item for item in body["skipped"])
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    filenames = {asset["filename"] for asset in materials.json()["assets"]}
    assert filenames == {"ok.mp4"}


def test_import_local_rejects_asset_id_with_directory_batch(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    root = tmp_path / "allowed" / "batch"
    root.mkdir(parents=True)
    (root / "a.mp4").write_bytes(b"a")
    (root / "b.mp4").write_bytes(b"b")

    response = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(root)], "asset_id": "asset_explicit"},
    )

    assert response.status_code == 400
    assert response.json()["detail"]["error_code"] == "asset_id_requires_single_file"
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    assert materials.json()["assets"] == []


def test_import_local_reimport_same_directory_dedupes_by_reference_path(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    root = tmp_path / "allowed" / "footage"
    root.mkdir(parents=True)
    (root / "a.mp4").write_bytes(b"a")

    first = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(root)]},
    )
    (root / "b.mp4").write_bytes(b"b")
    second = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(root)]},
    )

    assert first.status_code == 200
    assert second.status_code == 200
    assert second.json()["duplicates"] == ["a.mp4"]
    assert len(second.json()["asset_ids"]) == 1
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    assert len(materials.json()["assets"]) == 2


def test_import_local_missing_file_lands_in_failed_without_aborting_batch(
    tmp_path: Path,
) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert _create_draft(client).status_code == 201
    good = _media_file(tmp_path, b"good")
    missing = tmp_path / "allowed" / "gone.mp4"

    response = client.post(
        "/api/drafts/draft_1/materials/import-local",
        headers=AUTH,
        json={"paths": [str(missing), str(good)]},
    )

    assert response.status_code == 200
    body = response.json()
    assert len(body["asset_ids"]) == 1
    assert any("gone.mp4" in item for item in body["failed"])
    materials = client.get("/api/drafts/draft_1/materials", headers=AUTH)
    assert {asset["filename"] for asset in materials.json()["assets"]} == {"raw.mp4"}


def test_fs_pick_returns_native_paths(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    import apps.api.main as api_main

    app = _app(tmp_path)
    client = _client(app)
    monkeypatch.setattr(api_main, "_native_picker_available", lambda: True)
    monkeypatch.setattr(api_main, "_run_native_picker", lambda mode: [f"/tmp/{mode}/clip.mp4"])

    response = client.post("/api/fs/pick", headers=AUTH, json={"mode": "files"})

    assert response.status_code == 200
    assert response.json() == {"available": True, "paths": ["/tmp/files/clip.mp4"]}


def test_fs_pick_cancel_and_unavailable(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    import apps.api.main as api_main

    app = _app(tmp_path)
    client = _client(app)

    monkeypatch.setattr(api_main, "_native_picker_available", lambda: True)
    monkeypatch.setattr(api_main, "_run_native_picker", lambda mode: [])
    cancelled = client.post("/api/fs/pick", headers=AUTH, json={"mode": "folder"})
    assert cancelled.json() == {"available": True, "paths": []}

    monkeypatch.setattr(api_main, "_native_picker_available", lambda: False)
    unavailable = client.post("/api/fs/pick", headers=AUTH, json={"mode": "files"})
    assert unavailable.json() == {"available": False, "paths": []}
