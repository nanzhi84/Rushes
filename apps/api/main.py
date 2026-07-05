"""FastAPI application factory for the local Rushes API."""

from __future__ import annotations

import asyncio
import logging
import os
import uuid
from collections.abc import AsyncIterator, Mapping, Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, cast

from fastapi import FastAPI, HTTPException, Request, status
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.loop import (
    LLMPlanner,
    ScriptedPlanner,
    recover_approved_pending_tool_calls,
    run_turn,
)
from agent_harness.reducer import ReducerApplyResult, apply
from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem, TurnRunner
from contracts.decision import DecisionAnswer
from contracts.events import (
    CaseCreated,
    DecisionAnswered,
    JobCancelled,
    ProjectCreated,
)
from providers.gateway import ProviderCallRecord
from providers.planner import build_openai_compatible_planner
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import (
    CasesRepository,
    DecisionsRepository,
    EventLogRepository,
    JobsRepository,
    MessagesRepository,
    ProviderCallsRepository,
)
from storage.repositories.projects import ProjectsRepository

from .deps import (
    MEDIA_EXTENSIONS,
    ApiState,
    PathEscapeError,
    SsePredicate,
    canonicalize_allowed_path,
    configured_fs_roots,
    encode_sse_row,
    event_row_matches,
    generate_token,
    refuse_path_escape,
    route_case,
    route_workspace,
    security_baseline_middleware,
    startup_port_from_env,
    state_from_request,
)

LOGGER = logging.getLogger("rushes.api")
SSE_POLL_INTERVAL_SECONDS = 0.05
SSE_BATCH_SIZE = 100
DEFAULT_LLM_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
DEFAULT_LLM_MODEL = "qwen-plus"


class ProjectCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str = "Untitled Project"
    defaults: dict[str, Any] = Field(default_factory=dict)


class CaseCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    name: str = "Untitled Case"
    brief: dict[str, Any] = Field(default_factory=lambda: {"goal": ""})


class MessageCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    content: str
    message_id: str | None = None


class DecisionAnswerRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    answer: DecisionAnswer
    project_id: str | None = None
    case_id: str | None = None


class JobCancelRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str | None = None


def create_app(
    workspace_path: str | Path,
    *,
    token: str | None = None,
    fs_roots: Sequence[str | Path] | None = None,
    planner: LLMPlanner | None = None,
    turn_runner: TurnRunner | None = None,
    startup_port: int | None = None,
    sse_max_events: int | None = None,
) -> FastAPI:
    """Create the M0 local API app bound to one workspace."""

    engine = create_workspace_engine(workspace_path)
    with engine.begin() as connection:
        schema.create_all(connection)

    api_token = token or generate_token()
    active_port = startup_port or startup_port_from_env()
    env_planner = planner or _planner_from_env(engine)
    queue: TurnQueue | None = None

    async def default_runner(item: TurnQueueItem, stop_token: StopToken) -> None:
        if queue is None:
            raise RuntimeError("turn queue is not initialized")
        active_planner = env_planner or ScriptedPlanner([])
        await run_turn(
            item,
            engine=engine,
            planner=active_planner,
            turn_queue=queue,
            stop_token=stop_token,
        )

    queue = TurnQueue(turn_runner or default_runner)
    app = FastAPI(title="Rushes API", version="0.1.0")
    app.state.api_state = ApiState(
        engine=engine,
        token=api_token,
        fs_roots=configured_fs_roots(fs_roots),
        turn_queue=queue,
        startup_port=active_port,
        sse_max_events=sse_max_events,
    )
    app.middleware("http")(security_baseline_middleware)
    _register_lifecycle(app)
    _register_routes(app)
    return app


def create_app_from_env() -> FastAPI:
    workspace = os.environ.get("RUSHES_WORKSPACE_PATH", str(Path.cwd() / ".rushes"))
    token = os.environ.get("RUSHES_API_TOKEN") or None
    return create_app(workspace, token=token, startup_port=startup_port_from_env())


class _StorageProviderCallRecorder:
    def __init__(self, engine: Engine) -> None:
        self._engine = engine

    def record_provider_call(self, record: ProviderCallRecord) -> None:
        with begin_immediate(self._engine) as connection:
            ProviderCallsRepository(connection).insert(
                {
                    "call_id": record.call_id,
                    "provider_id": record.provider_id,
                    "capability": record.capability,
                    "model": record.model,
                    "case_id": record.case_id,
                    "job_id": record.job_id,
                    "latency_ms": record.latency_ms,
                    "usage_json": record.usage_json,
                    "cost_estimate": record.cost_estimate,
                    "status": record.status,
                }
            )


def _planner_from_env(engine: Engine) -> LLMPlanner | None:
    api_key = os.environ.get("RUSHES_DASHSCOPE_API_KEY") or os.environ.get("RUSHES_LLM_API_KEY")
    if not api_key:
        return None
    return cast(
        LLMPlanner,
        build_openai_compatible_planner(
            base_url=os.environ.get("RUSHES_LLM_BASE_URL", DEFAULT_LLM_BASE_URL),
            api_key=api_key,
            model=os.environ.get("RUSHES_LLM_MODEL", DEFAULT_LLM_MODEL),
            recorder=_StorageProviderCallRecorder(engine),
        ),
    )


def _register_lifecycle(app: FastAPI) -> None:
    @app.on_event("startup")
    async def _startup() -> None:
        state = _state_from_app(app)
        url = f"http://127.0.0.1:{state.startup_port}/#t={state.token}"
        print(url, flush=True)
        LOGGER.info("Rushes API startup URL: %s", url)

    @app.on_event("shutdown")
    async def _shutdown() -> None:
        state = _state_from_app(app)
        await state.turn_queue.shutdown()


def _register_routes(app: FastAPI) -> None:
    @app.post("/api/projects", status_code=status.HTTP_201_CREATED)
    async def create_project(payload: ProjectCreateRequest, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        project_id = payload.project_id or _new_id("project")
        event = ProjectCreated(
            project_id=project_id,
            name=payload.name,
            payload={
                "name": payload.name,
                "defaults": payload.defaults,
                "status": "active",
            },
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        project = _require_project(state.engine, project_id)
        return {"project": project, "event_ids": _event_ids(result)}

    @app.get("/api/projects")
    async def list_projects(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        with state.engine.connect() as connection:
            rows = connection.execute(
                select(schema.projects).order_by(schema.projects.c.created_at)
            ).all()
            projects = [dict(row._mapping) for row in rows]
        return {"projects": projects}

    @app.get("/api/project-tree")
    async def project_tree(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        with state.engine.connect() as connection:
            project_rows = connection.execute(
                select(schema.projects).order_by(schema.projects.c.created_at)
            ).all()
            case_rows = connection.execute(select(schema.cases).order_by(schema.cases.c.name)).all()
        cases_by_project: dict[str, list[dict[str, Any]]] = {}
        for row in case_rows:
            values = dict(row._mapping)
            cases_by_project.setdefault(str(values["project_id"]), []).append(
                {
                    "case_id": values["case_id"],
                    "project_id": values["project_id"],
                    "name": values["name"],
                    "status": values["status"],
                }
            )
        projects = []
        for row in project_rows:
            values = dict(row._mapping)
            project_id = str(values["project_id"])
            projects.append(
                {
                    "project_id": project_id,
                    "name": values["name"],
                    "status": values["status"],
                    "cases": cases_by_project.get(project_id, []),
                }
            )
        return {"projects": projects}

    @app.post("/api/projects/{project_id}/cases", status_code=status.HTTP_201_CREATED)
    async def create_case(
        project_id: str,
        payload: CaseCreateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        case_id = payload.case_id or _new_id("case")
        event = CaseCreated(
            project_id=project_id,
            case_id=case_id,
            payload={
                "name": payload.name,
                "brief": payload.brief,
                "status": "active",
            },
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        case = _require_case(state.engine, project_id, case_id)
        return {"case": case, "event_ids": _event_ids(result)}

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/messages",
        status_code=status.HTTP_202_ACCEPTED,
    )
    async def enqueue_message(
        project_id: str,
        case_id: str,
        payload: MessageCreateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        message_id = payload.message_id or _new_id("msg")
        now = _now_iso()
        with begin_immediate(state.engine) as connection:
            MessagesRepository(connection).insert(
                {
                    "message_id": message_id,
                    "case_id": case_id,
                    "role": "user",
                    "content": payload.content,
                    "created_at": now,
                }
            )
        await state.turn_queue.enqueue(
            TurnQueueItem(
                case_id=case_id,
                kind="user_message",
                item_id=message_id,
                payload={
                    "content": payload.content,
                    "message_id": message_id,
                    "message_recorded": True,
                },
                enqueued_at=now,
            )
        )
        return {
            "status": "queued",
            "kind": "user_message",
            "project_id": project_id,
            "case_id": case_id,
            "message_id": message_id,
        }

    @app.get("/api/projects/{project_id}/cases/{case_id}/decisions/current")
    async def current_decision(project_id: str, case_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        case = _require_case(state.engine, project_id, case_id)
        decision_id = case.get("pending_decision_id")
        if not isinstance(decision_id, str):
            return {"decision": None}
        with state.engine.connect() as connection:
            decision = DecisionsRepository(connection).get(decision_id)
        if decision is None or decision.get("status") != "pending":
            return {"decision": None}
        return {"decision": decision}

    @app.post("/api/decisions/{decision_id}/answer")
    async def answer_decision(
        decision_id: str,
        payload: DecisionAnswerRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        decision = _require_decision(state.engine, decision_id)
        _validate_decision_ownership(decision, payload)
        base_version = _base_version_for_decision(state.engine, decision)
        event = DecisionAnswered(
            decision_id=decision_id,
            scope_type=decision["scope_type"],
            project_id=decision.get("project_id"),
            case_id=decision.get("case_id"),
            payload={"answer": payload.answer.model_dump(mode="json")},
        )
        result = apply((event,), engine=state.engine, base_version=base_version, actor="user")
        _ensure_applied(result)
        replay_count = await recover_approved_pending_tool_calls(
            engine=state.engine,
            turn_queue=state.turn_queue,
        )
        return {
            "decision_id": decision_id,
            "status": "answered",
            "event_ids": _event_ids(result),
            "replays_enqueued": replay_count,
        }

    @app.post("/api/jobs/{job_id}/cancel")
    async def cancel_job(
        job_id: str,
        payload: JobCancelRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        job = _require_job(state.engine, job_id)
        event_payload = {
            "kind": job["kind"],
            "finished_at": _now_iso(),
            "reason": payload.reason,
        }
        event = JobCancelled(
            job_id=job_id,
            project_id=job.get("project_id"),
            case_id=job.get("case_id"),
            requested_by_case_id=job.get("requested_by_case_id"),
            payload=event_payload,
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        target_case_id = _job_observation_case_id(job)
        if target_case_id is not None:
            await state.turn_queue.enqueue_job_observation(
                target_case_id,
                job_id=job_id,
                event=event.model_dump(mode="json"),
            )
        return {"job_id": job_id, "status": "cancelled", "event_ids": _event_ids(result)}

    @app.get("/api/fs/roots")
    async def fs_roots(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {
            "roots": [
                {
                    "path": str(root),
                    "name": _root_name(root),
                    "exists": root.exists(),
                }
                for root in state.fs_roots
            ]
        }

    @app.get("/api/fs/list")
    async def fs_list(path: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        try:
            root = canonicalize_allowed_path(path, state.fs_roots)
        except PathEscapeError:
            refuse_path_escape(request, path)
        if not root.exists() or not root.is_dir():
            raise HTTPException(status_code=404, detail={"reason": "not_found"})
        entries = _list_media_entries(root)
        return {"path": str(root), "entries": entries}

    @app.get("/api/projects/{project_id}/cases/{case_id}/events")
    async def case_events(
        project_id: str,
        case_id: str,
        request: Request,
    ) -> StreamingResponse:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        return _sse_response(request, state.engine, route_case(case_id))

    @app.get("/api/events")
    async def workspace_events(request: Request) -> StreamingResponse:
        state = state_from_request(request)
        return _sse_response(request, state.engine, route_workspace())


def _state_from_app(app: FastAPI) -> ApiState:
    return cast(ApiState, app.state.api_state)


def _sse_response(request: Request, engine: Engine, predicate: SsePredicate) -> StreamingResponse:
    cursor = _last_event_id(request)
    max_events = state_from_request(request).sse_max_events
    return StreamingResponse(
        _sse_stream(request, engine, cursor, predicate, max_events=max_events),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


async def _sse_stream(
    request: Request,
    engine: Engine,
    cursor: int,
    predicate: SsePredicate,
    *,
    max_events: int | None = None,
) -> AsyncIterator[str]:
    last_seen = cursor
    emitted_total = 0
    while True:
        emitted = False
        with engine.connect() as connection:
            rows = EventLogRepository(connection).read_after(last_seen, limit=SSE_BATCH_SIZE)
        for row in rows:
            last_seen = row.event_id
            if not event_row_matches(row, predicate):
                continue
            emitted = True
            emitted_total += 1
            yield encode_sse_row(row)
            if max_events is not None and emitted_total >= max_events:
                return
        if await request.is_disconnected():
            return
        if not emitted:
            await asyncio.sleep(SSE_POLL_INTERVAL_SECONDS)


def _last_event_id(request: Request) -> int:
    raw = request.headers.get("last-event-id") or request.query_params.get("last_event_id") or "0"
    try:
        return max(0, int(raw))
    except ValueError:
        return 0


def _require_project(engine: Engine, project_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = ProjectsRepository(connection).get(project_id)
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "project_not_found"})
    return row


def _require_case(engine: Engine, project_id: str, case_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = CasesRepository(connection).get(case_id)
    if row is None or row.get("project_id") != project_id:
        raise HTTPException(status_code=404, detail={"reason": "case_not_found"})
    return row


def _require_decision(engine: Engine, decision_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = DecisionsRepository(connection).get(decision_id)
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "decision_not_found"})
    return row


def _require_job(engine: Engine, job_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = JobsRepository(connection).get(job_id)
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "job_not_found"})
    return row


def _validate_decision_ownership(
    decision: Mapping[str, Any],
    payload: DecisionAnswerRequest,
) -> None:
    if payload.project_id is not None and payload.project_id != decision.get("project_id"):
        raise HTTPException(status_code=404, detail={"reason": "decision_not_found"})
    if payload.case_id is not None and payload.case_id != decision.get("case_id"):
        raise HTTPException(status_code=404, detail={"reason": "decision_not_found"})


def _base_version_for_decision(engine: Engine, decision: Mapping[str, Any]) -> int | None:
    if decision.get("scope_type") != "case":
        return None
    case_id = decision.get("case_id")
    project_id = decision.get("project_id")
    if not isinstance(case_id, str) or not isinstance(project_id, str):
        raise HTTPException(status_code=409, detail={"reason": "invalid_decision_scope"})
    case = _require_case(engine, project_id, case_id)
    return int(case["state_version"])


def _job_observation_case_id(job: Mapping[str, Any]) -> str | None:
    requested_by_case_id = job.get("requested_by_case_id")
    if isinstance(requested_by_case_id, str):
        return requested_by_case_id
    case_id = job.get("case_id")
    return case_id if isinstance(case_id, str) else None


def _ensure_applied(result: ReducerApplyResult) -> None:
    if result.status == "applied":
        return
    raise HTTPException(
        status_code=409,
        detail={
            "reason": f"reducer_{result.status}",
            "conflict": None
            if result.conflict is None
            else {
                "case_id": result.conflict.case_id,
                "expected_base_version": result.conflict.expected_base_version,
                "actual_state_version": result.conflict.actual_state_version,
                "event_type": result.conflict.event_type,
            },
        },
    )


def _event_ids(result: ReducerApplyResult) -> list[int]:
    return [event.event_id for event in result.applied_events]


def _list_media_entries(root: Path) -> list[dict[str, Any]]:
    entries: list[dict[str, Any]] = []
    for child in sorted(root.iterdir(), key=lambda path: (not path.is_dir(), path.name.lower())):
        if child.is_dir():
            entries.append(
                {
                    "name": child.name,
                    "path": str(child.resolve(strict=False)),
                    "type": "directory",
                }
            )
            continue
        if child.suffix.lower() not in MEDIA_EXTENSIONS:
            continue
        stat = child.stat()
        entries.append(
            {
                "name": child.name,
                "path": str(child.resolve(strict=False)),
                "type": "file",
                "size": stat.st_size,
                "extension": child.suffix.lower(),
            }
        )
    return entries


def _root_name(root: Path) -> str:
    if root == Path.home().resolve(strict=False):
        return "Home"
    return root.name or str(root)


def _new_id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
