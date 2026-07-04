"""Heartbeat helpers for claimed jobs."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from datetime import UTC, datetime

from sqlalchemy.engine import Engine

from storage.db import begin_immediate
from storage.repositories import JobsRepository

NowFn = Callable[[], datetime]


def utc_now() -> datetime:
    return datetime.now(UTC)


async def heartbeat_until_done(
    *,
    engine: Engine,
    job_id: str,
    worker_id: str,
    done: asyncio.Event,
    interval_seconds: float,
    now_fn: NowFn = utc_now,
) -> None:
    while not done.is_set():
        try:
            await asyncio.wait_for(done.wait(), timeout=interval_seconds)
        except TimeoutError:
            with begin_immediate(engine) as connection:
                JobsRepository(connection).heartbeat(
                    job_id,
                    worker_id=worker_id,
                    now=now_fn().isoformat(),
                )
