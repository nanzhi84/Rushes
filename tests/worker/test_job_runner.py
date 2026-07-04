import asyncio
from pathlib import Path
from typing import Any

from apps.worker.job_registry import (
    JobCancelledError,
    JobExecutionError,
    JobExecutionResult,
    JobHandlerRegistry,
)
from apps.worker.job_runner import JobRunner
from sqlalchemy.engine import Engine

from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem
from contracts.jobs import Job
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import CasesRepository, EventLogRepository, JobsRepository
from storage.repositories.projects import ProjectsRepository

NOW = "2026-07-04T00:00:00+00:00"


def _prepare_workspace(tmp_path: Path) -> Engine:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
    with begin_immediate(engine) as connection:
        ProjectsRepository(connection).insert(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "defaults": {"aspect_ratio": "9:16", "fps": 30},
                "created_at": NOW,
                "updated_at": NOW,
            }
        )
        CasesRepository(connection).insert(_case_values())
    return engine


def _case_values() -> dict[str, object]:
    return {
        "case_id": "case_1",
        "project_id": "project_1",
        "name": "Case",
        "state_version": 0,
        "status": "active",
        "pending_decision_id": None,
        "running_jobs": [],
        "last_error": None,
        "brief": {"goal": "test", "confirmed_facts": []},
        "content_plan": None,
        "audio_plan": None,
        "cut_plan": None,
        "candidate_pack_id": None,
        "timeline_current_version": None,
        "timeline_validated": False,
        "preview_current_id": None,
        "last_viewed_preview_id": None,
        "rough_cut_approved": False,
        "rough_cut_approved_version": None,
        "postprocess_plan": None,
        "export_current_id": None,
        "selected_asset_ids": [],
        "disabled_asset_ids": [],
        "scratch_memory": {},
    }


def _insert_job(engine: Engine, **overrides: Any) -> None:
    values: dict[str, Any] = {
        "job_id": "job_1",
        "kind": "noop",
        "status": "pending",
        "project_id": "project_1",
        "case_id": "case_1",
        "requested_by_case_id": None,
        "asset_id": None,
        "idempotency_key": "idem_1",
        "payload_json": {"ok": True},
        "result_json": None,
        "error_json": None,
        "attempts": 0,
        "max_retries": 2,
        "next_run_at": NOW,
        "progress": None,
        "worker_id": None,
        "heartbeat_at": None,
        "created_at": NOW,
        "started_at": None,
        "finished_at": None,
    }
    values.update(overrides)
    with begin_immediate(engine) as connection:
        JobsRepository(connection).insert(values)


def _registry(handler: Any) -> JobHandlerRegistry:
    registry = JobHandlerRegistry()
    registry.register("noop", handler)
    return registry


async def test_runner_success_emits_event_routes_case_observation(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine)
    observed: list[TurnQueueItem] = []

    async def handler(job: Job) -> JobExecutionResult:
        return JobExecutionResult({"job_id": job.job_id})

    async def turn_runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        observed.append(item)

    queue = TurnQueue(turn_runner)
    runner = JobRunner(
        engine=engine,
        registry=_registry(handler),
        turn_queue=queue,
        heartbeat_interval_seconds=0.01,
    )

    result = await runner.run_once()
    await queue.join_all()
    await queue.shutdown()

    assert result.status == "succeeded"
    assert result.observation_enqueued
    assert observed[0].kind == "job_observation"
    assert observed[0].case_id == "case_1"
    assert _job(engine, "job_1")["status"] == "succeeded"
    assert "JobSucceeded" in _event_types(engine)


async def test_runner_retryable_failure_schedules_backoff(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine)

    async def handler(job: Job) -> JobExecutionResult:
        del job
        raise JobExecutionError("temporary", error_code="temporary", retryable=True)

    runner = JobRunner(engine=engine, registry=_registry(handler))

    result = await runner.run_once()
    job = _job(engine, "job_1")

    assert result.status == "pending"
    assert result.retry_scheduled
    assert job["status"] == "pending"
    assert job["attempts"] == 1
    assert job["next_run_at"] != NOW
    assert "JobFailed" not in _event_types(engine)


async def test_runner_exhausted_failure_marks_failed(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine, attempts=2, max_retries=2)

    async def handler(job: Job) -> JobExecutionResult:
        del job
        raise JobExecutionError("still bad", error_code="bad", retryable=True)

    runner = JobRunner(engine=engine, registry=_registry(handler))

    result = await runner.run_once()
    job = _job(engine, "job_1")

    assert result.status == "failed"
    assert job["status"] == "failed"
    assert job["attempts"] == 3
    assert job["error_json"]["error_code"] == "bad"
    assert "JobFailed" in _event_types(engine)


async def test_runner_cancelled_handler_marks_cancelled(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine)

    async def handler(job: Job) -> JobExecutionResult:
        del job
        raise JobCancelledError

    runner = JobRunner(engine=engine, registry=_registry(handler))

    result = await runner.run_once()

    assert result.status == "cancelled"
    assert _job(engine, "job_1")["status"] == "cancelled"
    assert "JobCancelled" in _event_types(engine)


async def test_runner_updates_heartbeat_while_handler_runs(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine)

    async def handler(job: Job) -> JobExecutionResult:
        del job
        await asyncio.sleep(0.03)
        return JobExecutionResult({"ok": True})

    runner = JobRunner(
        engine=engine,
        registry=_registry(handler),
        worker_id="worker_1",
        heartbeat_interval_seconds=0.01,
    )

    await runner.run_once()
    job = _job(engine, "job_1")

    assert job["heartbeat_at"] is not None
    assert job["worker_id"] == "worker_1"


def test_runner_startup_resets_stale_running_jobs(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(
        engine,
        status="running",
        worker_id="dead_worker",
        started_at=NOW,
        heartbeat_at="2026-07-04T00:00:01+00:00",
    )
    runner = JobRunner(engine=engine, heartbeat_timeout_seconds=10)

    reset = runner.recover_stale_running(now=_dt("2026-07-04T00:01:00+00:00"))
    job = _job(engine, "job_1")

    assert reset == 1
    assert job["status"] == "pending"
    assert job["worker_id"] is None


async def test_project_job_without_requested_case_does_not_enqueue_observation(
    tmp_path: Path,
) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine, case_id=None)
    observed: list[TurnQueueItem] = []

    async def handler(job: Job) -> JobExecutionResult:
        del job
        return JobExecutionResult({"ok": True})

    async def turn_runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        observed.append(item)

    queue = TurnQueue(turn_runner)
    runner = JobRunner(engine=engine, registry=_registry(handler), turn_queue=queue)

    result = await runner.run_once()
    await queue.join_all()
    await queue.shutdown()

    assert result.status == "succeeded"
    assert not result.observation_enqueued
    assert observed == []


async def test_project_job_requested_by_case_routes_observation(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine, case_id=None, requested_by_case_id="case_1")
    observed: list[TurnQueueItem] = []

    async def handler(job: Job) -> JobExecutionResult:
        del job
        return JobExecutionResult({"ok": True})

    async def turn_runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        observed.append(item)

    queue = TurnQueue(turn_runner)
    runner = JobRunner(engine=engine, registry=_registry(handler), turn_queue=queue)

    result = await runner.run_once()
    await queue.join_all()
    await queue.shutdown()

    assert result.observation_enqueued
    assert observed[0].case_id == "case_1"


async def test_duplicate_terminal_event_does_not_enqueue_twice(tmp_path: Path) -> None:
    engine = _prepare_workspace(tmp_path)
    _insert_job(engine)
    observed: list[TurnQueueItem] = []

    async def handler(job: Job) -> JobExecutionResult:
        del job
        return JobExecutionResult({"ok": True})

    async def turn_runner(item: TurnQueueItem, token: StopToken) -> None:
        del token
        observed.append(item)

    queue = TurnQueue(turn_runner)
    runner = JobRunner(engine=engine, registry=_registry(handler), turn_queue=queue)

    await runner.run_once()
    duplicate = await runner._emit_terminal_event(
        Job.model_validate(_job(engine, "job_1")),
        "succeeded",
    )
    await queue.join_all()
    await queue.shutdown()

    assert not duplicate
    assert len(observed) == 1


def _job(engine: Engine, job_id: str) -> dict[str, Any]:
    with begin_immediate(engine) as connection:
        row = JobsRepository(connection).get(job_id)
    assert row is not None
    return row


def _event_types(engine: Engine) -> list[str]:
    with begin_immediate(engine) as connection:
        return [row.event_type for row in EventLogRepository(connection).read_after(0)]


def _dt(value: str):
    from datetime import datetime

    return datetime.fromisoformat(value)
