from __future__ import annotations

from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any

import httpx
from apps.api.main import create_app
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy import func, select
from sqlalchemy.engine import Engine

from agent_harness.loop import ScriptedPlanner
from agent_harness.reducer import apply
from agent_harness.turn_queue import StopToken, TurnQueueItem
from contracts.events import (
    AssetImported,
    AssetLinked,
    CaseAssetScopeChanged,
    CaseCreated,
    ProjectCreated,
)
from storage import schema
from storage.db import begin_immediate
from storage.repositories import CasesRepository, EventLogRepository, ProjectsRepository
from storage.repositories._json import dump_json, load_json
from tools import build_default_tool_registry

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_project_tree_shows_only_project_and_case_nodes(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_project_case(engine)
    _apply_events(
        engine,
        AssetImported(asset_id="asset_1", job_id="job_import"),
        AssetLinked(project_id="project_1", asset_id="asset_1"),
    )
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.memories.insert().values(
                memory_id="memory_1",
                scope="project",
                project_id="project_1",
                content="style",
                tags=dump_json([]),
                created_from_case_id=None,
                created_at="2026-07-04T00:00:00+00:00",
            )
        )

    response = _client(app).get("/api/project-tree", headers=AUTH)

    assert response.status_code == 200
    assert response.json()["projects"] == [
        {
            "project_id": "project_1",
            "name": "Project",
            "status": "active",
            "cases": [
                {
                    "case_id": "case_1",
                    "project_id": "project_1",
                    "name": "Case",
                    "status": "active",
                }
            ],
        }
    ]
    assert "asset_1" not in response.text
    assert "memory_1" not in response.text


def test_project_page_route_data_contains_homepage_fields(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_project_case(engine)

    response = _client(app).get("/api/projects/project_1", headers=AUTH)

    assert response.status_code == 200
    payload = response.json()
    assert payload["project"]["project_id"] == "project_1"
    assert payload["case_count"] == 1
    assert payload["asset_count"] == 0
    assert payload["actions"]["create_case"] == "/api/projects/project_1/cases"
    assert payload["actions"]["materials"] == "/projects/project_1/materials"
    assert "messages" not in payload


def test_create_case_from_project_records_brief_goal_for_console(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)
    assert (
        client.post(
            "/api/projects",
            headers=AUTH,
            json={"project_id": "project_1", "name": "Project"},
        ).status_code
        == 201
    )

    created = client.post(
        "/api/projects/project_1/cases",
        headers=AUTH,
        json={"case_id": "case_1", "name": "Case", "goal": "剪一条 30 秒种草视频"},
    )
    fetched = client.get("/api/projects/project_1/cases/case_1", headers=AUTH)

    assert created.status_code == 201
    assert created.json()["case"]["brief"]["goal"] == "剪一条 30 秒种草视频"
    assert fetched.status_code == 200
    assert fetched.json()["case"]["brief"]["goal"] == "剪一条 30 秒种草视频"


async def test_project_delete_uses_project_scoped_ask_and_replay(tmp_path: Path) -> None:
    planner = ScriptedPlanner(
        [
            {
                "tool_name": "project.delete",
                "arguments": {"project_id": "project_1"},
                "tool_call_id": "tc_delete",
            }
        ]
    )
    app = _app(tmp_path, planner=planner)

    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app),
        base_url=BASE_URL,
        headers=AUTH,
    ) as client:
        project = await client.post(
            "/api/projects",
            json={"project_id": "project_1", "name": "Project"},
        )
        assert project.status_code == 201
        case = await client.post(
            "/api/projects/project_1/cases",
            json={"case_id": "case_1", "name": "Case", "brief": {"goal": "test"}},
        )
        assert case.status_code == 201
        message = await client.post(
            "/api/projects/project_1/cases/case_1/messages",
            json={"message_id": "msg_1", "content": "删除这个项目"},
        )
        assert message.status_code == 202
        await _state(app).turn_queue.join_case("case_1")
        decision = _pending_project_decision(app)
        assert decision["type"] == "destructive_project_action"
        assert decision["scope_type"] == "project"
        assert decision["pending_tool_call"]["tool_name"] == "project.delete"

        answered = await client.post(
            f"/api/decisions/{decision['decision_id']}/answer",
            json={
                "project_id": "project_1",
                "answer": {
                    "option_id": "approve",
                    "answered_via": "button",
                    "payload": {"approved": True},
                },
            },
        )
        assert answered.status_code == 200
        assert answered.json()["replays_enqueued"] == 1
        await _state(app).turn_queue.join_case("case_1")

    with _engine(app).connect() as connection:
        project_row = ProjectsRepository(connection).get("project_1")
    assert project_row is not None
    assert project_row["status"] == "trashed"
    assert "ProjectTrashed" in _event_types(app)


def test_move_case_copies_referenced_asset_links_to_target_project(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_project_case(engine)
    _apply_events(engine, ProjectCreated(project_id="project_2", name="Project 2"))
    _apply_events(
        engine,
        AssetImported(asset_id="asset_1", job_id="job_import"),
        AssetLinked(project_id="project_1", asset_id="asset_1"),
    )
    scope_result = apply(
        (
            CaseAssetScopeChanged(
                case_id="case_1",
                payload={"selected_asset_ids": ["asset_1"]},
            ),
        ),
        engine=engine,
        base_version=0,
        actor="user",
    )
    assert scope_result.status == "applied"

    response = _client(app).post(
        "/api/projects/project_1/cases/case_1/move",
        headers=AUTH,
        json={"target_project_id": "project_2", "confirm": True},
    )

    assert response.status_code == 200
    assert response.json()["case"]["project_id"] == "project_2"
    with engine.connect() as connection:
        link_count = connection.execute(
            select(func.count())
            .select_from(schema.project_asset_links)
            .where(schema.project_asset_links.c.project_id == "project_2")
            .where(schema.project_asset_links.c.asset_id == "asset_1")
        ).scalar_one()
    assert link_count == 1
    assert _event_types(app).count("AssetLinked") == 2


def test_case_rename_delete_copy_are_rest_only_and_reduce_correctly(tmp_path: Path) -> None:
    registry = build_default_tool_registry()
    assert registry.get("case.rename") is None
    assert registry.get("case.delete") is None
    assert registry.get("case.copy") is None

    app = _app(tmp_path)
    engine = _engine(app)
    _seed_project_case(engine)
    client = _client(app)

    renamed = client.patch(
        "/api/projects/project_1/cases/case_1",
        headers=AUTH,
        json={"name": "Renamed Case"},
    )
    copied = client.post(
        "/api/projects/project_1/cases/case_1/copy",
        headers=AUTH,
        json={"case_id": "case_2", "name": "Copied Case"},
    )
    trashed = client.request(
        "DELETE",
        "/api/projects/project_1/cases/case_1",
        headers=AUTH,
        json={"confirm": True},
    )

    assert renamed.status_code == 200
    assert copied.status_code == 201
    assert trashed.status_code == 200
    assert {"CaseRenamed", "CaseCopied", "CaseTrashed"} <= set(_event_types(app))
    with engine.connect() as connection:
        source = CasesRepository(connection).get("case_1")
        copied_case = CasesRepository(connection).get("case_2")
    assert source is not None
    assert source["name"] == "Renamed Case"
    assert source["status"] == "trashed"
    assert copied_case is not None
    assert copied_case["name"] == "Copied Case"
    assert copied_case["state_version"] == 0


def _app(
    tmp_path: Path,
    *,
    planner: ScriptedPlanner | None = None,
    turn_runner: Callable[[TurnQueueItem, StopToken], Awaitable[None]] | None = None,
) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path / "allowed"],
        planner=planner,
        turn_runner=turn_runner,
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


def _apply_events(engine: Engine, *events: Any) -> list[int]:
    result = apply(events, engine=engine, base_version=None, actor="user")
    assert result.status == "applied"
    return [event.event_id for event in result.applied_events]


def _event_types(app: FastAPI) -> list[str]:
    with _engine(app).connect() as connection:
        rows = EventLogRepository(connection).read_after(0, limit=500)
    return [row.event_type for row in rows]


def _pending_project_decision(app: FastAPI) -> dict[str, Any]:
    with _engine(app).connect() as connection:
        row = connection.execute(
            select(schema.decisions).where(
                schema.decisions.c.type == "destructive_project_action",
                schema.decisions.c.scope_type == "project",
            )
        ).one()
    values = dict(row._mapping)
    values["options"] = load_json(values["options"])
    if values["pending_tool_call"] is not None:
        values["pending_tool_call"] = load_json(values["pending_tool_call"])
    return values
