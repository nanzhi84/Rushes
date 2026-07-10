"""Hash job: 后台补算 REFERENCE 素材的 canonical sha256（best-effort）。

导入的同步路径不再整文件哈希（只发 ``pending:{size}:{mtime}`` 占位），真 sha256 由本 job
在后台分块算出，经 ``AssetHashComputed`` 落库替换占位。哈希与 mtime/size **同刻快照**一起落库，
三列一致，失效检测的 stat 快路径才不会拿旧元数据比新哈希而永久误判。

best-effort：任何异常（reference_path 缺失、文件读失败）只记降级、job 仍成功，绝不发 JobFailed
——那会被 reducer 阴杀成 usable=False 且无恢复路径；哈希只是元数据补算，产不出不影响素材可用。
hash 列保持 pending 占位，失效检测在挂起期不判定（见 media.invalidation）。
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from pathlib import Path

from sqlalchemy import select
from sqlalchemy.engine import Engine

from contracts.events import AssetHashComputed
from contracts.jobs import Job
from storage import schema
from storage.object_store import sha256_file
from storage.workspace_paths import WorkspacePaths

from .job_registry import JobExecutionResult, JobHandler
from .media_jobs import _apply_or_raise, _job_asset_id

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class _HashSnapshot:
    hash: str
    mtime: int
    size: int


def build_hash_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    del paths  # hash 只读 reference_path 原文件，不落对象存储，无需工作区路径。

    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        reference_path = _reference_path(engine, asset_id)
        try:
            if reference_path is None:
                raise FileNotFoundError(f"asset has no reference_path to hash: {asset_id}")
            # stat + 整文件分块哈希是同刻快照，丢同一个线程池调用里做，别阻塞事件循环。
            snapshot = await asyncio.to_thread(_hash_with_stat, reference_path)
        except Exception as exc:  # best-effort：元数据补算失败只降级，绝不阴杀素材
            logger.warning(
                "hash job 降级（不发 AssetHashComputed）：asset_id=%s err=%s", asset_id, exc
            )
            return JobExecutionResult(
                {"asset_id": asset_id, "hash_status": "skipped", "reason": str(exc)}
            )
        _apply_or_raise(
            engine,
            AssetHashComputed(
                draft_id=job.draft_id,
                asset_id=asset_id,
                job_id=job.job_id,
                payload={
                    "hash": snapshot.hash,
                    "mtime": snapshot.mtime,
                    "size": snapshot.size,
                },
            ),
        )
        return JobExecutionResult({"asset_id": asset_id, "hash": snapshot.hash})

    return _handler


def _hash_with_stat(reference_path: str) -> _HashSnapshot:
    path = Path(reference_path).expanduser()
    # 先 stat 再算哈希：万一文件在两步间又变了，落的是旧 mtime/size + 新 hash，下次 stat 必不等、
    # 走慢路径重算哈希发现不符而正确判失效——比反过来（新元数据 + 旧 hash 骗过快路径）安全。
    stat = path.stat()
    return _HashSnapshot(hash=sha256_file(path), mtime=stat.st_mtime_ns, size=stat.st_size)


def _reference_path(engine: Engine, asset_id: str) -> str | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets.c.reference_path).where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        return None
    value = row._mapping["reference_path"]
    return value if isinstance(value, str) and value else None
