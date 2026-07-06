"""In-process turn stream hub: fan out loop events to live SSE subscribers.

The harness (``agent_harness.loop.run_turn``) emits synchronous ``TurnListener``
events while a turn runs inside the API event loop. This hub records the current
turn's events into a per-case snapshot buffer and pushes them onto each
subscriber's queue. Subscribers connect via the ``turn-stream`` SSE route, which
first replays the snapshot and then drains the live queue.

Layering: ``TurnListener`` is a duck-typed protocol defined in the harness;
this concrete hub lives on the apps side (apps may import anything).
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Mapping
from typing import Any

from agent_harness.loop import TurnListener

# 订阅者队列上限：慢消费者积压超过此值即被踢下线并关闭其 SSE，
# 避免单个卡住的浏览器把内存吃穿。
SUBSCRIBER_QUEUE_LIMIT = 1024

# turn 结束/出错后清空快照缓冲：新连接的订阅者不应再看到上一回合的事件。
_TERMINAL_EVENT_TYPES = frozenset({"turn_ended", "turn_error"})

# 队列里的关闭哨兵：hub 把它塞进被踢订阅者的队列，SSE 读取端据此收尾。
TURN_STREAM_CLOSED = object()


class _HubListener:
    """Per-case ``TurnListener`` returned to the runner."""

    __slots__ = ("_case_id", "_hub")

    def __init__(self, hub: TurnStreamHub, case_id: str) -> None:
        self._hub = hub
        self._case_id = case_id

    def emit(self, event: Mapping[str, Any]) -> None:
        self._hub.record(self._case_id, event)


class TurnStreamHub:
    def __init__(self, *, queue_limit: int = SUBSCRIBER_QUEUE_LIMIT) -> None:
        self._buffers: dict[str, list[dict[str, Any]]] = {}
        self._subscribers: dict[str, set[asyncio.Queue[Any]]] = {}
        self._queue_limit = queue_limit

    def listener_for(self, case_id: str) -> TurnListener:
        return _HubListener(self, case_id)

    async def subscribe(self, case_id: str) -> tuple[list[dict[str, Any]], asyncio.Queue[Any]]:
        """Return the current turn snapshot plus a fresh live-event queue."""

        queue: asyncio.Queue[Any] = asyncio.Queue()
        self._subscribers.setdefault(case_id, set()).add(queue)
        snapshot = [dict(event) for event in self._buffers.get(case_id, ())]
        return snapshot, queue

    def unsubscribe(self, case_id: str, queue: asyncio.Queue[Any]) -> None:
        subscribers = self._subscribers.get(case_id)
        if subscribers is None:
            return
        subscribers.discard(queue)
        if not subscribers:
            self._subscribers.pop(case_id, None)

    def record(self, case_id: str, event: Mapping[str, Any]) -> None:
        """Buffer + fan out one event. Never raises into the loop."""

        try:
            frozen = dict(event)
            self._buffers.setdefault(case_id, []).append(frozen)
            self._fanout(case_id, frozen)
            if frozen.get("type") in _TERMINAL_EVENT_TYPES:
                self._buffers.pop(case_id, None)
        except Exception:  # pragma: no cover - hub must never break the turn
            pass

    def _fanout(self, case_id: str, event: dict[str, Any]) -> None:
        subscribers = self._subscribers.get(case_id)
        if not subscribers:
            return
        for queue in list(subscribers):
            if queue.qsize() >= self._queue_limit:
                # 慢订阅者：踢出 fan-out 集合并塞入关闭哨兵，让其 SSE 收尾。
                subscribers.discard(queue)
                queue.put_nowait(TURN_STREAM_CLOSED)
                continue
            queue.put_nowait(event)


def encode_turn_stream_row(event: Mapping[str, Any]) -> str:
    """Encode one hub event as an SSE ``turn_stream`` frame (no id, no Last-Event-ID)."""

    payload = json.dumps(event, ensure_ascii=False, separators=(",", ":"))
    return f"event: turn_stream\ndata: {payload}\n\n"
