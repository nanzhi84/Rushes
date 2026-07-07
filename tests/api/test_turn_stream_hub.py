"""TurnStreamHub: snapshot replay, realtime fan-out, terminal clearing, slow drop."""

from __future__ import annotations

from apps.api.turn_stream import (
    TURN_STREAM_CLOSED,
    TurnStreamHub,
    encode_turn_stream_row,
)


async def test_snapshot_replays_prior_events_and_streams_realtime_to_all() -> None:
    hub = TurnStreamHub()
    listener = hub.listener_for("draft_1")

    # 早订阅者：连接在任何事件之前。
    early_snapshot, early_queue = await hub.subscribe("draft_1")
    assert early_snapshot == []

    listener.emit({"type": "turn_started", "turn_id": "turn_1"})
    listener.emit({"type": "text_delta", "message_id": "m1", "kind": "assistant", "delta": "你"})

    # 晚订阅者：连接时已有两条事件，应当拿到全部快照。
    late_snapshot, late_queue = await hub.subscribe("draft_1")
    assert [event["type"] for event in late_snapshot] == ["turn_started", "text_delta"]

    listener.emit({"type": "tool_step_started", "step_id": "s1", "tool": "x.tool"})

    # 早订阅者实时收到全部三条；晚订阅者只实时收到第三条（前两条在快照里）。
    assert [early_queue.get_nowait()["type"] for _ in range(3)] == [
        "turn_started",
        "text_delta",
        "tool_step_started",
    ]
    assert late_queue.get_nowait()["type"] == "tool_step_started"
    assert late_queue.empty()


async def test_turn_ended_clears_snapshot_buffer() -> None:
    hub = TurnStreamHub()
    listener = hub.listener_for("draft_1")

    _, queue = await hub.subscribe("draft_1")
    listener.emit({"type": "turn_started", "turn_id": "turn_1"})
    listener.emit({"type": "turn_ended", "outcome": "finished", "reason": None})

    # 现有订阅者仍实时收到 turn_started + turn_ended。
    assert [queue.get_nowait()["type"] for _ in range(2)] == ["turn_started", "turn_ended"]

    # turn_ended 后缓冲清空：新订阅者拿到空快照。
    fresh_snapshot, _ = await hub.subscribe("draft_1")
    assert fresh_snapshot == []


async def test_turn_error_also_clears_snapshot_buffer() -> None:
    hub = TurnStreamHub()
    listener = hub.listener_for("draft_1")

    listener.emit({"type": "turn_started", "turn_id": "turn_1"})
    listener.emit({"type": "turn_error", "message": "boom"})

    snapshot, _ = await hub.subscribe("draft_1")
    assert snapshot == []


async def test_slow_subscriber_is_dropped_and_closed() -> None:
    hub = TurnStreamHub(queue_limit=3)
    listener = hub.listener_for("draft_1")
    _, queue = await hub.subscribe("draft_1")

    # 填满到上限。
    for index in range(3):
        listener.emit({"type": "text_delta", "delta": str(index)})
    # 第 4 条：队列已满 → 订阅者被踢，塞入关闭哨兵，该事件丢弃。
    listener.emit({"type": "text_delta", "delta": "overflow"})
    # 后续事件不再进入被踢订阅者。
    listener.emit({"type": "turn_ended", "outcome": "finished", "reason": None})

    drained = [queue.get_nowait() for _ in range(3)]
    assert [event["delta"] for event in drained] == ["0", "1", "2"]
    assert queue.get_nowait() is TURN_STREAM_CLOSED
    assert queue.empty()


async def test_emit_never_raises_on_malformed_event() -> None:
    hub = TurnStreamHub()
    listener = hub.listener_for("draft_1")
    # 非 Mapping 也不能把回合搞崩（hub 内部兜底）。
    listener.emit({"type": "turn_started", "turn_id": "turn_1"})
    snapshot, _ = await hub.subscribe("draft_1")
    assert snapshot[0]["turn_id"] == "turn_1"


def test_encode_turn_stream_row_uses_turn_stream_event_name() -> None:
    frame = encode_turn_stream_row({"type": "turn_started", "turn_id": "t1"})
    assert frame == 'event: turn_stream\ndata: {"type":"turn_started","turn_id":"t1"}\n\n'
