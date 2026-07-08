"""Asset import and listing tool handlers（单级草稿模型：直挂当前草稿）。"""

from __future__ import annotations

import hashlib
import uuid
from pathlib import Path
from typing import Any

from sqlalchemy import select

from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.events import AssetImported, AssetLinked, JobEnqueued
from contracts.tool_result import ToolError, ToolResult
from media.probe import asset_needs_proxy
from storage import schema
from storage.object_store import CHUNK_SIZE, ObjectStore
from storage.workspace_paths import WorkspacePaths
from tools.context import ToolExecutionContext
from tools.specs import (
    AssetImportLocalFileInput,
    AssetImportUrlInput,
    AssetListAssetsInput,
)


def import_local_file(
    input_model: AssetImportLocalFileInput,
    context: ToolExecutionContext,
) -> ToolResult:
    draft_id = _active_draft_id(context)
    if draft_id is None:
        return _failed(
            "asset.import_local_file",
            context,
            "missing_draft",
            "active draft required",
        )
    source = Path(input_model.path).expanduser().resolve(strict=True)
    stat = source.stat()
    digest = _sha256(source)
    asset_id = input_model.asset_id or _new_id("asset")
    object_hash: str | None = None
    object_size: int | None = None
    reference_path: str | None = str(source)
    if input_model.storage_mode is StorageMode.COPY:
        ref = ObjectStore(_workspace_paths(context)).put_file(source)
        object_hash = ref.object_hash
        object_size = ref.size
        reference_path = None
    link_payload: dict[str, Any] = {}
    if input_model.rel_dir:
        link_payload["rel_dir"] = input_model.rel_dir
    events = [
        _asset_imported_event(
            asset_id=asset_id,
            draft_id=draft_id,
            storage_mode=input_model.storage_mode,
            source=AssetSource.LOCAL_PATH,
            filename=source.name,
            kind=input_model.kind,
            digest=digest,
            size=stat.st_size,
            mtime=stat.st_mtime_ns,
            object_hash=object_hash,
            object_size=object_size,
            reference_path=reference_path,
        ),
        AssetLinked(draft_id=draft_id, asset_id=asset_id, payload=link_payload),
        # poster 先入队（claim 优先级高于 proxy/index）：缩略图/时长秒出。
        _poster_job_event(draft_id=draft_id, asset_id=asset_id),
    ]
    # 代理只为「浏览器播不动的格式」兜底：h264/hevc 视频、常见音频、图片都直读即播，不入 proxy 队，
    # 直接入队 index（index 原本挂在 proxy handler 末尾，跳 proxy 就得在此补上索引这一步）。
    if asset_needs_proxy(input_model.kind, source):
        events.append(_proxy_job_event(draft_id=draft_id, asset_id=asset_id))
    else:
        events.append(_index_job_event(draft_id=draft_id, asset_id=asset_id))
    return _succeeded(
        "asset.import_local_file",
        context,
        f"imported local asset {asset_id}",
        data={"draft_id": draft_id, "asset_id": asset_id, "hash": digest},
        events=[event.model_dump(mode="json") for event in events],
    )


def import_url(input_model: AssetImportUrlInput, context: ToolExecutionContext) -> ToolResult:
    draft_id = _active_draft_id(context)
    if draft_id is None:
        return _failed("asset.import_url", context, "missing_draft", "active draft required")
    asset_id = input_model.asset_id or _new_id("asset")
    event = _import_url_job_event(
        draft_id=draft_id,
        asset_id=asset_id,
        url=str(input_model.url),
        filename=input_model.filename,
        kind=input_model.kind,
        max_bytes=input_model.max_bytes,
    )
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name="asset.import_url",
        status="running",
        observation=f"url import queued: {event.job_id}",
        data={"draft_id": draft_id, "asset_id": asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


def list_assets(input_model: AssetListAssetsInput, context: ToolExecutionContext) -> ToolResult:
    del input_model
    draft_id = _active_draft_id(context)
    if draft_id is None or context.readonly_connection is None:
        return _failed(
            "asset.list_assets",
            context,
            "missing_draft",
            "active draft and repository access required",
        )
    rows = context.readonly_connection.execute(
        select(
            schema.assets.c.asset_id,
            schema.assets.c.kind,
            schema.assets.c.usable,
            schema.draft_asset_links.c.rel_dir,
        )
        .select_from(
            schema.assets.join(
                schema.draft_asset_links,
                schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.draft_asset_links.c.draft_id == draft_id)
        .order_by(schema.assets.c.asset_id)
    ).all()
    asset_ids = [str(row._mapping["asset_id"]) for row in rows]
    with_summary = _asset_ids_with_summary(context, asset_ids)
    assets = [
        {
            "asset_id": str(row._mapping["asset_id"]),
            "kind": str(row._mapping["kind"]),
            "rel_dir": row._mapping["rel_dir"],
            "usable": bool(row._mapping["usable"]),
            "has_summary": str(row._mapping["asset_id"]) in with_summary,
        }
        for row in rows
    ]
    return _succeeded(
        "asset.list_assets",
        context,
        f"当前草稿共链接 {len(assets)} 个素材。",
        data={"draft_id": draft_id, "assets": assets},
        events=[],
    )


def _asset_ids_with_summary(
    context: ToolExecutionContext,
    asset_ids: list[str],
) -> set[str]:
    if not asset_ids or context.readonly_connection is None:
        return set()
    rows = context.readonly_connection.execute(
        select(schema.material_summaries.c.asset_id)
        .where(schema.material_summaries.c.asset_id.in_(asset_ids))
        .where(schema.material_summaries.c.status == "ready")
    ).all()
    return {str(row._mapping["asset_id"]) for row in rows}


def _asset_imported_event(
    *,
    asset_id: str,
    draft_id: str,
    storage_mode: StorageMode,
    source: AssetSource,
    filename: str,
    kind: AssetKind,
    digest: str,
    size: int,
    mtime: int,
    object_hash: str | None = None,
    object_size: int | None = None,
    reference_path: str | None = None,
) -> AssetImported:
    return AssetImported(
        draft_id=draft_id,
        asset_id=asset_id,
        job_id=f"import_{asset_id}",
        payload={
            "storage_mode": storage_mode.value,
            "object_hash": object_hash,
            "object_size": object_size,
            "reference_path": reference_path,
            "kind": kind.value,
            "source": source.value,
            "filename": filename,
            "hash": digest,
            "mtime": mtime,
            "size": size,
            "probe": None,
            "proxy_object_hash": None,
            "ingest_status": "imported",
            "usable": True,
            "failure": None,
        },
    )


def _poster_job_event(*, draft_id: str, asset_id: str) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:poster"
    return JobEnqueued(
        job_id=_job_id("poster", idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
        payload={
            "kind": "poster",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


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


def _import_url_job_event(
    *,
    draft_id: str,
    asset_id: str,
    url: str,
    filename: str | None,
    kind: AssetKind,
    max_bytes: int | None,
) -> JobEnqueued:
    idempotency_key = f"asset:{draft_id}:url:{hashlib.sha256(url.encode()).hexdigest()}"
    return JobEnqueued(
        job_id=_job_id("import_url", idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
        payload={
            "kind": "import_url",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "asset_id": asset_id,
                "draft_id": draft_id,
                "url": url,
                "filename": filename,
                "kind": kind.value,
                "max_bytes": max_bytes,
            },
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _active_draft_id(context: ToolExecutionContext) -> str | None:
    if context.draft_state is not None:
        return context.draft_state.draft_id
    return None


def _workspace_paths(context: ToolExecutionContext) -> WorkspacePaths:
    raw_paths = context.metadata.get("workspace_paths")
    if isinstance(raw_paths, WorkspacePaths):
        return raw_paths.initialize()
    raw_root = context.metadata.get("workspace_path")
    if isinstance(raw_root, str | Path):
        return WorkspacePaths.from_root(raw_root).initialize()
    raise ValueError("asset tool requires workspace_paths metadata")


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(CHUNK_SIZE), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _job_id(kind: str, idempotency_key: str) -> str:
    digest = hashlib.sha256(f"{kind}:{idempotency_key}".encode()).hexdigest()
    return f"job_{digest[:20]}"


def _new_id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


def _succeeded(
    tool_name: str,
    context: ToolExecutionContext,
    observation: str,
    *,
    data: dict[str, Any],
    events: list[dict[str, Any]],
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="succeeded",
        observation=observation,
        data=data,
        events=events,
    )


def _failed(
    tool_name: str,
    context: ToolExecutionContext,
    error_code: str,
    message: str,
) -> ToolResult:
    return ToolResult(
        tool_call_id=context.tool_call_id,
        tool_name=tool_name,
        status="failed",
        observation=message,
        error=ToolError(
            error_code=error_code,
            message=message,
            retryable=False,
            details={},
        ),
    )
