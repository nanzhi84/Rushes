"""Job observation bridge：终态 job 事件回灌草稿 Turn Queue（原属 tests/agent_harness，
因依赖 apps.api.main 迁到 tests/api）。"""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

import pytest

from agent_harness.turn_queue import TurnQueue, TurnQueueItem
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json

NOW = "2026-07-06T00:00:00+00:00"


def _engine(tmp_path: Path):
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
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


@pytest.mark.anyio
async def test_job_observation_bridge_enqueues_terminal_job_events(tmp_path: Path) -> None:
    from apps.api.main import _job_observation_bridge

    engine = _engine(tmp_path)
    received: list[TurnQueueItem] = []

    async def runner(item: TurnQueueItem, stop_token: Any) -> None:
        received.append(item)

    queue = TurnQueue(runner)
    task = asyncio.create_task(_job_observation_bridge(engine, queue, poll_interval=0.05))
    try:
        await asyncio.sleep(0.1)
        with engine.begin() as connection:
            connection.execute(
                schema.event_log.insert().values(
                    event_type="JobSucceeded",
                    actor="job",
                    draft_id="draft_1",
                    payload_json=dump_json(
                        {
                            "event": "JobSucceeded",
                            "job_id": "job_x",
                            "requested_by_draft_id": "draft_1",
                            "payload": {"kind": "asr"},
                        }
                    ),
                    created_at=NOW,
                )
            )
        for _ in range(40):
            await asyncio.sleep(0.05)
            if received:
                break
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await queue.shutdown()

    assert received
    assert received[0].kind == "job_observation"
    assert received[0].payload["job_id"] == "job_x"


@pytest.mark.anyio
async def test_job_observation_bridge_skips_material_jobs(tmp_path: Path) -> None:
    """proxy/index/poster 这类素材加工 job 的终态事件不该唤醒 Agent（导入不刷气泡）。"""
    from apps.api.main import _job_observation_bridge

    engine = _engine(tmp_path)
    received: list[TurnQueueItem] = []

    async def runner(item: TurnQueueItem, stop_token: Any) -> None:
        received.append(item)

    queue = TurnQueue(runner)
    task = asyncio.create_task(_job_observation_bridge(engine, queue, poll_interval=0.05))
    try:
        await asyncio.sleep(0.1)
        # 先灌三条素材加工 job（不应入队），再灌一条 asr（应入队）——
        # 只有 asr 进队即证明素材加工 job 被过滤，且轮询游标正常越过它们。
        with engine.begin() as connection:
            for kind, job_id in (
                ("proxy", "job_proxy"),
                ("index", "job_index"),
                ("poster", "job_poster"),
                ("asr", "job_asr"),
            ):
                connection.execute(
                    schema.event_log.insert().values(
                        event_type="JobSucceeded",
                        actor="job",
                        draft_id="draft_1",
                        payload_json=dump_json(
                            {
                                "event": "JobSucceeded",
                                "job_id": job_id,
                                "requested_by_draft_id": "draft_1",
                                "payload": {"kind": kind},
                            }
                        ),
                        created_at=NOW,
                    )
                )
        for _ in range(40):
            await asyncio.sleep(0.05)
            if received:
                break
    finally:
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        await queue.shutdown()

    assert [item.payload["job_id"] for item in received] == ["job_asr"]
