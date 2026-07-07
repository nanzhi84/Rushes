"""turn-stream SSE 端点：快照回放、帧格式、query-token 鉴权。"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
from apps.api.main import create_app
from fastapi import FastAPI

from agent_harness.reducer import apply
from contracts.events import DraftCreated

TOKEN = "test-token"
BASE_URL = "http://127.0.0.1:8000"
AUTH = {"Authorization": f"Bearer {TOKEN}"}


def _app(tmp_path: Path, *, sse_max_events: int | None = None) -> FastAPI:
    app = create_app(
        tmp_path / "workspace",
        token=TOKEN,
        fs_roots=[tmp_path / "allowed"],
        startup_port=8000,
        sse_max_events=sse_max_events,
    )
    engine = app.state.api_state.engine
    result = apply(
        (
            DraftCreated(
                draft_id="draft_1",
                payload={"name": "Draft", "brief": {"goal": "test"}},
            ),
        ),
        engine=engine,
        base_version=None,
        actor="user",
    )
    assert result.status == "applied"
    return app


def _parse_frames(text: str) -> list[dict[str, Any]]:
    frames: list[dict[str, Any]] = []
    for block in text.split("\n\n"):
        block = block.strip()
        if not block:
            continue
        event_name = ""
        data = ""
        for line in block.split("\n"):
            if line.startswith("event: "):
                event_name = line[len("event: ") :]
            elif line.startswith("data: "):
                data = line[len("data: ") :]
        if data:
            frames.append({"event": event_name, "data": json.loads(data)})
    return frames


async def test_turn_stream_replays_current_turn_snapshot(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=5)
    hub = app.state.api_state.turn_stream_hub
    listener = hub.listener_for("draft_1")
    # 预置一段"进行中"回合（不含终止事件，缓冲保留）。
    listener.emit({"type": "turn_started", "turn_id": "turn_1"})
    listener.emit({"type": "text_delta", "message_id": "m1", "kind": "assistant", "delta": "你好"})
    listener.emit(
        {"type": "message_completed", "message_id": "m1", "kind": "reply", "content": "你好"}
    )
    listener.emit({"type": "tool_step_started", "step_id": "s1", "tool": "x.tool"})
    listener.emit(
        {"type": "tool_step_finished", "step_id": "s1", "tool": "x.tool", "status": "succeeded"}
    )

    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app),
        base_url=BASE_URL,
        headers=AUTH,
    ) as client:
        response = await client.get(
            "/api/drafts/draft_1/turn-stream",
        )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/event-stream")
    frames = _parse_frames(response.text)
    assert [frame["event"] for frame in frames] == ["turn_stream"] * 5
    assert [frame["data"]["type"] for frame in frames] == [
        "turn_started",
        "text_delta",
        "message_completed",
        "tool_step_started",
        "tool_step_finished",
    ]
    assert frames[1]["data"]["delta"] == "你好"


async def test_turn_stream_accepts_query_token(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=1)
    hub = app.state.api_state.turn_stream_hub
    hub.listener_for("draft_1").emit({"type": "turn_started", "turn_id": "turn_1"})

    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app),
        base_url=BASE_URL,
    ) as client:
        # 无 Authorization 头，仅 query token（EventSource 无法设置请求头）。
        response = await client.get(
            f"/api/drafts/draft_1/turn-stream?token={TOKEN}",
        )

    assert response.status_code == 200
    frames = _parse_frames(response.text)
    assert frames[0]["data"]["type"] == "turn_started"


async def test_turn_stream_rejects_missing_token(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=1)

    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app),
        base_url=BASE_URL,
    ) as client:
        response = await client.get(
            "/api/drafts/draft_1/turn-stream",
        )

    assert response.status_code == 401


async def test_turn_stream_unknown_draft_returns_404(tmp_path: Path) -> None:
    app = _app(tmp_path, sse_max_events=1)

    async with httpx.AsyncClient(
        transport=httpx.ASGITransport(app=app),
        base_url=BASE_URL,
        headers=AUTH,
    ) as client:
        response = await client.get(
            "/api/drafts/missing/turn-stream",
        )

    assert response.status_code == 404
