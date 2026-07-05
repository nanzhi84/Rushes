"""SQLite-backed job runner for PRD §4.10 and §14.3."""

from __future__ import annotations

import asyncio
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import Any
from uuid import uuid4

from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from agent_harness.turn_queue import TurnQueue
from contracts.events import JobCancelled, JobFailed, JobSucceeded
from contracts.jobs import Job, JobError
from storage.db import begin_immediate
from storage.repositories import JobsRepository

from .heartbeat import heartbeat_until_done, utc_now
from .job_registry import (
    JobCancelledError,
    JobExecutionError,
    JobExecutionResult,
    JobHandlerRegistry,
    build_default_job_registry,
)


@dataclass(frozen=True, slots=True)
class JobRunResult:
    job_id: str | None
    status: str
    observation_enqueued: bool = False
    retry_scheduled: bool = False


class JobRunner:
    def __init__(
        self,
        *,
        engine: Engine,
        registry: JobHandlerRegistry | None = None,
        turn_queue: TurnQueue | None = None,
        worker_id: str | None = None,
        poll_interval_seconds: float = 0.25,
        heartbeat_interval_seconds: float = 5.0,
        heartbeat_timeout_seconds: float = 60.0,
    ) -> None:
        self._engine = engine
        self._registry = registry or build_default_job_registry(engine=engine)
        self._turn_queue = turn_queue
        self._worker_id = worker_id or f"worker_{uuid4().hex[:12]}"
        self._poll_interval_seconds = poll_interval_seconds
        self._heartbeat_interval_seconds = heartbeat_interval_seconds
        self._heartbeat_timeout_seconds = heartbeat_timeout_seconds
        self._stop = asyncio.Event()

    @property
    def worker_id(self) -> str:
        return self._worker_id

    def stop(self) -> None:
        self._stop.set()

    def recover_stale_running(self, *, now: datetime | None = None) -> int:
        current = now or utc_now()
        heartbeat_before = current - timedelta(seconds=self._heartbeat_timeout_seconds)
        with begin_immediate(self._engine) as connection:
            return JobsRepository(connection).reset_stale_running(
                heartbeat_before=heartbeat_before.isoformat(),
                next_run_at=current.isoformat(),
            )

    async def run_forever(self) -> None:
        self.recover_stale_running()
        while not self._stop.is_set():
            result = await self.run_once()
            if result.job_id is None:
                try:
                    await asyncio.wait_for(
                        self._stop.wait(),
                        timeout=self._poll_interval_seconds,
                    )
                except TimeoutError:
                    continue

    async def run_once(self) -> JobRunResult:
        job = self._claim_next()
        if job is None:
            return JobRunResult(job_id=None, status="idle")
        done = asyncio.Event()
        heartbeat_task = asyncio.create_task(
            heartbeat_until_done(
                engine=self._engine,
                job_id=job.job_id,
                worker_id=self._worker_id,
                done=done,
                interval_seconds=self._heartbeat_interval_seconds,
            )
        )
        try:
            handler = self._registry.require(job.kind)
            raw_result = await handler(job)
            if self._job_status(job.job_id) == "cancelled":
                enqueued = await self._emit_terminal_event(
                    job,
                    "cancelled",
                    error_json=_error_json(
                        JobExecutionError(
                            "job was cancelled",
                            error_code="job_cancelled",
                            retryable=False,
                        )
                    ),
                )
                return JobRunResult(
                    job_id=job.job_id,
                    status="cancelled",
                    observation_enqueued=enqueued,
                )
            result_json = _result_json(raw_result)
            enqueued = await self._emit_terminal_event(job, "succeeded", result_json=result_json)
            return JobRunResult(
                job_id=job.job_id,
                status="succeeded",
                observation_enqueued=enqueued,
            )
        except JobCancelledError:
            enqueued = await self._emit_terminal_event(
                job,
                "cancelled",
                error_json=_error_json(
                    JobExecutionError(
                        "job was cancelled",
                        error_code="job_cancelled",
                        retryable=False,
                    )
                ),
            )
            return JobRunResult(
                job_id=job.job_id,
                status="cancelled",
                observation_enqueued=enqueued,
            )
        except Exception as exc:
            error = _coerce_error(exc)
            attempts = job.attempts + 1
            if error.retryable and attempts <= job.max_retries:
                self._schedule_retry(job, attempts=attempts, error=error)
                return JobRunResult(job_id=job.job_id, status="pending", retry_scheduled=True)
            enqueued = await self._emit_terminal_event(
                job,
                "failed",
                error_json=_error_json(error),
                attempts=attempts,
            )
            return JobRunResult(job_id=job.job_id, status="failed", observation_enqueued=enqueued)
        finally:
            done.set()
            await heartbeat_task

    def _claim_next(self) -> Job | None:
        now = utc_now().isoformat()
        with begin_immediate(self._engine) as connection:
            repository = JobsRepository(connection)
            job_id = repository.claim_next(worker_id=self._worker_id, now=now)
            if job_id is None:
                return None
            row = repository.get(job_id)
        if row is None:
            raise RuntimeError(f"claimed job disappeared: {job_id}")
        return Job.model_validate(row)

    def _schedule_retry(self, job: Job, *, attempts: int, error: JobExecutionError) -> None:
        next_run_at = utc_now() + timedelta(seconds=_retry_delay_seconds(attempts))
        with begin_immediate(self._engine) as connection:
            JobsRepository(connection).schedule_retry(
                job.job_id,
                attempts=attempts,
                next_run_at=next_run_at.isoformat(),
                error_json=_error_json(error),
            )

    async def _emit_terminal_event(
        self,
        job: Job,
        status: str,
        *,
        result_json: dict[str, Any] | None = None,
        error_json: dict[str, Any] | None = None,
        attempts: int | None = None,
    ) -> bool:
        finished_at = utc_now().isoformat()
        event = _terminal_event(job, status, result_json, error_json, finished_at)
        reducer_result = apply(
            [event],
            engine=self._engine,
            base_version=None,
            actor="job",
            created_at=finished_at,
        )
        if reducer_result.status != "applied" or not reducer_result.applied_events:
            return False
        with begin_immediate(self._engine) as connection:
            JobsRepository(connection).finish(
                job.job_id,
                status=status,
                finished_at=finished_at,
                result_json=result_json,
                error_json=error_json,
                attempts=attempts,
            )
        return await self._route_observation(job, event)

    async def _route_observation(self, job: Job, event: dict[str, Any]) -> bool:
        if self._turn_queue is None:
            return False
        target_case_id = job.requested_by_case_id or job.case_id
        if target_case_id is None:
            return False
        await self._turn_queue.enqueue_job_observation(
            target_case_id,
            job_id=job.job_id,
            event=event,
        )
        return True

    def _job_status(self, job_id: str) -> str | None:
        with begin_immediate(self._engine) as connection:
            row = JobsRepository(connection).get(job_id)
        return None if row is None else str(row["status"])


def _terminal_event(
    job: Job,
    status: str,
    result_json: dict[str, Any] | None,
    error_json: dict[str, Any] | None,
    finished_at: str,
) -> dict[str, Any]:
    payload = {
        "kind": job.kind,
        "result": result_json,
        "error": error_json,
        "finished_at": finished_at,
    }
    if status == "succeeded":
        return JobSucceeded(
            job_id=job.job_id,
            project_id=job.project_id,
            case_id=job.case_id,
            requested_by_case_id=job.requested_by_case_id,
            payload=payload,
        ).model_dump(mode="json")
    if status == "cancelled":
        return JobCancelled(
            job_id=job.job_id,
            project_id=job.project_id,
            case_id=job.case_id,
            requested_by_case_id=job.requested_by_case_id,
            payload=payload,
        ).model_dump(mode="json")
    return JobFailed(
        job_id=job.job_id,
        project_id=job.project_id,
        case_id=job.case_id,
        requested_by_case_id=job.requested_by_case_id,
        payload=payload,
    ).model_dump(mode="json")


def _coerce_error(exc: Exception) -> JobExecutionError:
    if isinstance(exc, JobExecutionError):
        return exc
    if isinstance(exc, KeyError):
        return JobExecutionError(
            str(exc),
            error_code="job_handler_not_registered",
            retryable=False,
            details={"exception_type": type(exc).__name__},
        )
    return JobExecutionError(
        str(exc),
        retryable=True,
        details={"exception_type": type(exc).__name__},
    )


def _error_json(error: JobExecutionError) -> dict[str, Any]:
    return JobError(
        error_code=error.error_code,
        message=str(error),
        retryable=error.retryable,
        stderr_summary=error.stderr_summary,
        details=error.details,
    ).model_dump(mode="json")


def _result_json(raw_result: JobExecutionResult | Mapping[str, Any]) -> dict[str, Any]:
    if isinstance(raw_result, JobExecutionResult):
        return raw_result.result_json
    return dict(raw_result)


def _retry_delay_seconds(attempts: int) -> float:
    return float(min(60, 2 ** max(0, attempts - 1)))
