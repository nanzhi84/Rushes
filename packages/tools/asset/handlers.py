"""Asset import and scope tool handlers."""

from __future__ import annotations

import hashlib
import uuid
from pathlib import Path
from typing import Any

from sqlalchemy import select

from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.case import CaseState
from contracts.events import (
    AssetImported,
    AssetLinked,
    AssetUnlinked,
    CaseAssetScopeChanged,
    JobEnqueued,
)
from contracts.tool_result import ToolError, ToolResult
from storage import schema
from storage.object_store import CHUNK_SIZE, ObjectStore
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths
from tools.context import ToolExecutionContext
from tools.specs import (
    AssetDisableForCaseInput,
    AssetImportLocalFileInput,
    AssetImportUrlInput,
    AssetLinkInput,
    AssetListCaseScopeInput,
    AssetListProjectInput,
    AssetSelectForCaseInput,
    AssetUnlinkInput,
    AssetUploadCompleteInput,
)


def upload_complete(
    input_model: AssetUploadCompleteInput,
    context: ToolExecutionContext,
) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "asset.upload_complete",
            context,
            "missing_project",
            "active project required",
        )
    paths = _workspace_paths(context)
    source = Path(input_model.path).expanduser().resolve(strict=True)
    stat = source.stat()
    ref = ObjectStore(paths).put_file(source)
    asset_id = input_model.asset_id or _new_id("asset")
    filename = input_model.filename or source.name
    events = [
        _asset_imported_event(
            asset_id=asset_id,
            project_id=project_id,
            storage_mode=StorageMode.COPY,
            source=AssetSource.UPLOAD,
            filename=filename,
            kind=input_model.kind,
            digest=ref.object_hash,
            size=stat.st_size,
            mtime=stat.st_mtime_ns,
            object_hash=ref.object_hash,
            object_size=ref.size,
        ),
        AssetLinked(project_id=project_id, asset_id=asset_id),
        _proxy_job_event(project_id=project_id, asset_id=asset_id),
    ]
    return _succeeded(
        "asset.upload_complete",
        context,
        f"uploaded asset {asset_id}",
        data={"project_id": project_id, "asset_id": asset_id, "object_hash": ref.object_hash},
        events=[event.model_dump(mode="json") for event in events],
    )


def import_local_file(
    input_model: AssetImportLocalFileInput,
    context: ToolExecutionContext,
) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "asset.import_local_file",
            context,
            "missing_project",
            "active project required",
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
            project_id=project_id,
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
        AssetLinked(project_id=project_id, asset_id=asset_id, payload=link_payload),
        _proxy_job_event(project_id=project_id, asset_id=asset_id),
    ]
    return _succeeded(
        "asset.import_local_file",
        context,
        f"imported local asset {asset_id}",
        data={"project_id": project_id, "asset_id": asset_id, "hash": digest},
        events=[event.model_dump(mode="json") for event in events],
    )


def import_url(input_model: AssetImportUrlInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed("asset.import_url", context, "missing_project", "active project required")
    asset_id = input_model.asset_id or _new_id("asset")
    event = _import_url_job_event(
        project_id=project_id,
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
        data={"project_id": project_id, "asset_id": asset_id, "job_id": event.job_id},
        events=[event.model_dump(mode="json")],
    )


def link_to_project(input_model: AssetLinkInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "asset.link_to_project",
            context,
            "missing_project",
            "active project required",
        )
    event = AssetLinked(
        project_id=project_id,
        asset_id=input_model.asset_id,
        payload={"enabled": input_model.enabled, "note": input_model.note},
    )
    return _succeeded(
        "asset.link_to_project",
        context,
        f"linked asset {input_model.asset_id}",
        data={"project_id": project_id, "asset_id": input_model.asset_id},
        events=[event.model_dump(mode="json")],
    )


def unlink_from_project(input_model: AssetUnlinkInput, context: ToolExecutionContext) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None:
        return _failed(
            "asset.unlink_from_project",
            context,
            "missing_project",
            "active project required",
        )
    event = AssetUnlinked(project_id=project_id, asset_id=input_model.asset_id)
    return _succeeded(
        "asset.unlink_from_project",
        context,
        f"unlinked asset {input_model.asset_id}",
        data={"project_id": project_id, "asset_id": input_model.asset_id},
        events=[event.model_dump(mode="json")],
    )


def select_for_case(
    input_model: AssetSelectForCaseInput,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = _case_state(context, input_model.case_id)
    if case_state is None:
        return _failed("asset.select_for_case", context, "missing_case", "active case required")
    selected = set(case_state.selected_asset_ids)
    disabled = set(case_state.disabled_asset_ids)
    selected.add(input_model.asset_id)
    disabled.discard(input_model.asset_id)
    event = CaseAssetScopeChanged(
        case_id=case_state.case_id,
        project_id=case_state.project_id,
        payload={
            "selected_asset_ids": sorted(selected),
            "disabled_asset_ids": sorted(disabled),
        },
    )
    return _succeeded(
        "asset.select_for_case",
        context,
        f"selected asset {input_model.asset_id}",
        data={"case_id": case_state.case_id, "asset_id": input_model.asset_id},
        events=[event.model_dump(mode="json")],
    )


def disable_for_case(
    input_model: AssetDisableForCaseInput,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = _case_state(context, input_model.case_id)
    if case_state is None:
        return _failed("asset.disable_for_case", context, "missing_case", "active case required")
    selected = set(case_state.selected_asset_ids)
    disabled = set(case_state.disabled_asset_ids)
    selected.discard(input_model.asset_id)
    disabled.add(input_model.asset_id)
    event = CaseAssetScopeChanged(
        case_id=case_state.case_id,
        project_id=case_state.project_id,
        payload={
            "selected_asset_ids": sorted(selected),
            "disabled_asset_ids": sorted(disabled),
        },
    )
    return _succeeded(
        "asset.disable_for_case",
        context,
        f"disabled asset {input_model.asset_id}",
        data={"case_id": case_state.case_id, "asset_id": input_model.asset_id},
        events=[event.model_dump(mode="json")],
    )


def list_project_assets(
    input_model: AssetListProjectInput,
    context: ToolExecutionContext,
) -> ToolResult:
    project_id = _active_project_id(context, input_model.project_id)
    if project_id is None or context.readonly_connection is None:
        return _failed(
            "asset.list_project_assets",
            context,
            "missing_project",
            "active project and repository access required",
        )
    rows = context.readonly_connection.execute(
        select(schema.assets, schema.project_asset_links.c.enabled)
        .select_from(
            schema.assets.join(
                schema.project_asset_links,
                schema.project_asset_links.c.asset_id == schema.assets.c.asset_id,
            )
        )
        .where(schema.project_asset_links.c.project_id == project_id)
        .order_by(schema.assets.c.asset_id)
    ).all()
    assets = [_asset_row_payload(dict(row._mapping)) for row in rows]
    if not input_model.include_disabled:
        assets = [asset for asset in assets if asset["enabled"]]
    return _succeeded(
        "asset.list_project_assets",
        context,
        "loaded project assets",
        data={"project_id": project_id, "assets": assets},
        events=[],
    )


def list_case_scope(
    input_model: AssetListCaseScopeInput,
    context: ToolExecutionContext,
) -> ToolResult:
    case_state = _case_state(context, input_model.case_id)
    if case_state is None:
        return _failed("asset.list_case_scope", context, "missing_case", "active case required")
    return _succeeded(
        "asset.list_case_scope",
        context,
        "loaded case asset scope",
        data={
            "case_id": case_state.case_id,
            "selected_asset_ids": list(case_state.selected_asset_ids),
            "disabled_asset_ids": list(case_state.disabled_asset_ids),
        },
        events=[],
    )


def _asset_imported_event(
    *,
    asset_id: str,
    project_id: str,
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
        project_id=project_id,
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


def _proxy_job_event(*, project_id: str, asset_id: str) -> JobEnqueued:
    idempotency_key = f"asset:{asset_id}:probe_proxy"
    return JobEnqueued(
        job_id=_job_id("proxy", idempotency_key),
        project_id=project_id,
        payload={
            "kind": "proxy",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _import_url_job_event(
    *,
    project_id: str,
    asset_id: str,
    url: str,
    filename: str | None,
    kind: AssetKind,
    max_bytes: int | None,
) -> JobEnqueued:
    idempotency_key = f"asset:{project_id}:url:{hashlib.sha256(url.encode()).hexdigest()}"
    return JobEnqueued(
        job_id=_job_id("import_url", idempotency_key),
        project_id=project_id,
        payload={
            "kind": "import_url",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {
                "asset_id": asset_id,
                "project_id": project_id,
                "url": url,
                "filename": filename,
                "kind": kind.value,
                "max_bytes": max_bytes,
            },
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _asset_row_payload(values: dict[str, Any]) -> dict[str, Any]:
    payload = dict(values)
    for key in ("probe", "failure"):
        raw = payload.get(key)
        if isinstance(raw, str):
            payload[key] = load_json(raw)
    return payload


def _active_project_id(context: ToolExecutionContext, requested: str | None) -> str | None:
    active = None
    if context.project_state is not None:
        active = context.project_state.project_id
    elif context.case_state is not None:
        active = context.case_state.project_id
    if requested is None:
        return active
    if active is not None and requested != active:
        return None
    return requested


def _case_state(context: ToolExecutionContext, requested: str | None) -> CaseState | None:
    if context.case_state is not None:
        if requested is None or requested == context.case_state.case_id:
            return context.case_state
        return None
    if requested is None or context.readonly_connection is None:
        return None
    row = context.readonly_connection.execute(
        select(schema.cases).where(schema.cases.c.case_id == requested)
    ).first()
    if row is None:
        return None
    values = dict(row._mapping)
    for key in (
        "running_jobs",
        "last_error",
        "brief",
        "content_plan",
        "audio_plan",
        "cut_plan",
        "postprocess_plan",
        "selected_asset_ids",
        "disabled_asset_ids",
        "scratch_memory",
    ):
        raw = values.get(key)
        if isinstance(raw, str):
            values[key] = load_json(raw)
    return CaseState.model_validate(values)


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
