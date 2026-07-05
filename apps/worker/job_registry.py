"""Async job handler registry for apps.worker."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol

import httpx
from sqlalchemy.engine import Engine

from contracts.jobs import Job
from providers import ProviderGateway
from storage.workspace_paths import WorkspacePaths


@dataclass(frozen=True, slots=True)
class JobExecutionResult:
    """Normalized handler success payload."""

    result_json: dict[str, Any] = field(default_factory=dict)


class JobExecutionError(Exception):
    """Structured handler failure consumed by JobRunner retry policy."""

    def __init__(
        self,
        message: str,
        *,
        error_code: str = "job_handler_failed",
        retryable: bool = True,
        stderr_summary: str | None = None,
        details: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.error_code = error_code
        self.retryable = retryable
        self.stderr_summary = stderr_summary
        self.details = dict(details or {})


class JobCancelledError(Exception):
    """Raised by a handler when it observes cooperative cancellation."""


class JobHandler(Protocol):
    async def __call__(self, job: Job) -> JobExecutionResult | Mapping[str, Any]:
        """Execute a claimed job outside the SQLite write transaction."""


class JobHandlerRegistry:
    """Map job kind to async handler; adding a media job only registers a handler."""

    def __init__(self) -> None:
        self._handlers: dict[str, JobHandler] = {}

    def register(self, kind: str, handler: JobHandler) -> None:
        if kind in self._handlers:
            raise ValueError(f"job handler already registered: {kind}")
        self._handlers[kind] = handler

    def require(self, kind: str) -> JobHandler:
        handler = self._handlers.get(kind)
        if handler is None:
            raise KeyError(f"job handler is not registered: {kind}")
        return handler

    def kinds(self) -> tuple[str, ...]:
        return tuple(sorted(self._handlers))


async def noop_handler(job: Job) -> JobExecutionResult:
    """M0 smoke-test handler: echo payload without side effects."""

    return JobExecutionResult(
        result_json={
            "kind": job.kind,
            "payload": job.payload_json,
        }
    )


def build_default_job_registry(
    *,
    engine: Engine | None = None,
    workspace_paths: WorkspacePaths | None = None,
    http_transport: httpx.AsyncBaseTransport | None = None,
    provider_gateway: ProviderGateway | None = None,
) -> JobHandlerRegistry:
    registry = JobHandlerRegistry()
    registry.register("noop", noop_handler)
    if engine is not None:
        from .annotation_jobs import build_annotation_handler
        from .media_jobs import (
            build_import_url_handler,
            build_proxy_handler,
            workspace_paths_from_engine,
        )

        paths = workspace_paths or workspace_paths_from_engine(engine)
        registry.register("proxy", build_proxy_handler(engine, paths))
        registry.register(
            "annotation",
            build_annotation_handler(engine, paths, gateway=provider_gateway),
        )
        registry.register(
            "import_url",
            build_import_url_handler(engine, paths, http_transport=http_transport),
        )
    return registry
