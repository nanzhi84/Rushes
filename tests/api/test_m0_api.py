from __future__ import annotations

import json
import threading
import time
from collections.abc import Awaitable, Callable, Sequence
from pathlib import Path
from typing import Any

import httpx
from apps.api.main import create_app
from fastapi import FastAPI
from fastapi.testclient import TestClient
from sqlalchemy.engine import Engine

from agent_harness.loop import ScriptedPlanner
from agent_harness.reducer import apply
from agent_harness.turn_queue import StopToken, TurnQueueItem
from contracts.events import CaseCreated, DecisionCreated, JobEnqueued, JobProgress, ProjectCreated
from storage.repositories import (
    CasesRepository,
    EventLogRepository,
    JobsRepository,
    MessagesRepository,
)

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def test_security_baseline_refusals_emit_security_refusal_events(tmp_path: Path) -> None:
    app = _app(tmp_path)
    client = _client(app)

    checks = [
        (
            client.post("/api/projects", json={"name": "Missing"}),
            401,
            "missing_token",
        ),
        (
            client.post(
                "/api/projects",
                headers={"Authorization": "Bearer wrong"},
                json={"name": "Wrong"},
            ),
            401,
            "bad_token",
        ),
        (
            client.post(
                "/api/projects",
                headers={**AUTH, "Origin": "http://evil.example"},
                json={"name": "Origin"},
            ),
            403,
            "origin_mismatch",
        ),
        (
            client.post(
                "/api/projects",
                headers={**AUTH, "Content-Type": "text/plain"},
                content="{}",
            ),
            415,
            "bad_content_type",
        ),
        (
            client.post(
                "/api/projects",
                headers={**AUTH, "Host": "evil.example:8000"},
                json={"name": "Host"},
            ),
            403,
            "host_mismatch",
        ),
        (
            client.get("/api/fs/list", headers=AUTH, params={"path": "/etc/passwd"}),
            403,
            "path_escape",
        ),
        (
            client.get("/api/fs/list", headers=AUTH, params={"path": "../"}),
            403,
            "path_escape",
        ),
    ]

    for response, status_code, reason in checks:
        assert response.status_code == status_code
        assert reason in _security_reasons(app)


def test_case_sse_replays_then_polls_realtime_with_shared_route_predicate(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=2)
    engine = _engine(app)
    _seed_project_case(engine)
    first_id = _apply_events(
        engine,
        {
            "event": "TurnEnded",
            "turn_id": "turn_seen",
            "project_id": "project_1",
            "case_id": "case_1",
        },
    )[0]
    replay_id = _apply_events(
        engine,
        JobProgress(
            job_id="job_replay",
            project_id="project_1",
            requested_by_case_id="case_1",
            progress=0.5,
            payload={"kind": "render_preview"},
        ),
    )[0]
    realtime_ids: list[int] = []

    def emit_realtime() -> None:
        time.sleep(0.05)
        realtime_ids.extend(
            _apply_events(
                engine,
                JobProgress(
                    job_id="job_realtime",
                    project_id="project_1",
                    requested_by_case_id="case_1",
                    progress=1.0,
                    payload={"kind": "render_preview"},
                ),
            )
        )

    # 同步 TestClient 会把整个响应缓冲完才从 stream() 返回，
    # 因此"实时"事件必须在发起请求前就开始发射（服务端 sse_max_events=2 收尾）。
    thread = threading.Thread(target=emit_realtime)
    thread.start()
    with _client(app).stream(
        "GET",
        "/api/projects/project_1/cases/case_1/events",
        headers={**AUTH, "Last-Event-ID": str(first_id)},
    ) as response:
        assert response.status_code == 200
        events = _read_sse_events(response, 2)
    thread.join(timeout=1)

    assert [event["id"] for event in events] == [replay_id, realtime_ids[0]]
    assert [event["event"] for event in events] == ["JobProgress", "JobProgress"]


def test_workspace_sse_accepts_query_token_and_replays_workspace_events(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=2)
    engine = _engine(app)
    first_id = _apply_events(
        engine,
        ProjectCreated(project_id="project_seen", name="Seen"),
    )[0]
    replay_id = _apply_events(
        engine,
        ProjectCreated(project_id="project_replay", name="Replay"),
    )[0]
    realtime_ids: list[int] = []

    def emit_realtime() -> None:
        time.sleep(0.05)
        realtime_ids.extend(
            _apply_events(
                engine,
                ProjectCreated(project_id="project_realtime", name="Realtime"),
            )
        )

    thread = threading.Thread(target=emit_realtime)
    thread.start()
    with _client(app).stream(
        "GET",
        f"/api/events?token={TOKEN}",
        headers={"Last-Event-ID": str(first_id)},
    ) as response:
        assert response.status_code == 200
        events = _read_sse_events(response, 2)
    thread.join(timeout=1)

    assert [event["id"] for event in events] == [replay_id, realtime_ids[0]]
    assert [event["event"] for event in events] == ["ProjectCreated", "ProjectCreated"]


async def test_message_endpoint_records_and_runs_scripted_turn(tmp_path: Path) -> None:
    planner = ScriptedPlanner(
        [{"tool_name": "respond", "arguments": {"message": "收到，已进入队列。"}}]
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
        response = await client.post(
            "/api/projects/project_1/cases/case_1/messages",
            json={"message_id": "msg_user", "content": "帮我剪一个开头"},
        )
        assert response.status_code == 202
        await _state(app).turn_queue.join_case("case_1")

    messages = _messages(app, "case_1")
    assert [message["role"] for message in messages] == ["user", "assistant"]
    assert messages[0]["message_id"] == "msg_user"
    assert messages[1]["content"] == "收到，已进入队列。"


def test_fs_list_returns_directories_and_media_files_only(tmp_path: Path) -> None:
    root = tmp_path / "media"
    root.mkdir()
    (root / "clips").mkdir()
    (root / "clip.mp4").write_bytes(b"video")
    (root / "voice.wav").write_bytes(b"audio")
    (root / "notes.txt").write_text("ignore")
    app = _app(tmp_path, fs_roots=[root])

    response = _client(app).get("/api/fs/list", headers=AUTH, params={"path": str(root)})

    assert response.status_code == 200
    entries = response.json()["entries"]
    assert [entry["name"] for entry in entries] == ["clips", "clip.mp4", "voice.wav"]
    assert [entry["type"] for entry in entries] == ["directory", "file", "file"]


def test_current_decision_and_answer_route_use_reducer(tmp_path: Path) -> None:
    app = _app(tmp_path)
    engine = _engine(app)
    _seed_project_case(engine)
    _apply_events(
        engine,
        DecisionCreated(
            decision_id="decision_1",
            scope_type="case",
            project_id="project_1",
            case_id="case_1",
            payload={
                "decision": {
                    "decision_id": "decision_1",
                    "scope_type": "case",
                    "project_id": "project_1",
                    "case_id": "case_1",
                    "type": "generic",
                    "question": "继续吗？",
                    "options": [
                        {
                            "option_id": "yes",
                            "label": "Yes",
                            "payload": {
                                "reduce_target": "brief.confirmed_facts",
                                "text": "用户确认继续",
                            },
                        }
                    ],
                    "blocking": True,
                }
            },
        ),
        base_version=0,
    )
    client = _client(app)

    current = client.get(
        "/api/projects/project_1/cases/case_1/decisions/current",
        headers=AUTH,
    )
    assert current.status_code == 200
    assert current.json()["decision"]["decision_id"] == "decision_1"

    answered = client.post(
        "/api/decisions/decision_1/answer",
        headers=AUTH,
        json={
            "project_id": "project_1",
            "case_id": "case_1",
            "answer": {"option_id": "yes", "answered_via": "button", "payload": {}},
        },
    )
    assert answered.status_code == 200
    assert "DecisionAnswered" in _event_types(app)
    with engine.connect() as connection:
        case = CasesRepository(connection).get("case_1")
    assert case is not None
    assert case["pending_decision_id"] is None


def test_job_cancel_uses_reducer_and_enqueues_case_observation(tmp_path: Path) -> None:
    async def no_op_runner(item: TurnQueueItem, token: StopToken) -> None:
        del item, token

    app = _app(tmp_path, turn_runner=no_op_runner)
    engine = _engine(app)
    _seed_project_case(engine)
    _apply_events(
        engine,
        JobEnqueued(
            job_id="job_1",
            project_id="project_1",
            case_id="case_1",
            payload={"kind": "render_preview"},
        ),
    )

    response = _client(app).post("/api/jobs/job_1/cancel", headers=AUTH, json={})

    assert response.status_code == 200
    assert "JobCancelled" in _event_types(app)
    with engine.connect() as connection:
        job = JobsRepository(connection).get("job_1")
    assert job is not None
    assert job["status"] == "cancelled"


def _app(
    tmp_path: Path,
    *,
    fs_roots: Sequence[str | Path] | None = None,
    planner: ScriptedPlanner | None = None,
    turn_runner: Callable[[TurnQueueItem, StopToken], Awaitable[None]] | None = None,
    sse_max_events: int | None = None,
) -> FastAPI:
    return create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=fs_roots or [tmp_path / "allowed"],
        planner=planner,
        turn_runner=turn_runner,
        startup_port=8000,
        sse_max_events=sse_max_events,
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


def _apply_events(
    engine: Engine,
    *events: Any,
    base_version: int | None = None,
) -> list[int]:
    result = apply(events, engine=engine, base_version=base_version, actor="user")
    assert result.status == "applied"
    return [event.event_id for event in result.applied_events]


def _security_reasons(app: FastAPI) -> list[str]:
    return [
        str(row.payload_json["reason"])
        for row in _event_rows(app)
        if row.event_type == "SecurityRefusal"
    ]


def _event_types(app: FastAPI) -> list[str]:
    return [row.event_type for row in _event_rows(app)]


def _event_rows(app: FastAPI) -> list[Any]:
    with _engine(app).connect() as connection:
        return EventLogRepository(connection).read_after(0, limit=500)


def _messages(app: FastAPI, case_id: str) -> list[dict[str, Any]]:
    with _engine(app).connect() as connection:
        return MessagesRepository(connection).list_for_case(case_id)


def _read_sse_events(response: Any, count: int) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    current: dict[str, Any] = {}
    deadline = time.monotonic() + 2
    for line in response.iter_lines():
        if isinstance(line, bytes):
            line = line.decode()
        if time.monotonic() > deadline:
            raise AssertionError("timed out waiting for SSE events")
        if line == "":
            if current:
                events.append(current)
                current = {}
            if len(events) >= count:
                return events
            continue
        if line.startswith("id: "):
            current["id"] = int(line.removeprefix("id: "))
        elif line.startswith("event: "):
            current["event"] = line.removeprefix("event: ")
        elif line.startswith("data: "):
            current["data"] = json.loads(line.removeprefix("data: "))
    return events
