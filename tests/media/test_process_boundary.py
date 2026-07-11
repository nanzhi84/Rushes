from __future__ import annotations

import asyncio
import os
import sys
import threading
from pathlib import Path

import pytest

from media.process import (
    MediaCommandCancelled,
    communicate_media_command,
    run_media_command,
    stream_media_command,
)


def test_media_package_has_no_bare_subprocess_calls_outside_process_boundary() -> None:
    root = Path(__file__).parents[2]
    search_roots = [root / "packages" / "media", root / "packages" / "tools", root / "apps"]
    violations: list[str] = []
    for search_root in search_roots:
        for path in search_root.rglob("*.py"):
            if path.name == "process.py":
                continue
            source = path.read_text(encoding="utf-8")
            if ("ffmpeg" in source or "ffprobe" in source) and (
                "subprocess." in source or "create_subprocess_exec" in source
            ):
                violations.append(str(path.relative_to(root)))

    assert violations == []


async def test_communicate_media_command_kills_process_when_task_is_cancelled(
    tmp_path: Path,
) -> None:
    pid_file = tmp_path / "communicate.pid"
    task = asyncio.create_task(communicate_media_command(_sleep_command(pid_file)))
    pid = await _wait_pid(pid_file)

    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    await _assert_process_stopped(pid)


async def test_stream_media_command_kills_process_when_task_is_cancelled(tmp_path: Path) -> None:
    pid_file = tmp_path / "stream.pid"
    task = asyncio.create_task(stream_media_command(_sleep_command(pid_file)))
    pid = await _wait_pid(pid_file)

    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task

    await _assert_process_stopped(pid)


async def test_sync_media_command_observes_cancel_event_and_reaps_process(
    tmp_path: Path,
) -> None:
    pid_file = tmp_path / "sync.pid"
    cancel_event = threading.Event()
    task = asyncio.create_task(
        asyncio.to_thread(
            run_media_command,
            _sleep_command(pid_file),
            cancel_event=cancel_event,
        )
    )
    pid = await _wait_pid(pid_file)

    cancel_event.set()
    with pytest.raises(MediaCommandCancelled):
        await asyncio.wait_for(task, timeout=2)

    await _assert_process_stopped(pid)


def _sleep_command(pid_file: Path) -> list[str]:
    script = f"import os,time; open({str(pid_file)!r}, 'w').write(str(os.getpid())); time.sleep(60)"
    return [sys.executable, "-c", script]


async def _wait_pid(path: Path) -> int:
    for _ in range(100):
        if path.is_file():
            return int(path.read_text(encoding="utf-8"))
        await asyncio.sleep(0.01)
    raise AssertionError(f"process did not write pid file: {path}")


async def _assert_process_stopped(pid: int) -> None:
    for _ in range(100):
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return
        await asyncio.sleep(0.01)
    raise AssertionError(f"process {pid} survived cancellation")
