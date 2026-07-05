from __future__ import annotations

from pathlib import Path
from typing import Any

from apps.api.main import create_app
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import CaseCreated, ExportCompleted, PreviewRendered, ProjectCreated
from storage.object_store import ObjectStore

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_preview_media_route_supports_range_206(tmp_path: Path) -> None:
    app = _app(tmp_path)
    object_ref = ObjectStore(_state(app).workspace_paths).put_bytes(b"0123456789")
    _seed_project_case(_engine(app))
    _apply_events(
        _engine(app),
        PreviewRendered(
            project_id="project_1",
            case_id="case_1",
            timeline_version=1,
            artifact_id="preview_1",
            payload={"object_hash": object_ref.object_hash},
        ),
    )

    response = _client(app).get(
        "/api/media/preview/preview_1",
        headers={**AUTH, "Range": "bytes=3-6"},
    )

    assert response.status_code == 206
    assert response.content == b"3456"
    assert response.headers["content-range"] == "bytes 3-6/10"


def test_export_media_route_supports_range_206(tmp_path: Path) -> None:
    app = _app(tmp_path)
    object_ref = ObjectStore(_state(app).workspace_paths).put_bytes(b"abcdefghij")
    _seed_project_case(_engine(app))
    _apply_events(
        _engine(app),
        ExportCompleted(
            project_id="project_1",
            case_id="case_1",
            timeline_version=1,
            artifact_id="export_1",
            payload={"object_hash": object_ref.object_hash},
        ),
    )

    response = _client(app).get(
        "/api/media/export/export_1",
        headers={**AUTH, "Range": "bytes=1-4"},
    )

    assert response.status_code == 206
    assert response.content == b"bcde"
    assert response.headers["accept-ranges"] == "bytes"


def _app(tmp_path: Path) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path],
        startup_port=8000,
    )


def _client(app: FastAPI) -> TestClient:
    return TestClient(app, base_url=BASE_URL)


def _state(app: FastAPI) -> Any:
    return app.state.api_state


def _engine(app: FastAPI) -> Engine:
    return _state(app).engine


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


def _apply_events(engine: Engine, *events: Any) -> None:
    result = apply(events, engine=engine, base_version=None, actor="user")
    assert result.status == "applied"
