"""FastAPI application factory for the local Rushes API."""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
import json
import logging
import os
import shutil
import subprocess
import sys
import uuid
from collections.abc import AsyncIterator, Iterator, Mapping, Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal, NoReturn, cast
from urllib.parse import urlsplit

from fastapi import FastAPI, HTTPException, Query, Request, Response, status
from fastapi.concurrency import run_in_threadpool
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, ConfigDict, Field
from sqlalchemy import func, select
from sqlalchemy.engine import Engine, Row

from agent_harness.decision_answering import DecisionAnswerResolver
from agent_harness.loop import (
    LLMPlanner,
    MappingPlannerAdapter,
    ScriptedPlanner,
    recover_approved_pending_tool_calls,
    run_turn,
)
from agent_harness.reducer import ReducerApplyResult, apply
from agent_harness.turn_queue import StopToken, TurnQueue, TurnQueueItem, TurnRunner
from contracts.asset import AssetKind, StorageMode
from contracts.decision import Decision, DecisionAnswer, DecisionOption, PendingToolCall
from contracts.draft import DraftState
from contracts.events import (
    Actor,
    AssetLinked,
    AssetUnlinked,
    DecisionAnswered,
    DecisionCreated,
    DraftCopied,
    DraftCreated,
    DraftRenamed,
    DraftTrashed,
    JobCancelled,
    JobEnqueued,
    PreviewViewed,
)
from contracts.tool_result import ToolResult
from contracts.workspace import WorkspaceDefaults
from media.invalidation import revalidate_draft_references
from providers.decision_answering import build_openai_compatible_decision_answer_resolver
from providers.gateway import ProviderCallRecord
from providers.planner import build_openai_compatible_planner
from providers.tool_gateway import build_default_tool_gateway
from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import (
    DecisionsRepository,
    DraftsRepository,
    EventLogRepository,
    JobsRepository,
    MaterialSummariesRepository,
    MessagesRepository,
    ProviderCallsRepository,
)
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths
from timeline import get_timeline_version
from timeline.summary import render_timeline_summary
from tools import ToolExecutionContext
from tools.asset import import_local_file as asset_import_local_file
from tools.specs import AssetImportLocalFileInput

from . import schemas as api_schemas
from .deps import (
    MATERIAL_KIND_BY_SUFFIX,
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
    route_draft,
    route_workspace,
    security_baseline_middleware,
    startup_port_from_env,
    state_from_request,
)
from .turn_stream import TURN_STREAM_CLOSED, TurnStreamHub, encode_turn_stream_row

LOGGER = logging.getLogger("rushes.api")
SSE_POLL_INTERVAL_SECONDS = 0.05
SSE_BATCH_SIZE = 100
DEFAULT_LLM_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
DEFAULT_LLM_MODEL = "qwen-plus"
# 首页草稿墙每卡封面上限（thumbnail_ready 素材，导入时间倒序）。
DRAFT_COVER_LIMIT = 4
# 只有 Agent 在回合内「等结果」的 job 种类，终态事件才该回灌成 job_observation turn
# 唤醒主循环；proxy/index/poster 这类素材加工 job 的进度只走 SSE 给 UI，绝不进对话
# （否则一次导入一批素材会刷出一堆「后台任务事件」气泡——真实素材包 31 个 .mov 实测）。
_AGENT_WAITED_JOB_KINDS: frozenset[str] = frozenset(
    {"asr", "tts", "align", "render_preview", "render_final", "import_url"}
)


class DraftCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    draft_id: str | None = None
    # 缺省时服务端生成剪映式日期名（本地时区，无前导零）；同名（不含 trashed）追加 (2)(3)…
    name: str | None = None
    goal: str | None = None
    brief: dict[str, Any] = Field(default_factory=lambda: {"goal": ""})
    # 缺省从 workspace defaults 拷贝（当前工作区无持久配置，等价于内置默认）。
    defaults: dict[str, Any] | None = None


class DraftUpdateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str


class DraftCopyRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    draft_id: str | None = None
    name: str | None = None


class ConfirmRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    confirm: bool = False


class MessageCreateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    content: str
    message_id: str | None = None


class DecisionAnswerRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    answer: DecisionAnswer
    draft_id: str | None = None


class JobCancelRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    reason: str | None = None


class FsPickRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    mode: Literal["files", "folder"] = "files"


class MaterialImportLocalRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    # 单路径（兼容旧调用）或批量路径；条目可为文件或目录，目录会递归导入并保留层级。
    path: str | None = None
    paths: list[str] | None = None
    storage_mode: StorageMode | None = None
    asset_id: str | None = None

    def all_paths(self) -> list[str]:
        merged = list(self.paths or [])
        if self.path:
            merged.insert(0, self.path)
        return merged


class MaterialImportUrlRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    url: str
    filename: str | None = None
    max_bytes: int | None = None
    asset_id: str | None = None


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
        apply_data_migrations(connection)

    api_token = token or generate_token()
    active_port = startup_port or startup_port_from_env()
    env_planner = planner or _planner_from_env(engine)
    env_decision_answer_resolver = decision_answer_resolver or _decision_answer_resolver_from_env(
        engine
    )
    queue: TurnQueue | None = None

    tool_gateway = build_default_tool_gateway(recorder=_StorageProviderCallRecorder(engine))
    turn_stream_hub = TurnStreamHub()

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
            turn_listener=turn_stream_hub.listener_for(item.draft_id),
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
        turn_stream_hub=turn_stream_hub,
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
                    "draft_id": record.draft_id,
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
    return MappingPlannerAdapter(
        build_openai_compatible_planner(
            base_url=os.environ.get("RUSHES_LLM_BASE_URL", DEFAULT_LLM_BASE_URL),
            api_key=api_key,
            model=os.environ.get("RUSHES_LLM_MODEL", DEFAULT_LLM_MODEL),
            recorder=_StorageProviderCallRecorder(engine),
        )
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
                draft_id = payload.get("requested_by_draft_id") or values.get("draft_id")
                job_id = payload.get("job_id")
                if not isinstance(draft_id, str) or not isinstance(job_id, str):
                    continue
                if _observation_job_kind(payload) not in _AGENT_WAITED_JOB_KINDS:
                    # 素材加工型 job（proxy/index/poster）不唤 Agent：进度经 SSE 给 UI。
                    continue
                await turn_queue.enqueue_job_observation(draft_id, job_id=job_id, event=payload)
            cursor = int(max_row or cursor)
        except asyncio.CancelledError:
            raise
        except Exception:  # pragma: no cover - 轮询桥绝不能死于单次故障
            LOGGER.exception("job observation bridge iteration failed")


def _register_routes(app: FastAPI) -> None:
    @app.post(
        "/api/drafts",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.DraftMutationResponse,
        responses=_response_docs(mutation=True, conflict=True),
    )
    async def create_draft(payload: DraftCreateRequest, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        draft_id = payload.draft_id or _new_id("draft")
        name = _resolve_draft_name(state.engine, payload.name)
        defaults = payload.defaults or WorkspaceDefaults().model_dump(mode="json")
        brief = _brief_payload(payload.brief, payload.goal)
        event = DraftCreated(
            draft_id=draft_id,
            payload={
                "name": name,
                "brief": brief,
                "defaults": defaults,
                "status": "active",
            },
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "draft": _require_draft(state.engine, draft_id),
            "event_ids": _event_ids(result),
        }

    @app.get(
        "/api/drafts",
        response_model=api_schemas.DraftListResponse,
        responses=_response_docs(),
    )
    async def list_drafts(request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {"drafts": _list_drafts(state.engine)}

    @app.get(
        "/api/drafts/{draft_id}",
        response_model=api_schemas.DraftResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_draft(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        return {"draft": _require_draft(state.engine, draft_id)}

    @app.patch(
        "/api/drafts/{draft_id}",
        response_model=api_schemas.DraftMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def rename_draft(
        draft_id: str,
        payload: DraftUpdateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        event = DraftRenamed(
            draft_id=draft_id,
            name=payload.name,
            payload={"name": payload.name},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "draft": _require_draft(state.engine, draft_id),
            "event_ids": _event_ids(result),
        }

    @app.delete(
        "/api/drafts/{draft_id}",
        response_model=api_schemas.DraftMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def delete_draft(
        draft_id: str,
        payload: ConfirmRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_confirm(payload)
        _require_draft(state.engine, draft_id)
        event = DraftTrashed(draft_id=draft_id)
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "draft": _require_draft(state.engine, draft_id),
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/drafts/{draft_id}/copy",
        status_code=status.HTTP_201_CREATED,
        response_model=api_schemas.DraftMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def copy_draft(
        draft_id: str,
        payload: DraftCopyRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        source_draft = _require_draft(state.engine, draft_id)
        new_draft_id = payload.draft_id or _new_id("draft")
        event = DraftCopied(
            draft_id=new_draft_id,
            source_draft_id=draft_id,
            payload={"name": payload.name or f"{source_draft['name']} Copy"},
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "draft": _require_draft(state.engine, new_draft_id),
            "event_ids": _event_ids(result),
        }

    @app.get(
        "/api/drafts/{draft_id}/materials",
        response_model=api_schemas.MaterialsResponse,
        responses=_response_docs(not_found=True),
    )
    async def list_materials(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        invalidation = revalidate_draft_references(state.engine, draft_id, apply_events=apply)
        return _materials_payload(
            state.engine,
            draft_id,
            invalidated_asset_ids=list(invalidation.invalidated_asset_ids),
        )

    @app.get(
        "/api/drafts/{draft_id}/materials/{asset_id}/summary",
        response_model=api_schemas.MaterialSummaryResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_material_summary(
        draft_id: str,
        asset_id: str,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft_asset(state.engine, draft_id, asset_id)
        with state.engine.connect() as connection:
            summary = MaterialSummariesRepository(connection).latest_ready(asset_id)
        if summary is None:
            raise HTTPException(status_code=404, detail={"reason": "summary_not_ready"})
        return {"asset_id": asset_id, "summary": summary["summary_json"]}

    @app.post(
        "/api/drafts/{draft_id}/materials/revalidate",
        response_model=api_schemas.MaterialsResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def revalidate_materials(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        invalidation = revalidate_draft_references(state.engine, draft_id, apply_events=apply)
        return _materials_payload(
            state.engine,
            draft_id,
            invalidated_asset_ids=list(invalidation.invalidated_asset_ids),
        )

    @app.post(
        "/api/drafts/{draft_id}/materials/import-local",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True, path_escape=True),
    )
    async def import_local_material(
        draft_id: str,
        payload: MaterialImportLocalRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        draft_state = _load_draft_state(state.engine, draft_id)
        requested_paths = payload.all_paths()
        if not requested_paths:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "error_code": "missing_path",
                    "message": "至少提供一个 path 或 paths 条目。",
                },
            )
        sources: list[Path] = []
        for raw_path in requested_paths:
            try:
                sources.append(canonicalize_allowed_path(raw_path, state.fs_roots))
            except PathEscapeError:
                refuse_path_escape(request, raw_path)

        def _import_all() -> dict[str, Any]:
            # 目录递归展开为 (文件, rel_dir)；展开的每个文件重新过 fs_roots 校验（防符号链接逃逸）。
            # 直接文件的不支持后缀在这里就 400（导入循环开始前），避免批量中途失败留下半批。
            plan, skipped = _expand_import_sources(sources, state.fs_roots)
            if payload.asset_id is not None and len(plan) != 1:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "error_code": "asset_id_requires_single_file",
                        "message": "asset_id 只能用于恰好一个文件的导入。",
                    },
                )
            candidate_paths = [str(file_path) for file_path, _ in plan]
            linked_paths = _draft_linked_reference_paths(state.engine, draft_id)
            global_assets = _global_assets_by_reference_path(state.engine, candidate_paths)
            asset_ids: list[str] = []
            event_ids: list[int] = []
            failed: list[str] = []
            duplicates: list[str] = []
            for file_path, rel_dir in plan:
                key = str(file_path)
                # 分支①：全局命中且本草稿已链 → duplicates，跳过。
                if key in linked_paths:
                    duplicates.append(file_path.name)
                    continue
                hit = global_assets.get(key)
                if hit is not None:
                    # 分支②：全局命中但本草稿未链 → 仅发 AssetLinked 秒建链；
                    # 缺 proxy/index 产物按现规则补队（同幂等键 merge，正常情况不入任何队）。
                    asset_ids.append(hit["asset_id"])
                    event_ids.extend(_link_existing_asset(state.engine, draft_id, hit, rel_dir))
                    continue
                # 分支③：未命中 → 现状链路 AssetImported + AssetLinked + JobEnqueued(proxy)。
                try:
                    result = _run_asset_tool(
                        state,
                        tool_name="asset.import_local_file",
                        handler=asset_import_local_file,
                        input_model=AssetImportLocalFileInput(
                            asset_id=payload.asset_id,
                            path=str(file_path),
                            storage_mode=payload.storage_mode or StorageMode.REFERENCE,
                            kind=_infer_material_kind(str(file_path)),
                            rel_dir=rel_dir,
                        ),
                        draft_state=draft_state,
                        actor="user",
                    )
                except OSError as error:
                    # 浏览与导入之间文件被删/改名/无权限：记入 failed，继续导剩余文件。
                    failed.append(f"{file_path.name}（{error.__class__.__name__}）")
                    continue
                imported_asset_id = result.data.get("asset_id")
                if isinstance(imported_asset_id, str):
                    asset_ids.append(imported_asset_id)
                event_ids.extend(result.data["event_ids"])
            return {
                "draft_id": draft_id,
                "asset_id": asset_ids[0] if asset_ids else None,
                "asset_ids": asset_ids,
                "skipped": skipped,
                "failed": failed,
                "duplicates": duplicates,
                "event_ids": event_ids,
            }

        # 扫描 + 哈希 + 逐文件落库都是同步阻塞操作，放线程池跑，避免冻结事件循环。
        return await run_in_threadpool(_import_all)

    @app.post(
        "/api/drafts/{draft_id}/materials/import-url",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def import_url_material(
        draft_id: str,
        payload: MaterialImportUrlRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        draft = _require_draft(state.engine, draft_id)
        decision = _url_import_decision(draft_id, payload)
        event = DecisionCreated(
            decision_id=decision.decision_id,
            scope_type=decision.scope_type,
            draft_id=draft_id,
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
        result = apply(
            (event,),
            engine=state.engine,
            base_version=int(draft["state_version"]),
            actor="user",
        )
        _ensure_applied(result)
        return {
            "draft_id": draft_id,
            "asset_id": payload.asset_id,
            "decision_id": decision.decision_id,
            "event_ids": _event_ids(result),
        }

    @app.delete(
        "/api/drafts/{draft_id}/materials/{asset_id}",
        response_model=api_schemas.MaterialMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def delete_material(
        draft_id: str,
        asset_id: str,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft_asset(state.engine, draft_id, asset_id)
        result = _run_asset_events(
            state,
            (AssetUnlinked(draft_id=draft_id, asset_id=asset_id),),
            actor="user",
        )
        return {
            "draft_id": draft_id,
            "asset_id": asset_id,
            "event_ids": _event_ids(result),
        }

    @app.post(
        "/api/drafts/{draft_id}/messages",
        status_code=status.HTTP_202_ACCEPTED,
        response_model=api_schemas.MessageQueuedResponse,
        responses=_response_docs(mutation=True, not_found=True),
    )
    async def enqueue_message(
        draft_id: str,
        payload: MessageCreateRequest,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        message_id = payload.message_id or _new_id("msg")
        now = _now_iso()
        with begin_immediate(state.engine) as connection:
            MessagesRepository(connection).insert(
                {
                    "message_id": message_id,
                    "draft_id": draft_id,
                    "role": "user",
                    "kind": "user",
                    "content": payload.content,
                    "created_at": now,
                }
            )
        await state.turn_queue.enqueue(
            TurnQueueItem(
                draft_id=draft_id,
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
            "draft_id": draft_id,
            "message_id": message_id,
        }

    @app.get(
        "/api/drafts/{draft_id}/messages",
        response_model=api_schemas.MessagesResponse,
        responses=_response_docs(not_found=True),
    )
    async def list_draft_messages(
        draft_id: str,
        request: Request,
        limit: int = Query(default=200, ge=1, le=1000),
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        with state.engine.connect() as connection:
            rows = MessagesRepository(connection).list_for_draft(draft_id, limit=limit)
        return {
            "draft_id": draft_id,
            "messages": [
                {
                    "message_id": str(row["message_id"]),
                    "role": str(row["role"]),
                    "kind": str(row["kind"]),
                    "content": str(row["content"]),
                    "created_at": str(row["created_at"]),
                }
                for row in rows
            ],
        }

    @app.get(
        "/api/drafts/{draft_id}/timeline",
        response_model=api_schemas.DraftTimelineResponse,
        responses=_response_docs(not_found=True),
    )
    async def get_draft_timeline(
        draft_id: str,
        request: Request,
        version: int | None = None,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        draft = _require_draft(state.engine, draft_id)
        requested_version = (
            version if version is not None else draft.get("timeline_current_version")
        )
        if not isinstance(requested_version, int):
            raise HTTPException(status_code=404, detail={"reason": "not_found"})
        record = get_timeline_version(state.engine, draft_id, requested_version)
        if record is None:
            raise HTTPException(status_code=404, detail={"reason": "not_found"})
        defaults = draft.get("defaults")
        aspect_ratio = "9:16"
        if isinstance(defaults, Mapping) and isinstance(defaults.get("aspect_ratio"), str):
            aspect_ratio = str(defaults["aspect_ratio"])
        return {
            "draft_id": draft_id,
            "timeline_version": record.version,
            "timeline": record.timeline.model_dump(mode="json"),
            "summary": render_timeline_summary(record.timeline, aspect_ratio=aspect_ratio),
            "preview_id": _latest_preview_id(state.engine, draft_id, record.version),
        }

    @app.post(
        "/api/drafts/{draft_id}/previews/{preview_id}/viewed",
        response_model=api_schemas.DraftMutationResponse,
        responses=_response_docs(mutation=True, not_found=True, conflict=True),
    )
    async def preview_viewed(
        draft_id: str,
        preview_id: str,
        request: Request,
    ) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        _require_draft_preview(state.engine, draft_id, preview_id)
        event = PreviewViewed(draft_id=draft_id, preview_id=preview_id)
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        return {
            "draft": _require_draft(state.engine, draft_id),
            "event_ids": _event_ids(result),
        }

    @app.get(
        "/api/drafts/{draft_id}/costs",
        response_model=api_schemas.DraftCostsResponse,
        responses=_response_docs(not_found=True),
    )
    async def draft_costs(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        return {
            "draft_id": draft_id,
            "costs": _draft_cost_summary(state.engine, draft_id),
        }

    @app.get(
        "/api/drafts/{draft_id}/decisions/current",
        response_model=api_schemas.CurrentDecisionResponse,
        response_model_exclude_unset=True,
        responses=_response_docs(not_found=True),
    )
    async def current_decision(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        draft = _require_draft(state.engine, draft_id)
        decision_id = draft.get("pending_decision_id")
        if not isinstance(decision_id, str):
            return {"decision": None}
        with state.engine.connect() as connection:
            decision = DecisionsRepository(connection).get(decision_id)
        if decision is None or decision.get("status") != "pending":
            return {"decision": None}
        return {"decision": decision}

    @app.get(
        "/api/drafts/{draft_id}/decisions/pending",
        response_model=api_schemas.PendingDecisionsResponse,
        responses=_response_docs(not_found=True),
    )
    async def pending_draft_decisions(draft_id: str, request: Request) -> dict[str, Any]:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        return {
            "draft_id": draft_id,
            "decisions": _pending_draft_decisions(state.engine, draft_id),
        }

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
            draft_id=decision.get("draft_id"),
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
        url_import_replays = _maybe_enqueue_url_import_job(state.engine, decision_id)
        replay_count = await recover_approved_pending_tool_calls(
            engine=state.engine,
            turn_queue=state.turn_queue,
        )
        return {
            "decision_id": decision_id,
            "status": "answered",
            "event_ids": _event_ids(result),
            "replays_enqueued": replay_count + url_import_replays,
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
            draft_id=job.get("draft_id"),
            requested_by_draft_id=job.get("requested_by_draft_id"),
            payload=event_payload,
        )
        result = apply((event,), engine=state.engine, base_version=None, actor="user")
        _ensure_applied(result)
        target_draft_id = _job_observation_draft_id(job)
        # 只有 Agent 等待型 job 取消才唤醒主循环；素材加工型（proxy/index/poster）取消不进对话。
        if target_draft_id is not None and job.get("kind") in _AGENT_WAITED_JOB_KINDS:
            await state.turn_queue.enqueue_job_observation(
                target_draft_id,
                job_id=job_id,
                event=event.model_dump(mode="json"),
            )
        return {"job_id": job_id, "status": "cancelled", "event_ids": _event_ids(result)}

    @app.api_route(
        "/api/media/{asset_id}/proxy",
        methods=["GET", "HEAD"],  # 播放器加载源前会 HEAD 探测 Content-Type，与 GET 同权
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def media_proxy(asset_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        proxy_path = _require_proxy_path(state.engine, state.workspace_paths, asset_id)
        return _range_response(proxy_path, request)

    @app.api_route(
        "/api/media/{asset_id}/thumbnail",
        methods=["GET", "HEAD"],  # 播放器加载源前会 HEAD 探测 Content-Type，与 GET 同权
        response_class=Response,
        responses=_response_docs(not_found=True),
    )
    async def media_thumbnail(asset_id: str, request: Request) -> Response:
        state = state_from_request(request)
        thumbnail_path = _require_thumbnail_path(state.engine, state.workspace_paths, asset_id)
        return Response(content=thumbnail_path.read_bytes(), media_type="image/jpeg")

    @app.api_route(
        "/api/media/preview/{preview_id}",
        methods=["GET", "HEAD"],  # 播放器加载源前会 HEAD 探测 Content-Type，与 GET 同权
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def media_preview(preview_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        preview_path = _require_preview_path(state.engine, state.workspace_paths, preview_id)
        return _range_response(preview_path, request)

    @app.api_route(
        "/api/media/export/{export_id}",
        methods=["GET", "HEAD"],  # 播放器加载源前会 HEAD 探测 Content-Type，与 GET 同权
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

    @app.post(
        "/api/fs/pick",
        response_model=api_schemas.FsPickResponse,
        responses=_response_docs(mutation=True),
    )
    async def fs_pick(payload: FsPickRequest, request: Request) -> dict[str, Any]:
        """弹出宿主机原生文件/文件夹选择对话框（macOS NSOpenPanel）并返回绝对路径。

        后端与用户同机，这是浏览器沙箱之外拿到磁盘路径、实现零拷贝 reference
        导入的唯一途径；非 macOS 或无 GUI 会话时报 available=false，前端提示改走对话导入。
        """

        state_from_request(request)
        if not _native_picker_available():
            return {"available": False, "paths": []}
        paths = await run_in_threadpool(_run_native_picker, payload.mode)
        if paths is None:
            return {"available": False, "paths": []}
        return {"available": True, "paths": paths}

    @app.get(
        "/api/drafts/{draft_id}/events",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def draft_events(draft_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        return _sse_response(request, state.engine, route_draft(draft_id))

    @app.get(
        "/api/drafts/{draft_id}/turn-stream",
        response_class=StreamingResponse,
        responses=_response_docs(not_found=True),
    )
    async def draft_turn_stream(draft_id: str, request: Request) -> StreamingResponse:
        state = state_from_request(request)
        _require_draft(state.engine, draft_id)
        return _turn_stream_response(request, state.turn_stream_hub, draft_id)

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


def _turn_stream_response(
    request: Request,
    hub: TurnStreamHub,
    draft_id: str,
) -> StreamingResponse:
    max_events = state_from_request(request).sse_max_events
    return StreamingResponse(
        _turn_stream(request, hub, draft_id, max_events=max_events),
        media_type="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


async def _turn_stream(
    request: Request,
    hub: TurnStreamHub,
    draft_id: str,
    *,
    max_events: int | None = None,
) -> AsyncIterator[str]:
    snapshot, queue = await hub.subscribe(draft_id)
    emitted_total = 0
    try:
        for event in snapshot:
            yield encode_turn_stream_row(event)
            emitted_total += 1
            if max_events is not None and emitted_total >= max_events:
                return
        while True:
            try:
                item = await asyncio.wait_for(queue.get(), timeout=SSE_POLL_INTERVAL_SECONDS)
            except TimeoutError:
                if await request.is_disconnected():
                    return
                continue
            if item is TURN_STREAM_CLOSED:
                return
            yield encode_turn_stream_row(item)
            emitted_total += 1
            if max_events is not None and emitted_total >= max_events:
                return
    finally:
        hub.unsubscribe(draft_id, queue)


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


def _resolve_draft_name(engine: Engine, provided_name: str | None) -> str:
    """缺省生成剪映式日期名（本地时区，无前导零）；同名（不含 trashed）追加 (2)(3)…。"""
    if provided_name is not None and provided_name.strip():
        base = provided_name
    else:
        now = datetime.now().astimezone()
        base = f"{now.month}月{now.day}日"
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.drafts.c.name).where(schema.drafts.c.status != "trashed")
        ).all()
    existing = {str(row._mapping["name"]) for row in rows}
    if base not in existing:
        return base
    suffix = 2
    while f"{base} ({suffix})" in existing:
        suffix += 1
    return f"{base} ({suffix})"


def _list_drafts(engine: Engine) -> list[dict[str, Any]]:
    """草稿墙列表：三条集合式 SQL（草稿行 + 素材计数 + 窗口函数封面），无 per-draft 循环。"""
    with engine.connect() as connection:
        draft_rows = connection.execute(
            select(
                schema.drafts.c.draft_id,
                schema.drafts.c.name,
                schema.drafts.c.status,
                schema.drafts.c.updated_at,
            )
            .where(schema.drafts.c.status == "active")
            .order_by(schema.drafts.c.updated_at.desc(), schema.drafts.c.draft_id)
        ).all()
        count_rows = connection.execute(
            select(
                schema.draft_asset_links.c.draft_id,
                func.count().label("material_count"),
            ).group_by(schema.draft_asset_links.c.draft_id)
        ).all()
        ranked = (
            select(
                schema.draft_asset_links.c.draft_id,
                schema.draft_asset_links.c.asset_id,
                func.row_number()
                .over(
                    partition_by=schema.draft_asset_links.c.draft_id,
                    order_by=(
                        schema.draft_asset_links.c.linked_at.desc(),
                        schema.draft_asset_links.c.asset_id.desc(),
                    ),
                )
                .label("rn"),
            )
            .select_from(
                schema.draft_asset_links.join(
                    schema.assets,
                    schema.assets.c.asset_id == schema.draft_asset_links.c.asset_id,
                )
            )
            .where(schema.assets.c.thumbnail_object_hash.is_not(None))
            .where(schema.assets.c.thumbnail_object_hash != "")
            .subquery()
        )
        cover_rows = connection.execute(
            select(ranked.c.draft_id, ranked.c.asset_id)
            .where(ranked.c.rn <= DRAFT_COVER_LIMIT)
            .order_by(ranked.c.draft_id, ranked.c.rn)
        ).all()
    counts = {
        str(row._mapping["draft_id"]): int(row._mapping["material_count"]) for row in count_rows
    }
    covers: dict[str, list[str]] = {}
    for row in cover_rows:
        covers.setdefault(str(row._mapping["draft_id"]), []).append(str(row._mapping["asset_id"]))
    drafts: list[dict[str, Any]] = []
    for row in draft_rows:
        draft_id = str(row._mapping["draft_id"])
        drafts.append(
            {
                "draft_id": draft_id,
                "name": row._mapping["name"],
                "status": row._mapping["status"],
                "updated_at": row._mapping["updated_at"],
                "material_count": counts.get(draft_id, 0),
                "cover_asset_ids": covers.get(draft_id, []),
            }
        )
    return drafts


def _pending_draft_decisions(engine: Engine, draft_id: str) -> list[Decision]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.decisions)
            .where(schema.decisions.c.draft_id == draft_id)
            .where(schema.decisions.c.scope_type == "draft")
            .where(schema.decisions.c.status == "pending")
            .where(schema.decisions.c.blocking.is_(False))
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


def _draft_cost_summary(engine: Engine, draft_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.provider_calls).where(schema.provider_calls.c.draft_id == draft_id)
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


def _require_draft(engine: Engine, draft_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = DraftsRepository(connection).get(draft_id)
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "draft_not_found"})
    return row


def _load_draft_state(engine: Engine, draft_id: str) -> DraftState:
    # 工具执行上下文要一个 DraftState；drafts 行多带 created_at/updated_at 两列，
    # DraftState extra="forbid" 会拒，validate 前先剔除。
    row = _require_draft(engine, draft_id)
    data = {key: value for key, value in row.items() if key not in ("created_at", "updated_at")}
    return DraftState.model_validate(data)


def _latest_preview_id(engine: Engine, draft_id: str, timeline_version: int) -> str | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews.c.preview_id)
            .where(schema.previews.c.draft_id == draft_id)
            .where(schema.previews.c.timeline_version == timeline_version)
            .order_by(schema.previews.c.created_at.desc(), schema.previews.c.preview_id.desc())
        ).first()
    if row is None:
        return None
    preview_id = row._mapping["preview_id"]
    return preview_id if isinstance(preview_id, str) else None


def _require_draft_preview(engine: Engine, draft_id: str, preview_id: str) -> dict[str, Any]:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews)
            .where(schema.previews.c.preview_id == preview_id)
            .where(schema.previews.c.draft_id == draft_id)
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


def _require_draft_asset(engine: Engine, draft_id: str, asset_id: str) -> dict[str, Any]:
    _require_draft(engine, draft_id)
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets)
            .select_from(
                schema.assets.join(
                    schema.draft_asset_links,
                    schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.draft_asset_links.c.draft_id == draft_id)
            .where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        raise HTTPException(status_code=404, detail={"reason": "asset_not_linked"})
    return dict(row._mapping)


def _draft_linked_reference_paths(engine: Engine, draft_id: str) -> set[str]:
    """本草稿已链接素材的 reference_path 集合（分支①去重用）。"""
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.assets.c.reference_path)
            .select_from(
                schema.assets.join(
                    schema.draft_asset_links,
                    schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.draft_asset_links.c.draft_id == draft_id)
            .where(schema.assets.c.reference_path.is_not(None))
        ).all()
    return {str(row[0]) for row in rows}


def _global_assets_by_reference_path(
    engine: Engine,
    candidate_paths: Sequence[str],
) -> dict[str, dict[str, Any]]:
    """全局按 reference_path 查已存在素材（分支②秒建链用；不限草稿）。"""
    if not candidate_paths:
        return {}
    with engine.connect() as connection:
        rows = connection.execute(
            select(
                schema.assets.c.asset_id,
                schema.assets.c.reference_path,
                schema.assets.c.proxy_object_hash,
                schema.assets.c.index_json,
            )
            .where(schema.assets.c.reference_path.in_(list(set(candidate_paths))))
            .where(schema.assets.c.reference_path.is_not(None))
        ).all()
    result: dict[str, dict[str, Any]] = {}
    for row in rows:
        values = dict(row._mapping)
        reference_path = values.get("reference_path")
        if isinstance(reference_path, str):
            result[reference_path] = {
                "asset_id": str(values["asset_id"]),
                "proxy_object_hash": values.get("proxy_object_hash"),
                "index_json": values.get("index_json"),
            }
    return result


def _link_existing_asset(
    engine: Engine,
    draft_id: str,
    hit: Mapping[str, Any],
    rel_dir: str | None,
) -> list[int]:
    """分支②：全局命中但本草稿未链——秒建链；缺 proxy/index 产物按现规则补队（同幂等键 merge）。"""
    asset_id = str(hit["asset_id"])
    link_payload: dict[str, Any] = {}
    if rel_dir:
        link_payload["rel_dir"] = rel_dir
    events: list[Any] = [AssetLinked(draft_id=draft_id, asset_id=asset_id, payload=link_payload)]
    proxy_hash = hit.get("proxy_object_hash")
    if not isinstance(proxy_hash, str) or proxy_hash == "":
        events.append(_proxy_job_event(draft_id=draft_id, asset_id=asset_id))
    elif hit.get("index_json") is None:
        events.append(_index_job_event(draft_id=draft_id, asset_id=asset_id))
    result = apply(tuple(events), engine=engine, base_version=None, actor="user")
    _ensure_applied(result)
    return _event_ids(result)


def _proxy_job_event(*, draft_id: str, asset_id: str) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:probe_proxy"
    return JobEnqueued(
        job_id=_job_id("proxy", idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
        payload={
            "kind": "proxy",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _index_job_event(*, draft_id: str, asset_id: str) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:index"
    return JobEnqueued(
        job_id=_job_id("index", idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
        payload={
            "kind": "index",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _materials_payload(
    engine: Engine,
    draft_id: str,
    *,
    invalidated_asset_ids: list[str] | None = None,
) -> dict[str, Any]:
    with engine.connect() as connection:
        asset_rows = connection.execute(
            select(
                schema.assets,
                schema.draft_asset_links.c.rel_dir.label("link_rel_dir"),
            )
            .select_from(
                schema.assets.join(
                    schema.draft_asset_links,
                    schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
                )
            )
            .where(schema.draft_asset_links.c.draft_id == draft_id)
            .order_by(schema.draft_asset_links.c.linked_at, schema.assets.c.asset_id)
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
        "draft_id": draft_id,
        "assets": assets,
        "invalidated_asset_ids": invalidated_asset_ids or [],
    }


def _material_asset_payload(values: dict[str, Any], jobs: list[dict[str, Any]]) -> dict[str, Any]:
    probe = _load_json_if_str(values.get("probe"))
    failure = _load_json_if_str(values.get("failure"))
    proxy_object_hash = values.get("proxy_object_hash")
    thumbnail_object_hash = values.get("thumbnail_object_hash")
    usable = bool(values["usable"])
    duration_sec = probe.get("duration_sec") if isinstance(probe, dict) else None
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
        "understanding_status": values.get("understanding_status") or "none",
        "usable": usable,
        "rel_dir": values.get("link_rel_dir"),
        "probe": probe if isinstance(probe, dict) else None,
        "duration_sec": duration_sec if isinstance(duration_sec, (int, float)) else None,
        "proxy_object_hash": proxy_object_hash,
        "proxy_ready": isinstance(proxy_object_hash, str) and proxy_object_hash != "",
        "thumbnail_ready": isinstance(thumbnail_object_hash, str) and thumbnail_object_hash != "",
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
    draft_state: DraftState,
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
                    draft_state=draft_state,
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


def _run_asset_events(
    state: ApiState,
    events: tuple[Any, ...],
    *,
    actor: Actor,
    base_version: int | None = None,
) -> ReducerApplyResult:
    result = apply(events, engine=state.engine, base_version=base_version, actor=actor)
    _ensure_applied(result)
    return result


# 单一定义在 apps/api/deps.py（与 fs/list 的 MEDIA_EXTENSIONS 同源）。
_MATERIAL_KIND_BY_SUFFIX = MATERIAL_KIND_BY_SUFFIX


def _native_picker_available() -> bool:
    return sys.platform == "darwin" and shutil.which("osascript") is not None


# choose file/folder 是 StandardAdditions 用户交互命令；包一层 with timeout 防
# AppleEvent 两分钟默认超时把还开着的对话框判死。
_PICKER_SCRIPTS = {
    "files": (
        "with timeout of 3600 seconds\n"
        'set picked to choose file with prompt "选择要导入的素材" '
        "with multiple selections allowed\n"
        'set out to ""\n'
        "repeat with f in picked\n"
        "set out to out & POSIX path of f & linefeed\n"
        "end repeat\n"
        "return out\n"
        "end timeout"
    ),
    "folder": (
        "with timeout of 3600 seconds\n"
        'set picked to choose folder with prompt "选择要导入的素材文件夹（可多选）" '
        "with multiple selections allowed\n"
        'set out to ""\n'
        "repeat with f in picked\n"
        "set out to out & POSIX path of f & linefeed\n"
        "end repeat\n"
        "return out\n"
        "end timeout"
    ),
}


def _run_native_picker(mode: str) -> list[str] | None:
    """跑 osascript 对话框：返回所选绝对路径；用户取消返回 []；环境不可用返回 None。"""

    try:
        completed = subprocess.run(
            ["osascript", "-e", _PICKER_SCRIPTS[mode]],
            capture_output=True,
            text=True,
            timeout=3600,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None
    if completed.returncode != 0:
        # -128 = 用户点了取消；其余（无 GUI 会话等）视为不可用走回退。
        if "-128" in completed.stderr:
            return []
        LOGGER.warning("原生选择对话框失败：%s", completed.stderr.strip())
        return None
    return [line for line in completed.stdout.splitlines() if line.strip()]


def _expand_import_sources(
    sources: list[Path],
    fs_roots: Sequence[Path],
) -> tuple[list[tuple[Path, str | None]], list[str]]:
    """把文件/目录混合的导入请求展开为 (文件, rel_dir) 列表。

    目录递归扫描并保留层级：rel_dir = 所选目录名 + 文件相对子目录（POSIX 分隔），
    素材面板按它分组。隐藏项直接跳过；目录里不支持的扩展名记入 skipped 而不中断
    批量导入。**展开出的每个文件都重新做 realpath + fs_roots 包含校验**——目录里的
    符号链接可能指向允许目录之外，越界项记入 skipped 而不是被静默导入。
    直接给出的单文件 rel_dir=None，格式不支持在此前置 400（导入尚未开始，无半批）。
    """

    plan: list[tuple[Path, str | None]] = []
    skipped: list[str] = []
    seen: set[Path] = set()
    for source in sources:
        if source.is_dir():
            for file_path in sorted(source.rglob("*")):
                if not file_path.is_file():
                    continue
                relative = file_path.relative_to(source)
                if any(part.startswith(".") for part in relative.parts):
                    continue
                if file_path.suffix.lower() not in _MATERIAL_KIND_BY_SUFFIX:
                    skipped.append(relative.as_posix())
                    continue
                try:
                    resolved = canonicalize_allowed_path(str(file_path), fs_roots)
                except PathEscapeError:
                    skipped.append(f"{relative.as_posix()}（越出允许目录）")
                    continue
                if resolved in seen:
                    continue
                seen.add(resolved)
                rel_parent = relative.parent.as_posix()
                rel_dir = source.name if rel_parent == "." else f"{source.name}/{rel_parent}"
                plan.append((resolved, rel_dir))
        else:
            # 顶层条目已在路由里 canonicalize；这里只做后缀前置校验与去重。
            _infer_material_kind(str(source))
            if source in seen:
                continue
            seen.add(source)
            plan.append((source, None))
    return plan, skipped


def _infer_material_kind(name_or_path: str) -> AssetKind:
    suffix = Path(name_or_path).suffix.lower()
    kind = _MATERIAL_KIND_BY_SUFFIX.get(suffix)
    if kind is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "error_code": "unsupported_material_type",
                "message": f"不支持的素材格式：{suffix or '（无扩展名）'}。"
                "支持常见视频/音频/图片/字体格式。",
            },
        )
    return kind


def _url_import_decision(draft_id: str, payload: MaterialImportUrlRequest) -> Decision:
    filename = payload.filename or Path(urlsplit(payload.url).path).name
    kind = _infer_material_kind(filename)
    arguments: dict[str, Any] = {
        "draft_id": draft_id,
        "url": payload.url,
        "filename": payload.filename,
        "kind": kind.value,
        "max_bytes": payload.max_bytes,
        "asset_id": payload.asset_id,
    }
    arguments = {key: value for key, value in arguments.items() if value is not None}
    fingerprint = _fingerprint(arguments)
    decision_id = f"dec_url_import_asset_import_url_{fingerprint[:16]}"
    pending = PendingToolCall(
        tool_name="asset.import_url",
        arguments=arguments,
        idempotency_key=f"asset.import_url:{draft_id}:{fingerprint}:decision:{decision_id}",
        argument_fingerprint=fingerprint,
    )
    return Decision(
        decision_id=decision_id,
        scope_type="draft",
        draft_id=draft_id,
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
    draft_id = pending.arguments.get("draft_id")
    url = pending.arguments.get("url")
    if not isinstance(draft_id, str) or not isinstance(url, str):
        raise HTTPException(status_code=409, detail={"reason": "invalid_url_import_decision"})
    asset_id = pending.arguments.get("asset_id")
    if not isinstance(asset_id, str) or asset_id == "":
        asset_id = "asset_" + hashlib.sha256(f"{draft_id}:{url}".encode()).hexdigest()[:20]
    return JobEnqueued(
        job_id=_job_id("import_url", pending.idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
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


def _require_thumbnail_path(engine: Engine, paths: WorkspacePaths, asset_id: str) -> Path:
    asset = _require_asset(engine, asset_id)
    thumbnail_hash = asset.get("thumbnail_object_hash")
    if not isinstance(thumbnail_hash, str) or thumbnail_hash == "":
        raise HTTPException(status_code=404, detail={"reason": "thumbnail_not_ready"})
    thumbnail_path = paths.object_path(thumbnail_hash)
    if not thumbnail_path.exists():
        raise HTTPException(status_code=404, detail={"reason": "thumbnail_not_found"})
    return thumbnail_path


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
    if payload.draft_id is not None and payload.draft_id != decision.get("draft_id"):
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
    if decision.get("scope_type") != "draft":
        return None
    draft_id = decision.get("draft_id")
    if not isinstance(draft_id, str):
        raise HTTPException(status_code=409, detail={"reason": "invalid_decision_scope"})
    draft = _require_draft(engine, draft_id)
    return int(draft["state_version"])


def _job_observation_draft_id(job: Mapping[str, Any]) -> str | None:
    requested_by_draft_id = job.get("requested_by_draft_id")
    if isinstance(requested_by_draft_id, str):
        return requested_by_draft_id
    draft_id = job.get("draft_id")
    return draft_id if isinstance(draft_id, str) else None


def _observation_job_kind(payload: Mapping[str, Any]) -> str | None:
    """终态事件 payload 里 kind 嵌在 event.payload.kind（见 job_runner `_terminal_event`）。"""
    inner = payload.get("payload")
    if isinstance(inner, Mapping):
        kind = inner.get("kind")
        return kind if isinstance(kind, str) else None
    return None


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
                "draft_id": result.conflict.draft_id,
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
