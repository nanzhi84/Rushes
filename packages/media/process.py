"""Single timeout-enforced process boundary for ffmpeg and ffprobe commands."""

from __future__ import annotations

import asyncio
import contextlib
import inspect
import os
import signal
import subprocess
import threading
import time
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from typing import Any, Literal, cast, overload

DEFAULT_MEDIA_TIMEOUT_S = 30 * 60.0
_DECODE_SEMAPHORE = threading.Semaphore(1)
_ASYNC_DECODE_SEMAPHORE = asyncio.Semaphore(1)


@overload
def run_media_command(
    command: Sequence[str],
    *,
    timeout: float = DEFAULT_MEDIA_TIMEOUT_S,
    text: Literal[True] = True,
    input_data: str | None = None,
    decode_intensive: bool = False,
    cancel_event: threading.Event | None = None,
) -> subprocess.CompletedProcess[str]: ...


@overload
def run_media_command(
    command: Sequence[str],
    *,
    timeout: float = DEFAULT_MEDIA_TIMEOUT_S,
    text: Literal[False],
    input_data: bytes | None = None,
    decode_intensive: bool = False,
    cancel_event: threading.Event | None = None,
) -> subprocess.CompletedProcess[bytes]: ...


def run_media_command(
    command: Sequence[str],
    *,
    timeout: float = DEFAULT_MEDIA_TIMEOUT_S,
    text: bool = True,
    input_data: str | bytes | None = None,
    decode_intensive: bool = False,
    cancel_event: threading.Event | None = None,
) -> subprocess.CompletedProcess[str] | subprocess.CompletedProcess[bytes]:
    """Run one media command with captured output and a mandatory finite timeout."""

    if timeout <= 0:
        raise ValueError("media command timeout must be positive")
    semaphore = _DECODE_SEMAPHORE if decode_intensive else _NullSemaphore()
    with semaphore:
        if cancel_event is not None:
            return _run_media_command_cancellable(
                command,
                timeout=timeout,
                text=text,
                input_data=input_data,
                cancel_event=cancel_event,
            )
        try:
            result = subprocess.run(
                list(command),
                capture_output=True,
                check=False,
                text=text,
                input=input_data,
                timeout=timeout,
            )
        except subprocess.TimeoutExpired as exc:
            raise TimeoutError(f"media command timed out after {timeout:g}s") from exc
    return cast(subprocess.CompletedProcess[str] | subprocess.CompletedProcess[bytes], result)


@dataclass(frozen=True, slots=True)
class AsyncMediaResult:
    returncode: int
    stdout: bytes
    stderr: bytes


class MediaCommandCancelled(RuntimeError):
    """Raised when a cooperative synchronous media command is cancelled."""


async def communicate_media_command(
    command: Sequence[str],
    *,
    timeout: float = DEFAULT_MEDIA_TIMEOUT_S,
    input_data: bytes | None = None,
    decode_intensive: bool = False,
) -> AsyncMediaResult:
    """Run an async media command to completion while enforcing timeout and cleanup."""

    async def _run() -> AsyncMediaResult:
        process = await asyncio.create_subprocess_exec(
            *command,
            stdin=asyncio.subprocess.PIPE if input_data is not None else None,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            start_new_session=os.name == "posix",
        )
        try:
            stdout, stderr = await asyncio.wait_for(
                process.communicate(input_data),
                timeout=timeout,
            )
        except BaseException:
            await _terminate_async_process(process)
            raise
        return AsyncMediaResult(process.returncode or 0, stdout, stderr)

    if decode_intensive:
        async with _ASYNC_DECODE_SEMAPHORE:
            return await _run()
    return await _run()


async def stream_media_command(
    command: Sequence[str],
    *,
    on_stdout_line: Callable[[bytes], Awaitable[None] | None] | None = None,
    timeout: float = DEFAULT_MEDIA_TIMEOUT_S,
) -> AsyncMediaResult:
    """Run ffmpeg while streaming stdout progress lines and still bounding wall time."""

    process = await asyncio.create_subprocess_exec(
        *command,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        start_new_session=os.name == "posix",
    )
    assert process.stdout is not None
    assert process.stderr is not None
    stdout_reader = process.stdout
    stderr_reader = process.stderr

    async def _consume() -> AsyncMediaResult:
        stderr_task = asyncio.create_task(stderr_reader.read())
        try:
            stdout_chunks: list[bytes] = []
            while True:
                line = await stdout_reader.readline()
                if not line:
                    break
                stdout_chunks.append(line)
                if on_stdout_line is not None:
                    outcome = on_stdout_line(line)
                    if inspect.isawaitable(outcome):
                        await outcome
            returncode = await process.wait()
            return AsyncMediaResult(returncode, b"".join(stdout_chunks), await stderr_task)
        finally:
            if not stderr_task.done():
                stderr_task.cancel()
            await asyncio.gather(stderr_task, return_exceptions=True)

    try:
        return await asyncio.wait_for(_consume(), timeout=timeout)
    except BaseException:
        await _terminate_async_process(process)
        raise


def _run_media_command_cancellable(
    command: Sequence[str],
    *,
    timeout: float,
    text: bool,
    input_data: str | bytes | None,
    cancel_event: threading.Event,
) -> subprocess.CompletedProcess[str] | subprocess.CompletedProcess[bytes]:
    """Use short communicate polls so a worker thread can cooperatively stop ffmpeg."""

    if cancel_event.is_set():
        raise MediaCommandCancelled("media command cancelled before launch")
    process = subprocess.Popen(
        list(command),
        stdin=subprocess.PIPE if input_data is not None else None,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=text,
        start_new_session=os.name == "posix",
    )
    deadline = time.monotonic() + timeout
    pending_input = input_data
    try:
        while True:
            if cancel_event.is_set():
                raise MediaCommandCancelled("media command cancelled")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"media command timed out after {timeout:g}s")
            try:
                stdout, stderr = process.communicate(
                    pending_input,
                    timeout=min(0.1, remaining),
                )
            except subprocess.TimeoutExpired:
                pending_input = None
                continue
            return cast(
                subprocess.CompletedProcess[str] | subprocess.CompletedProcess[bytes],
                subprocess.CompletedProcess(list(command), process.returncode, stdout, stderr),
            )
    except BaseException:
        _terminate_sync_process(process)
        raise


def _terminate_sync_process(process: subprocess.Popen[Any]) -> None:
    if process.poll() is None:
        _kill_process_group(process.pid)
        with contextlib.suppress(ProcessLookupError):
            process.kill()
    with contextlib.suppress(Exception):
        process.communicate(timeout=5)


async def _terminate_async_process(process: asyncio.subprocess.Process) -> None:
    if process.returncode is None:
        _kill_process_group(process.pid)
        with contextlib.suppress(ProcessLookupError):
            process.kill()
    with contextlib.suppress(Exception):
        await asyncio.shield(process.wait())


def _kill_process_group(pid: int | None) -> None:
    if os.name != "posix" or pid is None:
        return
    with contextlib.suppress(ProcessLookupError, PermissionError):
        os.killpg(pid, signal.SIGKILL)


class _NullSemaphore:
    def __enter__(self) -> None:
        return None

    def __exit__(self, *_args: object) -> None:
        return None
