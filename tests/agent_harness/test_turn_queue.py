import asyncio

from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem


async def test_turn_queue_fifo_orders_user_message_before_observation() -> None:
    processed: list[str] = []

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        processed.append(str(item.item_id))

    queue = TurnQueue(runner)
    await queue.enqueue(TurnQueueItem(case_id="case_1", kind="user_message", item_id="message_b"))
    await queue.enqueue(TurnQueueItem(case_id="case_1", kind="job_observation", item_id="job_done"))
    await queue.join_all()
    await queue.shutdown()

    assert processed == ["message_b", "job_done"]


async def test_turn_queue_same_case_is_strictly_serial() -> None:
    current = 0
    max_seen = 0

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        nonlocal current, max_seen
        del item, token
        current += 1
        max_seen = max(max_seen, current)
        await asyncio.sleep(0.01)
        current -= 1

    queue = TurnQueue(runner)
    await asyncio.gather(
        queue.enqueue(TurnQueueItem(case_id="case_1", kind="user_message", item_id="a")),
        queue.enqueue(TurnQueueItem(case_id="case_1", kind="ui_observation", item_id="b")),
    )
    await queue.join_all()
    await queue.shutdown()

    assert max_seen == 1


async def test_turn_queue_different_cases_run_in_parallel() -> None:
    started: list[str] = []
    release = asyncio.Event()

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        started.append(item.case_id)
        await release.wait()

    queue = TurnQueue(runner)
    await queue.enqueue(TurnQueueItem(case_id="case_1", kind="user_message"))
    await queue.enqueue(TurnQueueItem(case_id="case_2", kind="user_message"))

    for _ in range(20):
        if set(started) == {"case_1", "case_2"}:
            break
        await asyncio.sleep(0.005)
    release.set()
    await queue.join_all()
    await queue.shutdown()

    assert set(started) == {"case_1", "case_2"}


async def test_turn_queue_stop_sets_current_turn_token_without_killing_runner() -> None:
    started = asyncio.Event()
    release = asyncio.Event()
    observed_cancel: list[bool] = []

    async def runner(item: TurnQueueItem, token: StopToken) -> None:
        del item
        started.set()
        await release.wait()
        observed_cancel.append(token.cancel_requested)

    queue = TurnQueue(runner)
    await queue.enqueue(TurnQueueItem(case_id="case_1", kind="user_message"))
    await started.wait()

    assert queue.request_stop("case_1")
    release.set()
    await queue.join_all()
    await queue.shutdown()

    assert observed_cancel == [True]
