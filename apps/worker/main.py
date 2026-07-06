"""Worker CLI entrypoint."""

from __future__ import annotations

import argparse
import asyncio
import os
from pathlib import Path

from storage import schema
from storage.data_migrations import apply_data_migrations
from storage.db import create_workspace_engine

from .job_registry import build_default_job_registry
from .job_runner import JobRunner

DEFAULT_CONCURRENCY = 2


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run the Rushes SQLite job worker.")
    parser.add_argument("workspace", type=Path, help="Workspace root or rushes.db path")
    parser.add_argument("--worker-id", default=None)
    parser.add_argument("--poll-interval", type=float, default=0.25)
    parser.add_argument("--heartbeat-interval", type=float, default=5.0)
    parser.add_argument("--heartbeat-timeout", type=float, default=60.0)
    parser.add_argument(
        "--concurrency",
        type=int,
        default=None,
        help="Number of JobRunner tasks (defaults to RUSHES_WORKER_CONCURRENCY or 2)",
    )
    return parser.parse_args()


def resolve_concurrency(cli_value: int | None) -> int:
    if cli_value is not None:
        return max(1, cli_value)
    env_value = os.environ.get("RUSHES_WORKER_CONCURRENCY")
    if env_value:
        try:
            return max(1, int(env_value))
        except ValueError:
            return DEFAULT_CONCURRENCY
    return DEFAULT_CONCURRENCY


async def async_main() -> None:
    args = parse_args()
    engine = create_workspace_engine(args.workspace)
    # worker 可能先于 API 启动到全新 workspace：先建表再跑幂等数据迁移，
    # create_all(checkfirst=True) 与 apply_data_migrations 均可重复执行。
    with engine.begin() as connection:
        schema.create_all(connection)
        apply_data_migrations(connection)
    # 多个 JobRunner 并发：claim 用 begin_immediate + worker_id 已是多 worker 安全，
    # 每个 runner 拿到独立 worker_id 才能各自续心跳、各自认领任务。
    concurrency = resolve_concurrency(args.concurrency)
    registry = build_default_job_registry(engine=engine)
    runners = [
        JobRunner(
            engine=engine,
            registry=registry,
            worker_id=_worker_id(args.worker_id, index, concurrency),
            poll_interval_seconds=args.poll_interval,
            heartbeat_interval_seconds=args.heartbeat_interval,
            heartbeat_timeout_seconds=args.heartbeat_timeout,
        )
        for index in range(concurrency)
    ]
    await asyncio.gather(*(runner.run_forever() for runner in runners))


def _worker_id(base: str | None, index: int, concurrency: int) -> str | None:
    if base is None:
        return None
    return base if concurrency == 1 else f"{base}-{index}"


def main() -> None:
    asyncio.run(async_main())


if __name__ == "__main__":
    main()
