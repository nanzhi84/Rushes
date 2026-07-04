"""Per-case FIFO turn queue."""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal

TurnQueueItemKind = Literal["user_message", "job_observation", "ui_observation"]


@dataclass(frozen=True, slots=True)
class TurnQueueItem:
    case_id: str
    kind: TurnQueueItemKind
    payload: dict[str, Any] = field(default_factory=dict)
    item_id: str | None = None
    enqueued_at: str = field(default_factory=lambda: datetime.now(UTC).isoformat())


@dataclass(slots=True)
class StopToken:
    cancel_requested: bool = False


TurnRunner = Callable[[TurnQueueItem, StopToken], Awaitable[None]]


class TurnQueue:
    """Strict FIFO per case, parallel across cases."""

    def __init__(self, runner: TurnRunner) -> None:
        self._runner = runner
        self._workers: dict[str, _CaseWorker] = {}
        self._lock = asyncio.Lock()

    async def enqueue(self, item: TurnQueueItem) -> None:
        async with self._lock:
            worker = self._workers.get(item.case_id)
            if worker is None or worker.done:
                worker = _CaseWorker(item.case_id, self._runner, self._remove_worker)
                self._workers[item.case_id] = worker
            await worker.enqueue(item)

    async def enqueue_user_message(
        self,
        case_id: str,
        *,
        content: str,
        message_id: str | None = None,
    ) -> None:
        await self.enqueue(
            TurnQueueItem(
                case_id=case_id,
                kind="user_message",
                item_id=message_id,
                payload={"content": content, "message_id": message_id},
            )
        )

    async def enqueue_job_observation(
        self,
        case_id: str,
        *,
        job_id: str,
        event: dict[str, Any],
    ) -> None:
        await self.enqueue(
            TurnQueueItem(
                case_id=case_id,
                kind="job_observation",
                item_id=job_id,
                payload={"job_id": job_id, "event": event},
            )
        )

    async def enqueue_ui_observation(
        self,
        case_id: str,
        *,
        observation_type: str,
        payload: dict[str, Any],
        item_id: str | None = None,
    ) -> None:
        await self.enqueue(
            TurnQueueItem(
                case_id=case_id,
                kind="ui_observation",
                item_id=item_id,
                payload={"observation_type": observation_type, **payload},
            )
        )

    def request_stop(self, case_id: str) -> bool:
        worker = self._workers.get(case_id)
        if worker is None:
            return False
        return worker.request_stop()

    async def join_case(self, case_id: str) -> None:
        worker = self._workers.get(case_id)
        if worker is None:
            return
        await worker.join()

    async def join_all(self) -> None:
        workers = list(self._workers.values())
        await asyncio.gather(*(worker.join() for worker in workers))

    async def shutdown(self) -> None:
        workers = list(self._workers.values())
        for worker in workers:
            worker.cancel()
        await asyncio.gather(*(worker.wait_done() for worker in workers), return_exceptions=True)
        self._workers.clear()

    def _remove_worker(self, case_id: str, worker: _CaseWorker) -> None:
        if self._workers.get(case_id) is worker:
            self._workers.pop(case_id, None)


class _CaseWorker:
    def __init__(
        self,
        case_id: str,
        runner: TurnRunner,
        remove_callback: Callable[[str, _CaseWorker], None],
    ) -> None:
        self.case_id = case_id
        self._runner = runner
        self._remove_callback = remove_callback
        self._queue: asyncio.Queue[TurnQueueItem] = asyncio.Queue()
        self._task = asyncio.create_task(self._run())
        self._current_stop_token: StopToken | None = None

    @property
    def done(self) -> bool:
        return self._task.done()

    async def enqueue(self, item: TurnQueueItem) -> None:
        await self._queue.put(item)

    def request_stop(self) -> bool:
        if self._current_stop_token is None:
            return False
        self._current_stop_token.cancel_requested = True
        return True

    async def join(self) -> None:
        await self._queue.join()
        if self._task.done():
            await self._task

    def cancel(self) -> None:
        self._task.cancel()

    async def wait_done(self) -> None:
        await self._task

    async def _run(self) -> None:
        try:
            while True:
                try:
                    item = await asyncio.wait_for(self._queue.get(), timeout=0.05)
                except TimeoutError:
                    if self._queue.empty():
                        return
                    continue
                token = StopToken()
                self._current_stop_token = token
                try:
                    await self._runner(item, token)
                finally:
                    self._current_stop_token = None
                    self._queue.task_done()
        finally:
            self._remove_callback(self.case_id, self)
