"""FastAPI application factory for the local Rushes API."""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import json
import logging
import os
import uuid
from collections.abc import AsyncIterator, Iterator, Mapping, Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, NoReturn, cast

from fastapi import FastAPI, HTTPException, Request, status
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy import func, or_, select
from sqlalchemy.engine import Engine, Row

from agent_harness.decision_answering import DecisionAnswerResolver
from agent_harness.loop import (
    LLMPlanner,
    ScriptedPlanner,
    recover_approved_pending_tool_calls,
    run_turn,
)
from agent_harness.reducer import ReducerApplyResult, apply
from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem, TurnRunner
from contracts.asset import AssetKind, StorageMode
from contracts.decision import Decision, DecisionAnswer, DecisionOption, PendingToolCall
from contracts.events import (
    Actor,
    AssetImported,
    CaseCopied,
    CaseCreated,
    CaseMoved,
    CaseRenamed,
    CaseTrashed,
    DecisionAnswered,
    DecisionCreated,
    JobCancelled,
    JobEnqueued,
    PreviewViewed,
    ProjectCopied,
    ProjectCreated,
    ProjectRenamed,
    ProjectTrashed,
)
from contracts.tool_result import ToolResult
from media.invalidation import revalidate_project_references
from providers.decision_answering import build_openai_compatible_decision_answer_resolver
from providers.gateway import ProviderCallRecord
from providers.planner import build_openai_compatible_planner
from providers.tool_gateway import build_default_tool_gateway
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
from storage.repositories._json import load_json
from storage.repositories.projects import ProjectsRepository
from storage.workspace_paths import WorkspacePaths
from timeline import get_timeline_version
from timeline.summary import render_timeline_summary
from tools import ToolExecutionContext
from tools.annotation import retry as annotation_retry
from tools.asset import (
    disable_for_case as asset_disable_for_case,
)
from tools.asset import (
    import_local_file as asset_import_local_file,
)
from tools.asset import (
    link_to_project as asset_link_to_project,
)
from tools.asset import (
    select_for_case as asset_select_for_case,
)
from tools.asset import (
    unlink_from_project as asset_unlink_from_project,
)
from tools.asset import (
    upload_complete as asset_upload_complete,
)
from tools.specs import (
    AnnotationRetryInput,
    AssetDisableForCaseInput,
    AssetImportLocalFileInput,
    AssetLinkInput,
    AssetSelectForCaseInput,
    AssetUnlinkInput,
    AssetUploadCompleteInput,
)

from . import schemas as api_schemas
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


class ProjectUpdateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str


class ProjectCopyRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    name: str | None = None


class ConfirmRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    confirm: bool = False


class CaseCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    name: str = "Untitled Case"
    goal: str | None = None
    brief: dict[str, Any] = Field(default_factory=lambda: {"goal": ""})


class CaseUpdateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str


class CaseCopyRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    case_id: str | None = None
    name: str | None = None


class CaseMoveRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    target_project_id: str
    confirm: bool = False


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


class MaterialImportLocalRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    path: str
    storage_mode: StorageMode | None = None
    kind: AssetKind = AssetKind.VIDEO
    asset_id: str | None = None


class MaterialImportUrlRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    url: str
    filename: str | None = None
    kind: AssetKind = AssetKind.VIDEO
    max_bytes: int | None = None
    asset_id: str | None = None


class MaterialAssetLinkRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str
    enabled: bool = True
    note: str = ""


class MaterialPatchRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool | None = None
    reference_path: str | None = None


class CaseAssetScopeRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    asset_id: str


class UploadInitRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str
    filename: str
    size: int | None = None
    kind: AssetKind = AssetKind.VIDEO
    asset_id: str | None = None


class UploadCompleteRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    project_id: str | None = None
    asset_id: str | None = None
    kind: AssetKind | None = None


def create_app(
    workspace_path: str | Path,
    *,
    token: str | None = None,
    fs_roots: Sequence[str | Path] | None = None,
    planner: LLMPlanner | None = None,
    decision_answer_resolver: DecisionAnswerResolver | None = None,
    turn_runner: TurnRunner | None = None,
    startup_port: int | None = None,
    sse_max_events: int | None = None,
) -> FastAPI:
    """Create the local API app bound to one workspace."""

    workspace_candidate = Path(workspace_path).expanduser().resolve(strict=False)
    workspace_root = (
        workspace_candidate.parent if workspace_candidate.suffix == ".db" else workspace_candidate
    )
    workspace_paths = WorkspacePaths.from_root(workspace_root).initialize()
    engine = create_workspace_engine(workspace_paths)
    with engine.begin() as connection:
        schema.create_all(connection)

    api_token = token or generate_token()
    active_port = startup_port or startup_port_from_env()
    env_planner = planner or _planner_from_env(engine)
    env_decision_answer_resolver = decision_answer_resolver or _decision_answer_resolver_from_env(
        engine
    )
    queue: TurnQueue | None = None

    tool_gateway = build_default_tool_gateway(recorder=_StorageProviderCallRecorder(engine))

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
            decision_answer_resolver=env_decision_answer_resolver,
            tool_gateway=tool_gateway,
        )

    queue = TurnQueue(turn_runner or default_runner)
    app = FastAPI(title="Rushes API", version="0.1.0")
    app.state.api_state = ApiState(
        engine=engine,
        token=api_token,
        fs_roots=configured_fs_roots(fs_roots),
        workspace_paths=workspace_paths,
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
    return create_app(
        workspace,
        token=token,
        fs_roots=_fs_roots_from_env(),
        startup_port=startup_port_from_env(),
    )


def _fs_roots_from_env() -> list[str] | None:
    raw = os.environ.get("RUSHES_FS_ROOTS")
    if not raw:
        return None
    roots = [part for part in raw.split(os.pathsep) if part]
    return roots or None


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


def _decision_answer_resolver_from_env(engine: Engine) -> DecisionAnswerResolver | None:
    api_key = os.environ.get("RUSHES_DASHSCOPE_API_KEY") or os.environ.get("RUSHES_LLM_API_KEY")
    if not api_key:
        return None
    return build_openai_compatible_decision_answer_resolver(
        base_url=os.environ.get("RUSHES_LLM_BASE_URL", DEFAULT_LLM_BASE_URL),
        api_key=api_key,
        model=os.environ.get("RUSHES_LLM_MODEL", DEFAULT_LLM_MODEL),
        recorder=_StorageProviderCallRecorder(engine),
    )


def _response_docs(
    *,
    mutation: bool = False,
    not_found: bool = False,
    conflict: bool = False,
    path_escape: bool = False,
) -> dict[int | str, dict[str, Any]]:
    responses: dict[int | str, dict[str, Any]] = {
        401: {"model": api_schemas.SecurityRefusalResponse},
        403: {"model": api_schemas.SecurityRefusalResponse},
    }
    if path_escape:
        responses[403] = {
            "description": "Forbidden",
            "content": {
                "application/json": {
                    "schema": {
                        "oneOf": [
                            {"$ref": "#/components/schemas/SecurityRefusalResponse"},
                            {"$ref": "#/components/schemas/ErrorResponse"},
                        ]
                    }
                }
            },
        }
    if mutation:
        responses[415] = {"model": api_schemas.SecurityRefusalResponse}
    if not_found:
        responses[404] = {"model": api_schemas.ErrorResponse}
    if conflict:
        responses[409] = {"model": api_schemas.ErrorResponse}
    return responses


def _register_lifecycle(app: FastAPI) -> None:
    @app.on_event("startup")
    async def _startup() -> None:
        state = _state_from_app(app)
        url = f"http://127.0.0.1:{state.startup_port}/#t={state.token}"
        print(url, flush=True)
        LOGGER.info("Rushes API startup URL: %s", url)
        app.state.job_observation_bridge = asyncio.create_task(
            _job_observation_bridge(state.engine, state.turn_queue)
        )

    @app.on_event("shutdown")
    async def _shutdown() -> None:
        state = _state_from_app(app)
        bridge = getattr(app.state, "job_observation_bridge", None)
        if bridge is not None:
            bridge.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await bridge
        await state.turn_queue.shutdown()


async def _job_observation_bridge(
    engine: Engine,
    turn_queue: TurnQueue,
    *,
    poll_interval: float = 1.0,
) -> None:
    """独立 worker 进程完成 job 后只写 event_log；本轮询桥把
    JobSucceeded/JobFailed 转成 job_observation turn，Agent 才能被结果唤醒
    （M9 路径 1 实测：缺了它 agent 永远等不到 ASR 完成）。"""

    def _max_event_id() -> int:
        with engine.connect() as connection:
            row = connection.execute(select(func.max(schema.event_log.c.event_id))).scalar()
        return int(row or 0)

    cursor = _max_event_id()
    while True:
        await asyncio.sleep(poll_interval)
        try:
            with engine.connect() as connection:
                rows = connection.execute(
                    select(schema.event_log)
                    .where(schema.event_log.c.event_id > cursor)
                    .where(schema.event_log.c.event_type.in_(("JobSucceeded", "JobFailed")))
                    .order_by(schema.event_log.c.event_id)
                ).all()
                max_row = connection.execute(select(func.max(schema.event_log.c.event_id))).scalar()
            for row in rows:
                values = dict(row._mapping)
                payload = load_json(str(values["payload_json"]))
                if not isinstance(payload, dict):
                    continue
                case_id = payload.get("requested_by_case_id") or values.get("case_id")
                job_id = payload.get("job_id")
                if not isinstance(case_id, str) or not isinstance(job_id, str):
                    continue
                await turn_queue.enqueue_job_observation(case_id, job_id=job_id, event=payload)
            cursor = int(max_row or cursor)
        except asyncio.CancelledError:
            raise
        except Exception:  # pragma: no cover - 轮询桥绝不能死于单次故障
            LOGGER.exception("job observation bridge iteration failed")


def _register_routes(app: FastAPI) -> None:
    @app.post(
        "/api/projects",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.ProjectMutationResponse,
        responses=_response_docs(mutation=True, conflict=True),
    )
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

    @app.get(
        "/api/projects",
        response_model=api_schemas.ProjectListResponse,
        responses=_response_docs(),
    )
    async def list_projects(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {"projects": _list_projects(state.engine)}

    @app.get(
        "/api/project-tree",
        response_model=api_schemas.ProjectTreeResponse,
        responses=_response_docs(),
    )
    async def project_tree(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {"projects": _project_tree(state.engine)}

    @app.get(
        "/api/projects/{project_id}",
        response_model=api_schemas.ProjectPageResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_project(project_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        return _project_page_payload(state.engine, project_id)

    @app.get(
        "/api/projects/{project_id}/materials",
        response_model=api_schemas.MaterialsResponse,
        responses=_response_docs(not_found=True),
    )
    async def list_materials(project_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        invalidation = revalidate_project_references(state.engine, project_id, apply_events=apply)
        return _materials_payload(
            state.engine,
            project_id,
            invalidated_asset_ids=list(invalidation.invalidated_asset_ids),
        )

    @app.post(
        "/api/projects/{project_id}/materials/revalidate",
        response_model=api_schemas.MaterialsResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def revalidate_materials(project_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        invalidation = revalidate_project_references(state.engine, project_id, apply_events=apply)
        return _materials_payload(
            state.engine,
            project_id,
            invalidated_asset_ids=list(invalidation.invalidated_asset_ids),
        )

    @app.post(
        "/api/projects/{project_id}/materials/import-local",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True, path_escape=True),
    )
    async def import_local_material(
        project_id: str,
        payload: MaterialImportLocalRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        try:
            source = canonicalize_allowed_path(payload.path, state.fs_roots)
        except PathEscapeError:
            refuse_path_escape(request, payload.path)
        result = _run_asset_tool(
            state,
            tool_name="asset.import_local_file",
            handler=asset_import_local_file,
            input_model=AssetImportLocalFileInput(
                project_id=project_id,
                asset_id=payload.asset_id,
                path=str(source),
                storage_mode=payload.storage_mode or StorageMode.REFERENCE,
                kind=payload.kind,
            ),
            actor="user",
        )
        return {
            "project_id": project_id,
            "asset_id": result.data.get("asset_id"),
            "event_ids": result.data["event_ids"],
        }

    @app.post(
        "/api/projects/{project_id}/materials/import-url",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def import_url_material(
        project_id: str,
        payload: MaterialImportUrlRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        decision = _url_import_decision(project_id, payload)
        event = DecisionCreated(
            decision_id=decision.decision_id,
            scope_type=decision.scope_type,
            project_id=project_id,
            payload={
                "decision": decision.model_dump(mode="json"),
                "type": decision.type,
                "question": decision.question,
                "options": [option.model_dump(mode="json") for option in decision.options],
                "pending_tool_call": decision.pending_tool_call.model_dump(mode="json")
                if decision.pending_tool_call is not None
                else None,
                "pending_tool_call_status": decision.pending_tool_call_status,
                "blocking": decision.blocking,
            },
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "project_id": project_id,
            "asset_id": payload.asset_id,
            "decision_id": decision.decision_id,
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/materials/link",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def link_material(
        project_id: str,
        payload: MaterialAssetLinkRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        _require_asset(state.engine, payload.asset_id)
        result = _run_asset_tool(
            state,
            tool_name="asset.link_to_project",
            handler=asset_link_to_project,
            input_model=AssetLinkInput(
                project_id=project_id,
                asset_id=payload.asset_id,
                enabled=payload.enabled,
                note=payload.note,
            ),
            actor="user",
        )
        return {
            "project_id": project_id,
            "asset_id": payload.asset_id,
            "event_ids": result.data["event_ids"],
        }

    @app.post(
        "/api/projects/{project_id}/materials/unlink",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def unlink_material(
        project_id: str,
        payload: MaterialAssetLinkRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        _require_project_asset(state.engine, project_id, payload.asset_id)
        result = _run_asset_tool(
            state,
            tool_name="asset.unlink_from_project",
            handler=asset_unlink_from_project,
            input_model=AssetUnlinkInput(project_id=project_id, asset_id=payload.asset_id),
            actor="user",
        )
        return {
            "project_id": project_id,
            "asset_id": payload.asset_id,
            "event_ids": result.data["event_ids"],
        }

    @app.patch(
        "/api/projects/{project_id}/materials/{asset_id}",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True, path_escape=True),
    )
    async def patch_material(
        project_id: str,
        asset_id: str,
        payload: MaterialPatchRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project_asset(state.engine, project_id, asset_id)
        event_ids: list[int] = []
        if payload.enabled is not None:
            link_result = _run_asset_tool(
                state,
                tool_name="asset.link_to_project",
                handler=asset_link_to_project,
                input_model=AssetLinkInput(
                    project_id=project_id,
                    asset_id=asset_id,
                    enabled=payload.enabled,
                ),
                actor="user",
            )
            event_ids.extend(link_result.data["event_ids"])
        if payload.reference_path is not None:
            try:
                reference_path = canonicalize_allowed_path(payload.reference_path, state.fs_roots)
            except PathEscapeError:
                refuse_path_escape(request, payload.reference_path)
            event = _reference_relocated_event(state.engine, project_id, asset_id, reference_path)
            result = apply((event,), engine=state.engine, base_version=None, actor="user")
            _ensure_applied(result)
            event_ids.extend(_event_ids(result))
        return {"project_id": project_id, "asset_id": asset_id, "event_ids": event_ids}

    @app.post(
        "/api/projects/{project_id}/materials/{asset_id}/retry-annotation",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def retry_material_annotation(
        project_id: str,
        asset_id: str,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project_asset(state.engine, project_id, asset_id)
        result = _run_asset_tool(
            state,
            tool_name="annotation.retry",
            handler=annotation_retry,
            input_model=AnnotationRetryInput(project_id=project_id, asset_id=asset_id),
            actor="user",
        )
        return {
            "project_id": project_id,
            "asset_id": asset_id,
            "job_id": result.data.get("job_id"),
            "event_ids": result.data["event_ids"],
        }

    @app.patch(
        "/api/projects/{project_id}",
        response_model=api_schemas.ProjectMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def rename_project(
        project_id: str,
        payload: ProjectUpdateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        event = ProjectRenamed(
            project_id=project_id,
            name=payload.name,
            payload={"name": payload.name},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "project": _require_project(state.engine, project_id),
            "event_ids": _event_ids(result),
        }

    @app.delete(
        "/api/projects/{project_id}",
        response_model=api_schemas.ProjectMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def delete_project(
        project_id: str,
        payload: ConfirmRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_confirm(payload)
        _require_project(state.engine, project_id)
        event = ProjectTrashed(project_id=project_id)
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "project": _require_project(state.engine, project_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/copy",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.ProjectMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def copy_project(
        project_id: str,
        payload: ProjectCopyRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        source_project = _require_project(state.engine, project_id)
        new_project_id = payload.project_id or _new_id("project")
        event = ProjectCopied(
            project_id=new_project_id,
            source_project_id=project_id,
            payload={"name": payload.name or f"{source_project['name']} Copy"},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "project": _require_project(state.engine, new_project_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/cases",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def create_case(
        project_id: str,
        payload: CaseCreateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        case_id = payload.case_id or _new_id("case")
        brief = _brief_payload(payload.brief, payload.goal)
        event = CaseCreated(
            project_id=project_id,
            case_id=case_id,
            payload={
                "name": payload.name,
                "brief": brief,
                "status": "active",
            },
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        case = _require_case(state.engine, project_id, case_id)
        return {"case": case, "event_ids": _event_ids(result)}

    @app.get(
        "/api/projects/{project_id}/cases/{case_id}",
        response_model=api_schemas.CaseResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_case(project_id: str, case_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {"case": _require_case(state.engine, project_id, case_id)}

    @app.get(
        "/api/projects/{project_id}/cases/{case_id}/timeline",
        response_model=api_schemas.CaseTimelineResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_case_timeline(
        project_id: str,
        case_id: str,
        request: Request,
        version: int | None = None,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        case = _require_case(state.engine, project_id, case_id)
        project = _require_project(state.engine, project_id)
        requested_version = version if version is not None else case.get("timeline_current_version")
        if not isinstance(requested_version, int):
            raise HTTPException(status_code=404, detail={"reason": "not_found"})
        record = get_timeline_version(state.engine, case_id, requested_version)
        if record is None:
            raise HTTPException(status_code=404, detail={"reason": "not_found"})
        defaults = project.get("defaults")
        aspect_ratio = "9:16"
        if isinstance(defaults, Mapping) and isinstance(defaults.get("aspect_ratio"), str):
            aspect_ratio = str(defaults["aspect_ratio"])
        return {
            "case_id": case_id,
            "timeline_version": record.version,
            "timeline": record.timeline.model_dump(mode="json"),
            "summary": render_timeline_summary(record.timeline, aspect_ratio=aspect_ratio),
            "preview_id": _latest_preview_id(state.engine, case_id, record.version),
        }

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/previews/{preview_id}/viewed",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def preview_viewed(
        project_id: str,
        case_id: str,
        preview_id: str,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        _require_case_preview(state.engine, case_id, preview_id)
        event = PreviewViewed(project_id=project_id, case_id=case_id, preview_id=preview_id)
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "case": _require_case(state.engine, project_id, case_id),
            "event_ids": _event_ids(result),
        }

    @app.get(
        "/api/projects/{project_id}/cases/{case_id}/costs",
        response_model=api_schemas.CaseCostsResponse,
        responses=_response_docs(not_found=True),
    )
    async def case_costs(project_id: str, case_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        return {
            "project_id": project_id,
            "case_id": case_id,
            "costs": _case_cost_summary(state.engine, case_id),
        }

    @app.patch(
        "/api/projects/{project_id}/cases/{case_id}",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def rename_case(
        project_id: str,
        case_id: str,
        payload: CaseUpdateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        event = CaseRenamed(
            project_id=project_id,
            case_id=case_id,
            name=payload.name,
            payload={"name": payload.name},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "case": _require_case(state.engine, project_id, case_id),
            "event_ids": _event_ids(result),
        }

    @app.delete(
        "/api/projects/{project_id}/cases/{case_id}",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def delete_case(
        project_id: str,
        case_id: str,
        payload: ConfirmRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_confirm(payload)
        _require_case(state.engine, project_id, case_id)
        event = CaseTrashed(project_id=project_id, case_id=case_id)
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "case": _require_case(state.engine, project_id, case_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/copy",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def copy_case(
        project_id: str,
        case_id: str,
        payload: CaseCopyRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        source_case = _require_case(state.engine, project_id, case_id)
        new_case_id = payload.case_id or _new_id("case")
        event = CaseCopied(
            project_id=project_id,
            case_id=new_case_id,
            source_case_id=case_id,
            payload={"name": payload.name or f"{source_case['name']} Copy"},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "case": _require_case(state.engine, project_id, new_case_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/move",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def move_case(
        project_id: str,
        case_id: str,
        payload: CaseMoveRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_confirm(ConfirmRequest(confirm=payload.confirm))
        _require_case(state.engine, project_id, case_id)
        _require_project(state.engine, payload.target_project_id)
        event = CaseMoved(
            project_id=payload.target_project_id,
            case_id=case_id,
            source_project_id=project_id,
            target_project_id=payload.target_project_id,
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "case": _require_case(state.engine, payload.target_project_id, case_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/messages",
        status_code=status.HTTP_202_ACCEPTED,
        response_model=api_schemas.MessageQueuedResponse,
        responses=_response_docs(mutation=True, not_found=True),
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

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/assets/select",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def select_case_asset(
        project_id: str,
        case_id: str,
        payload: CaseAssetScopeRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        case = _require_case(state.engine, project_id, case_id)
        _require_project_asset(state.engine, project_id, payload.asset_id)
        result = _run_asset_tool(
            state,
            tool_name="asset.select_for_case",
            handler=asset_select_for_case,
            input_model=AssetSelectForCaseInput(case_id=case_id, asset_id=payload.asset_id),
            actor="user",
            base_version=int(case["state_version"]),
        )
        return {
            "case": _require_case(state.engine, project_id, case_id),
            "event_ids": result.data["event_ids"],
        }

    @app.post(
        "/api/projects/{project_id}/cases/{case_id}/assets/disable",
        response_model=api_schemas.CaseMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def disable_case_asset(
        project_id: str,
        case_id: str,
        payload: CaseAssetScopeRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        case = _require_case(state.engine, project_id, case_id)
        _require_project_asset(state.engine, project_id, payload.asset_id)
        result = _run_asset_tool(
            state,
            tool_name="asset.disable_for_case",
            handler=asset_disable_for_case,
            input_model=AssetDisableForCaseInput(case_id=case_id, asset_id=payload.asset_id),
            actor="user",
            base_version=int(case["state_version"]),
        )
        return {
            "case": _require_case(state.engine, project_id, case_id),
            "event_ids": result.data["event_ids"],
        }

    @app.get(
        "/api/projects/{project_id}/decisions/pending",
        response_model=api_schemas.PendingDecisionsResponse,
        responses=_response_docs(not_found=True),
    )
    async def pending_project_decisions(project_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, project_id)
        return {
            "project_id": project_id,
            "decisions": _pending_project_decisions(state.engine, project_id),
        }

    @app.get(
        "/api/projects/{project_id}/cases/{case_id}/decisions/current",
        response_model=api_schemas.CurrentDecisionResponse,
        response_model_exclude_unset=True,
        responses=_response_docs(not_found=True),
    )
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

    @app.post(
        "/api/decisions/{decision_id}/answer",
        response_model=api_schemas.DecisionAnswerResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
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
        try:
            result = apply((event,), engine=state.engine, base_version=base_version, actor="user")
        except ValueError as exc:
            # 归约层校验失败是客户端可修复错误（答案缺字段等），400 而非 500
            raise HTTPException(
                status_code=400,
                detail={"reason": "invalid_answer", "message": str(exc)},
            ) from exc
        _ensure_applied(result)
        project_job_replays = _maybe_enqueue_url_import_job(state.engine, decision_id)
        replay_count = await recover_approved_pending_tool_calls(
            engine=state.engine,
            turn_queue=state.turn_queue,
        )
        return {
            "decision_id": decision_id,
            "status": "answered",
            "event_ids": _event_ids(result),
            "replays_enqueued": replay_count + project_job_replays,
        }

    @app.post(
        "/api/jobs/{job_id}/cancel",
        response_model=api_schemas.JobCancelResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
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

    @app.post(
        "/api/uploads/init",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.UploadInitResponse,
        responses=_response_docs(mutation=True, not_found=True),
    )
    async def init_upload(payload: UploadInitRequest, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_project(state.engine, payload.project_id)
        upload_id = _new_id("upload")
        upload_dir = _upload_dir(state.workspace_paths, upload_id)
        upload_dir.mkdir(parents=True, exist_ok=False)
        _write_upload_manifest(
            upload_dir,
            {
                "upload_id": upload_id,
                "project_id": payload.project_id,
                "filename": payload.filename,
                "size": payload.size,
                "kind": payload.kind.value,
                "asset_id": payload.asset_id,
            },
        )
        return {
            "upload_id": upload_id,
            "part_url_template": f"/api/uploads/{upload_id}/parts/{{part_number}}",
            "complete_url": f"/api/uploads/{upload_id}/complete",
        }

    @app.put(
        "/api/uploads/{upload_id}/parts/{part_number}",
        response_model=api_schemas.UploadPartResponse,
        responses=_response_docs(mutation=True, not_found=True),
    )
    async def put_upload_part(
        upload_id: str,
        part_number: int,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        upload_dir = _require_upload_dir(state.workspace_paths, upload_id)
        body = await request.body()
        parts_dir = upload_dir / "parts"
        parts_dir.mkdir(parents=True, exist_ok=True)
        part_path = parts_dir / str(part_number)
        part_path.write_bytes(body)
        return {"upload_id": upload_id, "part_number": part_number, "size": len(body)}

    @app.post(
        "/api/uploads/{upload_id}/complete",
        response_model=api_schemas.UploadCompleteResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def complete_upload(
        upload_id: str,
        payload: UploadCompleteRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        upload_dir = _require_upload_dir(state.workspace_paths, upload_id)
        manifest = _read_upload_manifest(upload_dir)
        project_id = payload.project_id or str(manifest["project_id"])
        _require_project(state.engine, project_id)
        merged_path = _merge_upload_parts(upload_dir, str(manifest["filename"]))
        kind = payload.kind or AssetKind(str(manifest["kind"]))
        result = _run_asset_tool(
            state,
            tool_name="asset.upload_complete",
            handler=asset_upload_complete,
            input_model=AssetUploadCompleteInput(
                project_id=project_id,
                asset_id=payload.asset_id or manifest.get("asset_id"),
                path=str(merged_path),
                filename=str(manifest["filename"]),
                kind=kind,
            ),
            actor="user",
        )
        return {
            "upload_id": upload_id,
            "project_id": project_id,
            "asset_id": result.data["asset_id"],
            "event_ids": result.data["event_ids"],
        }

    @app.get(
        "/api/media/{asset_id}/proxy",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def media_proxy(asset_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        proxy_path = _require_proxy_path(state.engine, state.workspace_paths, asset_id)
        return _range_response(proxy_path, request)

    @app.get(
        "/api/media/preview/{preview_id}",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def media_preview(preview_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        preview_path = _require_preview_path(state.engine, state.workspace_paths, preview_id)
        return _range_response(preview_path, request)

    @app.get(
        "/api/media/export/{export_id}",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def media_export(export_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        export_path = _require_export_path(state.engine, state.workspace_paths, export_id)
        return _range_response(export_path, request)

    @app.get(
        "/api/fs/roots",
        response_model=api_schemas.FsRootsResponse,
        responses=_response_docs(),
    )
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

    @app.get(
        "/api/fs/list",
        response_model=api_schemas.FsListResponse,
        response_model_exclude_none=True,
        responses=_response_docs(not_found=True, path_escape=True),
    )
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

    @app.get(
        "/api/projects/{project_id}/cases/{case_id}/events",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def case_events(
        project_id: str,
        case_id: str,
        request: Request,
    ) -> StreamingResponse:
        state = state_from_request(request)
        _require_case(state.engine, project_id, case_id)
        return _sse_response(request, state.engine, route_case(case_id))

    @app.get(
        "/api/events",
        response_class=StreamingResponse,
        responses=_response_docs(),
    )
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


def _list_projects(engine: Engine) -> list[dict[str, Any]]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.projects).order_by(schema.projects.c.created_at)
        ).all()
    projects: list[dict[str, Any]] = []
    for row in rows:
        values = dict(row._mapping)
        values["defaults"] = load_json(values["defaults"])
        projects.append(values)
    return projects


def _project_tree(engine: Engine) -> list[dict[str, Any]]:
    with engine.connect() as connection:
        project_rows = connection.execute(
            select(schema.projects).order_by(schema.projects.c.created_at)
        ).all()
        case_rows = connection.execute(select(schema.cases).order_by(schema.cases.c.name)).all()
    cases_by_project: dict[str, list[dict[str, Any]]] = {}
    for row in case_rows:
        values = dict(row._mapping)
        project_id = str(values["project_id"])
        cases_by_project.setdefault(project_id, []).append(
            {
                "case_id": values["case_id"],
                "project_id": project_id,
                "name": values["name"],
                "status": values["status"],
            }
        )
    projects: list[dict[str, Any]] = []
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
    return projects


def _project_page_payload(engine: Engine, project_id: str) -> dict[str, Any]:
    project = _require_project(engine, project_id)
    with engine.connect() as connection:
        case_rows = connection.execute(
            select(schema.cases)
            .where(schema.cases.c.project_id == project_id)
            .order_by(schema.cases.c.name)
        ).all()
        asset_count = connection.execute(
            select(schema.project_asset_links.c.asset_id).where(
                schema.project_asset_links.c.project_id == project_id
            )
        ).all()
        memory_count = connection.execute(
            select(schema.memories.c.memory_id).where(schema.memories.c.project_id == project_id)
        ).all()
    cases = [
        {
            "case_id": row._mapping["case_id"],
            "project_id": row._mapping["project_id"],
            "name": row._mapping["name"],
            "status": row._mapping["status"],
            "brief": load_json(row._mapping["brief"]),
        }
        for row in case_rows
    ]
    return {
        "project": project,
        "cases": cases,
        "case_count": len(cases),
        "asset_count": len(asset_count),
        "memory_count": len(memory_count),
        "costs": _project_cost_summary(engine, project_id),
        "actions": {
            "create_case": f"/api/projects/{project_id}/cases",
            "materials": f"/projects/{project_id}/materials",
        },
    }


def _pending_project_decisions(engine: Engine, project_id: str) -> list[Decision]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.decisions)
            .where(schema.decisions.c.project_id == project_id)
            .where(schema.decisions.c.scope_type == "project")
            .where(schema.decisions.c.case_id.is_(None))
            .where(schema.decisions.c.status == "pending")
            .order_by(schema.decisions.c.decision_id)
        ).all()
    return [Decision.model_validate(_decode_decision_values(dict(row._mapping))) for row in rows]


def _decode_decision_values(values: dict[str, Any]) -> dict[str, Any]:
    decoded = dict(values)
    for key in ("options", "answer", "pending_tool_call"):
        raw_value = decoded.get(key)
        if isinstance(raw_value, str):
            decoded[key] = load_json(raw_value)
    return decoded


def _case_cost_summary(engine: Engine, case_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.provider_calls).where(schema.provider_calls.c.case_id == case_id)
        ).all()
    return _cost_summary([dict(row._mapping) for row in rows])


def _project_cost_summary(engine: Engine, project_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        case_ids = [
            str(row._mapping["case_id"])
            for row in connection.execute(
                select(schema.cases.c.case_id).where(schema.cases.c.project_id == project_id)
            ).all()
        ]
        filters = [schema.jobs.c.project_id == project_id]
        if case_ids:
            filters.append(schema.provider_calls.c.case_id.in_(case_ids))
        rows = connection.execute(
            select(schema.provider_calls)
            .select_from(
                schema.provider_calls.outerjoin(
                    schema.jobs,
                    schema.jobs.c.job_id == schema.provider_calls.c.job_id,
                )
            )
            .where(or_(*filters))
        ).all()
    return _cost_summary([dict(row._mapping) for row in rows])


def _cost_summary(rows: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    by_capability: dict[str, float] = {}
    by_provider: dict[str, float] = {}
    total = 0.0
    for row in rows:
        cost = row.get("cost_estimate")
        amount = float(cost) if isinstance(cost, int | float) else 0.0
        total += amount
        capability = str(row.get("capability") or "unknown")
        provider_id = str(row.get("provider_id") or "unknown")
        by_capability[capability] = by_capability.get(capability, 0.0) + amount
        by_provider[provider_id] = by_provider.get(provider_id, 0.0) + amount
    return {
        "provider_call_count": len(rows),
        "total_cost_estimate": total,
        "by_capability": by_capability,
        "by_provider": by_provider,
    }


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


def _latest_preview_id(engine: Engine, case_id: str, timeline_version: int) -> str | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews.c.preview_id)
            .where(schema.previews.c.case_id == case_id)
            .where(schema.previews.c.timeline_version == timeline_version)
            .order_by(schema.previews.c.created_at.desc(), schema.previews.c.preview_id.desc())
        ).first()
    if row is None:
        return None
    preview_id = row._mapping["preview_id"]
    return preview_id if isinstance(preview_id, str) else None


def _require_case_preview(engine: Engine, case_id: str, preview_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews)
            .where(schema.previews.c.preview_id == preview_id)
            .where(schema.previews.c.case_id == case_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "preview_not_found"})
    return dict(row._mapping)


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


def _require_asset(engine: Engine, asset_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "asset_not_found"})
    return dict(row._mapping)


def _require_project_asset(engine: Engine, project_id: str, asset_id: str) -> dict[str, Any]:
    _require_project(engine, project_id)
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets, schema.project_asset_links.c.enabled.label("link_enabled"))
            .select_from(
                schema.assets.join(
                    schema.project_asset_links,
                    schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.project_asset_links.c.project_id == project_id)
            .where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "asset_not_linked"})
    return dict(row._mapping)


def _materials_payload(
    engine: Engine,
    project_id: str,
    *,
    invalidated_asset_ids: list[str] | None = None,
) -> dict[str, Any]:
    with engine.connect() as connection:
        asset_rows = connection.execute(
            select(schema.assets, schema.project_asset_links.c.enabled.label("link_enabled"))
            .select_from(
                schema.assets.join(
                    schema.project_asset_links,
                    schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.project_asset_links.c.project_id == project_id)
            .order_by(schema.assets.c.asset_id)
        ).all()
        asset_ids = [str(row._mapping["asset_id"]) for row in asset_rows]
        job_rows: Sequence[Row[Any]] = []
        if asset_ids:
            job_rows = connection.execute(
                select(schema.jobs)
                .where(schema.jobs.c.asset_id.in_(asset_ids))
                .order_by(schema.jobs.c.created_at)
            ).all()
    jobs_by_asset: dict[str, list[dict[str, Any]]] = {asset_id: [] for asset_id in asset_ids}
    for row in job_rows:
        values = _decode_job_row(dict(row._mapping))
        asset_id = values.get("asset_id")
        if isinstance(asset_id, str):
            jobs_by_asset.setdefault(asset_id, []).append(
                {
                    "job_id": values["job_id"],
                    "kind": values["kind"],
                    "status": values["status"],
                    "progress": values["progress"],
                    "error_json": values["error_json"],
                }
            )
    assets = []
    for row in asset_rows:
        asset_id = str(row._mapping["asset_id"])
        assets.append(_material_asset_payload(dict(row._mapping), jobs_by_asset.get(asset_id, [])))
    return {
        "project_id": project_id,
        "assets": assets,
        "invalidated_asset_ids": invalidated_asset_ids or [],
    }


def _material_asset_payload(values: dict[str, Any], jobs: list[dict[str, Any]]) -> dict[str, Any]:
    probe = _load_json_if_str(values.get("probe"))
    failure = _load_json_if_str(values.get("failure"))
    proxy_object_hash = values.get("proxy_object_hash")
    usable = bool(values["usable"])
    return {
        "asset_id": values["asset_id"],
        "storage_mode": values["storage_mode"],
        "kind": values["kind"],
        "source": values["source"],
        "filename": values.get("filename") or "",
        "hash": values["hash"],
        "size": int(values["size"]),
        "mtime": values["mtime"],
        "ingest_status": values["ingest_status"],
        "annotation_status": values["annotation_status"],
        "annotation_pass": values["annotation_pass"],
        "index_status": values["index_status"],
        "usable": usable,
        "enabled": bool(values["link_enabled"]),
        "probe": probe if isinstance(probe, dict) else None,
        "proxy_object_hash": proxy_object_hash,
        "proxy_ready": isinstance(proxy_object_hash, str) and proxy_object_hash != "",
        "invalid": not usable and _failure_code(failure) == "reference_invalidated",
        "failure": failure if isinstance(failure, dict) else None,
        "jobs": jobs,
    }


def _decode_job_row(values: dict[str, Any]) -> dict[str, Any]:
    for key in ("payload_json", "result_json", "error_json"):
        values[key] = _load_json_if_str(values.get(key))
    return values


def _load_json_if_str(value: Any) -> Any:
    return load_json(value) if isinstance(value, str) else value


def _failure_code(value: Any) -> str | None:
    if not isinstance(value, Mapping):
        return None
    code = value.get("error_code")
    return code if isinstance(code, str) else None


def _run_asset_tool(
    state: ApiState,
    *,
    tool_name: str,
    handler: Any,
    input_model: Any,
    actor: Actor,
    base_version: int | None = None,
) -> ToolResult:
    with state.engine.connect() as connection:
        result = cast(
            ToolResult,
            handler(
                input_model,
                ToolExecutionContext(
                    tool_call_id=f"api_{tool_name.replace('.', '_')}_{uuid.uuid4().hex[:8]}",
                    turn_id="api",
                    readonly_connection=connection,
                    metadata={"workspace_paths": state.workspace_paths},
                ),
            ),
        )
    if result.status == "failed":
        reason = result.error.error_code if result.error is not None else "tool_failed"
        raise HTTPException(
            status_code=409,
            detail={"reason": reason},
        )
    reducer_result = apply(
        result.events,
        engine=state.engine,
        base_version=base_version,
        actor=actor,
    )
    _ensure_applied(reducer_result)
    data = dict(result.data)
    data["event_ids"] = _event_ids(reducer_result)
    return result.model_copy(update={"data": data})


def _reference_relocated_event(
    engine: Engine,
    project_id: str,
    asset_id: str,
    reference_path: Path,
) -> AssetImported:
    asset = _require_project_asset(engine, project_id, asset_id)
    if asset["storage_mode"] != StorageMode.REFERENCE.value:
        raise HTTPException(status_code=409, detail={"reason": "asset_is_not_reference"})
    stat = reference_path.stat()
    digest = _sha256_file(reference_path)
    return AssetImported(
        project_id=project_id,
        asset_id=asset_id,
        job_id=f"relocate_{asset_id}_{digest[:12]}",
        payload={
            "storage_mode": StorageMode.REFERENCE.value,
            "object_hash": None,
            "reference_path": str(reference_path),
            "kind": asset["kind"],
            "source": asset["source"],
            "filename": reference_path.name,
            "hash": digest,
            "mtime": stat.st_mtime_ns,
            "size": stat.st_size,
            "ingest_status": "imported",
            "annotation_status": "pending",
            "annotation_pass": "none",
            "index_status": "none",
            "usable": True,
            "failure": None,
        },
    )


def _url_import_decision(project_id: str, payload: MaterialImportUrlRequest) -> Decision:
    arguments: dict[str, Any] = {
        "project_id": project_id,
        "url": payload.url,
        "filename": payload.filename,
        "kind": payload.kind.value,
        "max_bytes": payload.max_bytes,
        "asset_id": payload.asset_id,
    }
    arguments = {key: value for key, value in arguments.items() if value is not None}
    fingerprint = _fingerprint(arguments)
    decision_id = f"dec_url_import_asset_import_url_{fingerprint[:16]}"
    pending = PendingToolCall(
        tool_name="asset.import_url",
        arguments=arguments,
        idempotency_key=f"asset.import_url:{project_id}:{fingerprint}:decision:{decision_id}",
        argument_fingerprint=fingerprint,
    )
    return Decision(
        decision_id=decision_id,
        scope_type="project",
        project_id=project_id,
        type="url_import",
        question="确认从 URL 导入素材？",
        options=[
            DecisionOption(option_id="approve", label="确认", payload={"approved": True}),
            DecisionOption(option_id="reject", label="取消", payload={"approved": False}),
        ],
        allow_free_text=False,
        status="pending",
        pending_tool_call=pending,
        pending_tool_call_status="pending",
        blocking=False,
    )


def _maybe_enqueue_url_import_job(engine: Engine, decision_id: str) -> int:
    with engine.connect() as connection:
        row = DecisionsRepository(connection).get(decision_id)
    if row is None:
        return 0
    decision = Decision.model_validate(row)
    if (
        decision.type != "url_import"
        or decision.pending_tool_call_status != "approved"
        or decision.pending_tool_call is None
    ):
        return 0
    event = _url_import_job_from_pending(decision.pending_tool_call)
    result = apply((event,), engine=engine, base_version=None, actor="user")
    _ensure_applied(result)
    with begin_immediate(engine) as connection:
        DecisionsRepository(connection).mark_pending_tool_call_replayed(
            decision_id,
            consumed_at=_now_iso(),
            replayed_tool_call_id=f"replay_{decision_id}",
        )
    return 1


def _url_import_job_from_pending(pending: PendingToolCall) -> JobEnqueued:
    project_id = pending.arguments.get("project_id")
    url = pending.arguments.get("url")
    if not isinstance(project_id, str) or not isinstance(url, str):
        raise HTTPException(status_code=409, detail={"reason": "invalid_url_import_decision"})
    asset_id = pending.arguments.get("asset_id")
    if not isinstance(asset_id, str) or asset_id == "":
        asset_id = "asset_" + hashlib.sha256(f"{project_id}:{url}".encode()).hexdigest()[:20]
    return JobEnqueued(
        job_id=_job_id("import_url", pending.idempotency_key),
        project_id=project_id,
        payload={
            "kind": "import_url",
            "asset_id": asset_id,
            "idempotency_key": pending.idempotency_key,
            "job_payload": {**pending.arguments, "asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _require_proxy_path(engine: Engine, paths: WorkspacePaths, asset_id: str) -> Path:
    asset = _require_asset(engine, asset_id)
    proxy_hash = asset.get("proxy_object_hash")
    if not isinstance(proxy_hash, str):
        raise HTTPException(status_code=404, detail={"reason": "proxy_not_ready"})
    proxy_path = paths.object_path(proxy_hash)
    if not proxy_path.exists():
        raise HTTPException(status_code=404, detail={"reason": "proxy_not_found"})
    return proxy_path


def _require_preview_path(engine: Engine, paths: WorkspacePaths, preview_id: str) -> Path:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews.c.object_hash).where(schema.previews.c.preview_id == preview_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "preview_not_found"})
    object_hash = row._mapping["object_hash"]
    if not isinstance(object_hash, str):
        raise HTTPException(status_code=404, detail={"reason": "preview_not_ready"})
    path = paths.object_path(object_hash)
    if not path.exists():
        raise HTTPException(status_code=404, detail={"reason": "preview_object_not_found"})
    return path


def _require_export_path(engine: Engine, paths: WorkspacePaths, export_id: str) -> Path:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.exports.c.object_hash).where(schema.exports.c.export_id == export_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "export_not_found"})
    object_hash = row._mapping["object_hash"]
    if not isinstance(object_hash, str):
        raise HTTPException(status_code=404, detail={"reason": "export_not_ready"})
    path = paths.object_path(object_hash)
    if not path.exists():
        raise HTTPException(status_code=404, detail={"reason": "export_object_not_found"})
    return path


def _range_response(path: Path, request: Request) -> StreamingResponse:
    file_size = path.stat().st_size
    start, end, partial = _parse_range_header(request.headers.get("range"), file_size)
    content_length = max(0, end - start + 1)
    headers = {
        "Accept-Ranges": "bytes",
        "Content-Length": str(content_length),
    }
    if partial:
        headers["Content-Range"] = f"bytes {start}-{end}/{file_size}"
    return StreamingResponse(
        _iter_file_range(path, start, end),
        status_code=status.HTTP_206_PARTIAL_CONTENT if partial else status.HTTP_200_OK,
        media_type=_media_type_for_path(path),
        headers=headers,
    )


def _parse_range_header(range_header: str | None, file_size: int) -> tuple[int, int, bool]:
    if range_header is None:
        return 0, file_size - 1, False
    if not range_header.startswith("bytes="):
        _raise_invalid_range(file_size)
    spec = range_header.removeprefix("bytes=").strip()
    if "," in spec or "-" not in spec:
        _raise_invalid_range(file_size)
    raw_start, raw_end = spec.split("-", 1)
    try:
        if raw_start == "":
            suffix_length = int(raw_end)
            if suffix_length <= 0:
                _raise_invalid_range(file_size)
            start = max(file_size - suffix_length, 0)
            end = file_size - 1
        else:
            start = int(raw_start)
            end = file_size - 1 if raw_end == "" else int(raw_end)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_416_REQUESTED_RANGE_NOT_SATISFIABLE,
            detail={"reason": "invalid_range"},
            headers={"Content-Range": f"bytes */{file_size}"},
        ) from exc
    if file_size <= 0 or start < 0 or end < start or start >= file_size:
        _raise_invalid_range(file_size)
    return start, min(end, file_size - 1), True


def _raise_invalid_range(file_size: int) -> NoReturn:
    raise HTTPException(
        status_code=status.HTTP_416_REQUESTED_RANGE_NOT_SATISFIABLE,
        detail={"reason": "invalid_range"},
        headers={"Content-Range": f"bytes */{file_size}"},
    )


def _iter_file_range(path: Path, start: int, end: int) -> Iterator[bytes]:
    if end < start:
        return
    remaining = end - start + 1
    with path.open("rb") as file:
        file.seek(start)
        while remaining > 0:
            chunk = file.read(min(1024 * 1024, remaining))
            if not chunk:
                break
            remaining -= len(chunk)
            yield chunk


def _media_type_for_path(path: Path) -> str:
    suffix = path.suffix.lower()
    if suffix == ".mp3":
        return "audio/mpeg"
    if suffix in {".m4a", ".aac"}:
        return "audio/aac"
    return "video/mp4"


def _upload_dir(paths: WorkspacePaths, upload_id: str) -> Path:
    return paths.tmp_dir / "uploads" / upload_id


def _require_upload_dir(paths: WorkspacePaths, upload_id: str) -> Path:
    upload_dir = _upload_dir(paths, upload_id)
    if not upload_dir.exists() or not upload_dir.is_dir():
        raise HTTPException(status_code=404, detail={"reason": "upload_not_found"})
    return upload_dir


def _write_upload_manifest(upload_dir: Path, manifest: Mapping[str, Any]) -> None:
    (upload_dir / "manifest.json").write_text(
        json.dumps(dict(manifest), ensure_ascii=False, sort_keys=True),
        encoding="utf-8",
    )


def _read_upload_manifest(upload_dir: Path) -> dict[str, Any]:
    try:
        payload = json.loads((upload_dir / "manifest.json").read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise HTTPException(status_code=404, detail={"reason": "upload_not_found"}) from exc
    if not isinstance(payload, dict):
        raise HTTPException(status_code=409, detail={"reason": "invalid_upload_manifest"})
    return payload


def _merge_upload_parts(upload_dir: Path, filename: str) -> Path:
    parts_dir = upload_dir / "parts"
    if not parts_dir.exists():
        raise HTTPException(status_code=409, detail={"reason": "upload_has_no_parts"})
    part_paths = sorted(parts_dir.iterdir(), key=lambda path: int(path.name))
    destination = upload_dir / f"complete_{Path(filename).name}"
    with destination.open("wb") as output:
        for part_path in part_paths:
            with part_path.open("rb") as part:
                for chunk in iter(lambda: part.read(1024 * 1024), b""):
                    output.write(chunk)
    return destination


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _fingerprint(arguments: Mapping[str, Any]) -> str:
    encoded = json.dumps(
        arguments,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def _job_id(kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


def _validate_decision_ownership(
    decision: Mapping[str, Any],
    payload: DecisionAnswerRequest,
) -> None:
    if payload.project_id is not None and payload.project_id != decision.get("project_id"):
        raise HTTPException(status_code=404, detail={"reason": "decision_not_found"})
    if payload.case_id is not None and payload.case_id != decision.get("case_id"):
        raise HTTPException(status_code=404, detail={"reason": "decision_not_found"})


def _require_confirm(payload: ConfirmRequest) -> None:
    if not payload.confirm:
        raise HTTPException(status_code=409, detail={"reason": "confirmation_required"})


def _brief_payload(brief: Mapping[str, Any], goal: str | None) -> dict[str, Any]:
    payload = dict(brief)
    if goal is not None:
        payload["goal"] = goal
    payload.setdefault("goal", "")
    return payload


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
