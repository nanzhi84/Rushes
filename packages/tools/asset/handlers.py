"""Asset import and listing tool handlers（单级草稿模型：直挂当前草稿）。"""

from __future__ import annotations

import hashlib
import uuid
from pathlib import Path
from typing import Any

from sqlalchemy import func, select

from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.events import AssetImported, AssetLinked, JobEnqueued
from contracts.tool_result import ToolError, ToolResult
from media.probe import asset_needs_proxy
from storage import schema
from storage.object_store import ObjectStore
from storage.repositories._json import load_json
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
    asset_id = input_model.asset_id or _new_id("asset")
    object_hash: str | None = None
    object_size: int | None = None
    reference_path: str | None = str(source)
    if input_model.storage_mode is StorageMode.COPY:
        # COPY 时 put_file 已整文件哈希——object_hash 即真 sha256，直接沿用，无需 hash job。
        ref = ObjectStore(_workspace_paths(context)).put_file(source)
        object_hash = ref.object_hash
        object_size = ref.size
        reference_path = None
        digest = ref.object_hash
    else:
        # REFERENCE 不在同步路径整文件哈希：先发 pending 占位，真 sha256 交后台 hash job。
        digest = f"pending:{stat.st_size}:{stat.st_mtime_ns}"
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
    if input_model.storage_mode is StorageMode.REFERENCE:
        # canonical sha256 在后台补算（claim 优先级最低，不与 poster/proxy/index 抢）。
        events.append(_hash_job_event(draft_id=draft_id, asset_id=asset_id))
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
    draft_id = _active_draft_id(context)
    if draft_id is None or context.readonly_connection is None:
        return _failed(
            "asset.list_assets",
            context,
            "missing_draft",
            "active draft and repository access required",
        )
    join = schema.assets.join(
        schema.draft_asset_links,
        schema.draft_asset_links.c.asset_id == schema.assets.c.asset_id,
    )
    filters = [schema.draft_asset_links.c.draft_id == draft_id]
    if input_model.kind is not None:
        filters.append(schema.assets.c.kind == input_model.kind)
    if input_model.rel_dir is not None:
        filters.append(schema.draft_asset_links.c.rel_dir == input_model.rel_dir)
    if input_model.ingest_status is not None:
        filters.append(schema.assets.c.ingest_status == input_model.ingest_status)
    if input_model.only_usable:
        filters.append(schema.assets.c.usable.is_(True))
    columns = (
        select(
            schema.assets.c.asset_id,
            schema.assets.c.filename,
            schema.assets.c.kind,
            schema.assets.c.probe,
            schema.assets.c.thumbnail_object_hash,
            schema.assets.c.ingest_status,
            schema.assets.c.understanding_status,
            schema.assets.c.usable,
            schema.draft_asset_links.c.rel_dir,
        )
        .select_from(join)
        .where(*filters)
        .order_by(schema.assets.c.asset_id)
    )
    connection = context.readonly_connection
    if input_model.has_audio is None:
        # has_audio 是唯一没法下推的过滤（藏在 probe JSON 里）；它为 None 时把 total/after/limit
        # 全下推 SQL，每页只取 limit+1 行、只对本页查 has_summary，不再全量取行 + 解码全部 probe。
        total = int(
            connection.execute(select(func.count()).select_from(join).where(*filters)).scalar_one()
        )
        statement = columns
        if input_model.after is not None:
            statement = statement.where(schema.assets.c.asset_id > input_model.after)
        if input_model.limit is not None:
            statement = statement.limit(input_model.limit + 1)
        rows = connection.execute(statement).all()
        asset_ids = [str(row._mapping["asset_id"]) for row in rows]
        with_summary = _asset_ids_with_summary(context, asset_ids)
        entries = [_asset_manifest_entry(row._mapping, with_summary) for row in rows]
        next_after: str | None = None
        if input_model.limit is not None and len(entries) > input_model.limit:
            entries = entries[: input_model.limit]
            next_after = entries[-1]["asset_id"]
    else:
        # has_audio 非 None：probe 在 JSON 里无法下推，保留内存路径（全量取行后筛）。
        rows = connection.execute(columns).all()
        asset_ids = [str(row._mapping["asset_id"]) for row in rows]
        with_summary = _asset_ids_with_summary(context, asset_ids)
        entries = [_asset_manifest_entry(row._mapping, with_summary) for row in rows]
        entries = [entry for entry in entries if entry["has_audio"] == input_model.has_audio]
        total = len(entries)
        if input_model.after is not None:
            entries = [entry for entry in entries if entry["asset_id"] > input_model.after]
        next_after = None
        if input_model.limit is not None and len(entries) > input_model.limit:
            entries = entries[: input_model.limit]
            next_after = entries[-1]["asset_id"]
    return _succeeded(
        "asset.list_assets",
        context,
        _list_assets_observation(entries, total=total, next_after=next_after),
        data={
            "draft_id": draft_id,
            "assets": entries,
            "total": total,
            "next_after": next_after,
        },
        events=[],
    )


def _asset_manifest_entry(
    mapping: Any,
    with_summary: set[str],
) -> dict[str, Any]:
    asset_id = str(mapping["asset_id"])
    probe = load_json(mapping["probe"]) if isinstance(mapping["probe"], str) else None
    width = probe.get("width") if isinstance(probe, dict) else None
    height = probe.get("height") if isinstance(probe, dict) else None
    duration = probe.get("duration_sec") if isinstance(probe, dict) else None
    fps = probe.get("fps") if isinstance(probe, dict) else None
    has_audio = probe.get("has_audio") if isinstance(probe, dict) else None
    thumbnail = mapping["thumbnail_object_hash"]
    return {
        "asset_id": asset_id,
        "filename": mapping["filename"] or "",
        "kind": str(mapping["kind"]),
        "rel_dir": mapping["rel_dir"],
        "duration_sec": duration if isinstance(duration, int | float) else None,
        "fps": float(fps) if isinstance(fps, int | float) else None,
        "width": width if isinstance(width, int) else None,
        "height": height if isinstance(height, int) else None,
        "orientation": _orientation(width, height),
        "has_audio": has_audio if isinstance(has_audio, bool) else None,
        "usable": bool(mapping["usable"]),
        "ingest_status": str(mapping["ingest_status"]),
        "understanding_status": str(mapping["understanding_status"] or "none"),
        "has_summary": asset_id in with_summary,
        "thumbnail_ready": isinstance(thumbnail, str) and thumbnail != "",
    }


def _orientation(width: Any, height: Any) -> str | None:
    if not isinstance(width, int) or not isinstance(height, int) or width <= 0 or height <= 0:
        return None
    if width > height:
        return "landscape"
    if width < height:
        return "portrait"
    return "square"


def _list_assets_observation(
    entries: list[dict[str, Any]],
    *,
    total: int,
    next_after: str | None,
) -> str:
    kinds = ("video", "audio", "image", "font")
    # 统计口径落在「过滤后总数」这一页之外的全集不便再算，观测按当前返回页给出即时概览。
    counts = {kind: sum(1 for entry in entries if entry["kind"] == kind) for kind in kinds}
    usable = sum(1 for entry in entries if entry["usable"])
    understood = sum(1 for entry in entries if entry["understanding_status"] == "ready")
    head = (
        f"共 {total} 个素材，本页 {len(entries)} 个"
        f"（视频 {counts['video']} / 音频 {counts['audio']} /"
        f" 图片 {counts['image']} / 字体 {counts['font']}）；"
        f"可用 {usable} 个，已理解 {understood} 个。"
    )
    lines = [head]
    for entry in entries[:10]:
        duration = entry["duration_sec"]
        duration_text = f"{duration:.1f}s" if isinstance(duration, int | float) else "时长未知"
        summary_flag = "已理解" if entry["has_summary"] else "未理解"
        usable_flag = "可用" if entry["usable"] else "不可用"
        lines.append(
            f"- {entry['asset_id']} {entry['filename']} | {entry['kind']} | "
            f"{duration_text} | {usable_flag} | {summary_flag}"
        )
    if next_after is not None:
        lines.append(f"还有更多，可用 after={next_after} 继续翻页。")
    return "\n".join(lines)


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


def _hash_job_event(*, draft_id: str, asset_id: str) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:hash"
    return JobEnqueued(
        job_id=_job_id("hash", idempotency_key),
        draft_id=draft_id,
        requested_by_draft_id=draft_id,
        payload={
            "kind": "hash",
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
