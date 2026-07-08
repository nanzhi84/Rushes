"""Poster job: 缩略图 + 时长秒出（剪映式即时反馈）。

poster 是最省的素材加工步，在导入时先于 proxy 认领：视频=probe 时长 + 封面缩略图，
音频=probe 时长，图片=缩略图。产物借现有 merge 事件落库（AssetProbed 写 probe，
AssetIndexReady 写 thumbnail_object_hash），UI 轮询 materials 即刻拿到 thumbnail_ready
与 duration，不必等几十秒的 proxy 转码。

best-effort：失败只降级返回，绝不发 JobFailed——那会被 reducer 标记 asset 不可用；
封面/时长产不出不影响素材可用性，proxy/index 仍会各自推进。
"""

from __future__ import annotations

from pathlib import Path

from sqlalchemy.engine import Engine

from contracts.asset import AssetKind
from contracts.events import AssetIndexReady, AssetProbed, DomainEventBase
from contracts.jobs import Job
from media.probe import probe_media
from media.thumbnails import extract_video_thumbnail, render_image_thumbnail
from storage.object_store import ObjectStore
from storage.workspace_paths import WorkspacePaths

from .job_registry import JobExecutionResult, JobHandler
from .media_jobs import _apply_many_or_raise, _asset_source, _job_asset_id


def build_poster_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        try:
            source_path, kind = _asset_source(engine, paths, asset_id)
            events = _poster_events(job, asset_id, kind, source_path, paths)
        except Exception as exc:  # best-effort：封面/时长产不出只降级，不阻塞素材可用性
            return JobExecutionResult(
                {"asset_id": asset_id, "poster_status": "skipped", "reason": str(exc)}
            )
        if events:
            _apply_many_or_raise(engine, tuple(events))
        return JobExecutionResult({"asset_id": asset_id, "poster_status": "ready"})

    return _handler


def _poster_events(
    job: Job,
    asset_id: str,
    kind: str,
    source_path: Path,
    paths: WorkspacePaths,
) -> list[DomainEventBase]:
    if kind == AssetKind.VIDEO.value:
        probe = probe_media(source_path)
        cover_sec = 1.0 if probe.duration_sec >= 2.0 else max(0.0, probe.duration_sec / 10.0)
        thumbnail = extract_video_thumbnail(source_path, seconds=cover_sec)
        thumbnail_object_hash = ObjectStore(paths).put_bytes(thumbnail).object_hash
        return [
            AssetProbed(
                draft_id=job.draft_id,
                asset_id=asset_id,
                job_id=job.job_id,
                payload={"probe": probe.model_dump(mode="json"), "ingest_status": "probing"},
            ),
            # thumbnail 走 AssetIndexReady（merge_key 为空，永不去重，秒出必达）；
            # 不带 ingest_status，避免把状态提前跳到 indexed。
            AssetIndexReady(
                draft_id=job.draft_id,
                asset_id=asset_id,
                payload={"thumbnail_object_hash": thumbnail_object_hash},
            ),
        ]
    if kind == AssetKind.AUDIO.value:
        probe = probe_media(source_path)
        return [
            AssetProbed(
                draft_id=job.draft_id,
                asset_id=asset_id,
                job_id=job.job_id,
                payload={"probe": probe.model_dump(mode="json"), "ingest_status": "probing"},
            ),
        ]
    if kind == AssetKind.IMAGE.value:
        thumbnail = render_image_thumbnail(source_path)
        thumbnail_object_hash = ObjectStore(paths).put_bytes(thumbnail).object_hash
        return [
            AssetIndexReady(
                draft_id=job.draft_id,
                asset_id=asset_id,
                payload={"thumbnail_object_hash": thumbnail_object_hash},
            ),
        ]
    # 字体等无封面/时长可加工：poster 跳过，元数据交给 index。
    return []
