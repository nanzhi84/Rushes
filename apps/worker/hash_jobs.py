"""Hash job: 后台补算 REFERENCE 素材的 canonical sha256。

导入的同步路径不再整文件哈希（只发 ``pending:{size}:{mtime}`` 占位），真 sha256 由本 job
在后台分块算出，经 ``AssetHashComputed`` 落库替换占位。文件缺失/读失败抛异常走 job 重试。
"""

from __future__ import annotations

import asyncio

from sqlalchemy import select
from sqlalchemy.engine import Engine

from contracts.events import AssetHashComputed
from contracts.jobs import Job
from storage import schema
from storage.object_store import sha256_file
from storage.workspace_paths import WorkspacePaths

from .job_registry import JobExecutionError, JobExecutionResult, JobHandler
from .media_jobs import _apply_or_raise, _job_asset_id


def build_hash_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    del paths  # hash 只读 reference_path 原文件，不落对象存储，无需工作区路径。

    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        reference_path = _reference_path(engine, asset_id)
        if reference_path is None:
            raise JobExecutionError(
                f"asset has no reference_path to hash: {asset_id}",
                error_code="hash_missing_reference",
                retryable=False,
            )
        # 整文件分块哈希是同步重活，丢线程池执行，别阻塞事件循环。
        digest = await asyncio.to_thread(sha256_file, reference_path)
        _apply_or_raise(
            engine,
            AssetHashComputed(
                draft_id=job.draft_id,
                asset_id=asset_id,
                job_id=job.job_id,
                payload={"hash": digest},
            ),
        )
        return JobExecutionResult({"asset_id": asset_id, "hash": digest})

    return _handler


def _reference_path(engine: Engine, asset_id: str) -> str | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets.c.reference_path).where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        return None
    value = row._mapping["reference_path"]
    return value if isinstance(value, str) and value else None
