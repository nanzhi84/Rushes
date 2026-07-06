"""Media job handlers for proxy generation and URL import."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import httpx
from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.events import (
    AssetImported,
    AssetLinked,
    AssetProbed,
    DomainEventBase,
    JobEnqueued,
    ProxyGenerated,
)
from contracts.jobs import Job
from media.probe import MediaProbeError, probe_media
from media.proxy import MediaProxyError, generate_proxy
from media.url_import import UrlImportError, download_url_to_object
from storage import schema
from storage.workspace_paths import WorkspacePaths, resolve_asset_path

from .job_registry import JobExecutionError, JobExecutionResult, JobHandler


def build_proxy_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        source_path, kind = _asset_source(engine, paths, asset_id)
        if kind == AssetKind.FONT.value:
            # 字体没有可转码的媒体代理：跳过 probe/proxy，直接进入本地索引读取元数据。
            _enqueue_index(engine, job, asset_id)
            return JobExecutionResult({"asset_id": asset_id, "index_enqueued": True})
        try:
            probe = probe_media(source_path)
            _apply_or_raise(
                engine,
                AssetProbed(
                    project_id=job.project_id,
                    asset_id=asset_id,
                    job_id=job.job_id,
                    payload={
                        "probe": probe.model_dump(mode="json"),
                        "ingest_status": "probing",
                    },
                ),
            )
            proxy = generate_proxy(
                source_path,
                paths=paths,
                audio_only=_is_audio_proxy_kind(kind),
            )
            _apply_or_raise(
                engine,
                ProxyGenerated(
                    project_id=job.project_id,
                    asset_id=asset_id,
                    job_id=job.job_id,
                    payload={
                        "proxy_object_hash": proxy.object_hash,
                        "proxy_object_size": proxy.size,
                        "ingest_status": "proxying",
                    },
                ),
            )
            _enqueue_index(engine, job, asset_id)
            return JobExecutionResult(
                {
                    "asset_id": asset_id,
                    "probe": probe.model_dump(mode="json"),
                    "proxy_object_hash": proxy.object_hash,
                }
            )
        except (FileNotFoundError, MediaProbeError, MediaProxyError) as exc:
            raise JobExecutionError(
                str(exc),
                error_code="media_proxy_failed",
                retryable=False,
                details={"asset_id": asset_id},
            ) from exc

    return _handler


def build_import_url_handler(
    engine: Engine,
    paths: WorkspacePaths,
    *,
    http_transport: httpx.AsyncBaseTransport | None = None,
) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        payload = job.payload_json
        asset_id = _payload_str(payload, "asset_id") or _job_asset_id(job)
        project_id = _payload_str(payload, "project_id") or job.project_id
        url = _payload_str(payload, "url")
        if project_id is None or url is None:
            raise JobExecutionError(
                "import_url job requires project_id and url",
                error_code="invalid_import_url_job",
                retryable=False,
            )
        try:
            result = await download_url_to_object(
                url,
                paths=paths,
                filename=_payload_str(payload, "filename"),
                max_bytes=_payload_int(payload, "max_bytes"),
                transport=http_transport,
            )
        except UrlImportError as exc:
            raise JobExecutionError(
                str(exc),
                error_code="url_import_failed",
                retryable=exc.retryable,
                details={"url": url},
            ) from exc
        kind = _payload_str(payload, "kind") or AssetKind.VIDEO.value
        object_ref = result.object_ref
        stat_mtime = Path(paths.object_path(object_ref.object_hash)).stat().st_mtime_ns
        imported = AssetImported(
            project_id=project_id,
            asset_id=asset_id,
            job_id=job.job_id,
            payload={
                "storage_mode": StorageMode.COPY.value,
                "object_hash": object_ref.object_hash,
                "object_size": object_ref.size,
                "reference_path": None,
                "kind": kind,
                "source": AssetSource.URL.value,
                "filename": result.filename,
                "hash": object_ref.object_hash,
                "mtime": stat_mtime,
                "size": object_ref.size,
                "probe": None,
                "proxy_object_hash": None,
                "ingest_status": "imported",
                "annotation_status": "pending",
                "annotation_pass": "none",
                "index_status": "none",
                "usable": True,
                "failure": None,
                "source_url": url,
                "content_type": result.content_type,
            },
        )
        proxy_job = _proxy_job_event(project_id=project_id, asset_id=asset_id)
        _apply_many_or_raise(
            engine,
            (
                imported,
                AssetLinked(project_id=project_id, asset_id=asset_id),
                proxy_job,
            ),
        )
        return JobExecutionResult(
            {
                "asset_id": asset_id,
                "object_hash": object_ref.object_hash,
                "proxy_job_id": proxy_job.job_id,
            }
        )

    return _handler


def workspace_paths_from_engine(engine: Engine) -> WorkspacePaths:
    database = engine.url.database
    if database is None:
        raise ValueError("workspace engine must have a filesystem database path")
    return WorkspacePaths.from_root(Path(database).parent).initialize()


def _job_asset_id(job: Job) -> str:
    if job.asset_id is not None:
        return job.asset_id
    asset_id = job.payload_json.get("asset_id")
    if isinstance(asset_id, str):
        return asset_id
    raise JobExecutionError(
        "job requires asset_id",
        error_code="invalid_asset_job",
        retryable=False,
    )


def _asset_source(engine: Engine, paths: WorkspacePaths, asset_id: str) -> tuple[Path, str]:
    with engine.connect() as connection:
        source_path = resolve_asset_path(asset_id, connection=connection, paths=paths)
        row = connection.execute(
            select(schema.assets.c.kind).where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        raise FileNotFoundError(f"asset not found: {asset_id}")
    return source_path, str(row._mapping["kind"])


def _is_audio_proxy_kind(kind: str) -> bool:
    return kind == AssetKind.AUDIO.value


def _apply_or_raise(engine: Engine, event: DomainEventBase) -> None:
    _apply_many_or_raise(engine, (event,))


def _apply_many_or_raise(engine: Engine, events: tuple[DomainEventBase, ...]) -> None:
    result = apply(events, engine=engine, base_version=None, actor="job")
    if result.status != "applied":
        raise JobExecutionError(
            f"reducer rejected media job events: {result.status}",
            error_code="media_job_reducer_rejected",
            retryable=True,
        )


def _proxy_job_event(*, project_id: str, asset_id: str) -> JobEnqueued:
    import hashlib

    idempotency_key = f"asset:{asset_id}:probe_proxy"
    digest = hashlib.sha256(f"proxy:{idempotency_key}".encode()).hexdigest()
    return JobEnqueued(
        job_id=f"job_{digest[:20]}",
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


def _enqueue_index(engine: Engine, job: Job, asset_id: str) -> None:
    _apply_or_raise(engine, _index_job_event(project_id=job.project_id, asset_id=asset_id))


def _index_job_event(*, project_id: str | None, asset_id: str) -> JobEnqueued:
    import hashlib

    idempotency_key = f"asset:{asset_id}:index"
    digest = hashlib.sha256(f"index:{idempotency_key}".encode()).hexdigest()
    return JobEnqueued(
        job_id=f"job_{digest[:20]}",
        project_id=project_id,
        payload={
            "kind": "index",
            "asset_id": asset_id,
            "idempotency_key": idempotency_key,
            "job_payload": {"asset_id": asset_id},
            "attempts": 0,
            "max_retries": 2,
        },
    )


def _payload_str(payload: dict[str, Any], key: str) -> str | None:
    value = payload.get(key)
    return value if isinstance(value, str) and value else None


def _payload_int(payload: dict[str, Any], key: str) -> int | None:
    value = payload.get(key)
    if value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None
