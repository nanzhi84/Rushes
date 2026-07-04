"""Worker CLI entrypoint."""

from __future__ import annotations

import argparse
import asyncio
from pathlib import Path

from storage.db import create_workspace_engine

from .job_registry import build_default_job_registry
from .job_runner import JobRunner


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run the Rushes SQLite job worker.")
    parser.add_argument("workspace", type=Path, help="Workspace root or rushes.db path")
    parser.add_argument("--worker-id", default=None)
    parser.add_argument("--poll-interval", type=float, default=0.25)
    parser.add_argument("--heartbeat-interval", type=float, default=5.0)
    parser.add_argument("--heartbeat-timeout", type=float, default=60.0)
    return parser.parse_args()


async def async_main() -> None:
    args = parse_args()
    engine = create_workspace_engine(args.workspace)
    runner = JobRunner(
        engine=engine,
        registry=build_default_job_registry(),
        worker_id=args.worker_id,
        poll_interval_seconds=args.poll_interval,
        heartbeat_interval_seconds=args.heartbeat_interval,
        heartbeat_timeout_seconds=args.heartbeat_timeout,
    )
    await runner.run_forever()


def main() -> None:
    asyncio.run(async_main())


if __name__ == "__main__":
    main()
