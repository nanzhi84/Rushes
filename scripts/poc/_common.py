"""Shared helpers for Rushes M-1 POC scripts."""

from __future__ import annotations

import base64
import json
import os
import re
import shutil
import subprocess
import sys
import time
import unicodedata
from collections.abc import Callable, Iterator, Mapping, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from types import TracebackType
from typing import Any, cast

import httpx

ROOT = Path(__file__).resolve().parents[2]
PACKAGES = ROOT / "packages"
if str(PACKAGES) not in sys.path:
    sys.path.insert(0, str(PACKAGES))
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

DASHSCOPE_BASE_URL = "https://dashscope.aliyuncs.com"
DASHSCOPE_COMPAT_BASE_URL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
EXIT_SKIP = 2

JsonObject = dict[str, Any]


class PocError(RuntimeError):
    """A POC failed and should return exit code 1."""


class PocSkip(RuntimeError):
    """A POC cannot run in the current local environment and should return 2."""


@dataclass(frozen=True)
class CommandOutput:
    stdout: str
    stderr: str


@dataclass
class Stopwatch:
    label: str
    started_at: float = 0.0
    ended_at: float | None = None

    def __enter__(self) -> Stopwatch:
        self.started_at = time.monotonic()
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self.ended_at = time.monotonic()

    @property
    def elapsed_ms(self) -> int:
        end = self.ended_at if self.ended_at is not None else time.monotonic()
        return int((end - self.started_at) * 1000)


def timestamp() -> str:
    return datetime.now(tz=UTC).strftime("%Y%m%dT%H%M%SZ")


def ensure_dir(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    return path


def load_dotenv(env_path: Path | None = None) -> None:
    path = env_path or ROOT / ".env"
    if not path.exists():
        return
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("export "):
            stripped = stripped.removeprefix("export ").strip()
        key, separator, value = stripped.partition("=")
        key = key.strip()
        if separator != "=" or not key or key.startswith("#"):
            continue
        if key in os.environ:
            continue
        os.environ[key] = _clean_env_value(value.strip())


def _clean_env_value(value: str) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value.split(" #", maxsplit=1)[0].strip()


def require_env(name: str, *, label: str | None = None) -> str:
    value = os.environ.get(name)
    if value:
        return value
    display_name = label or name
    raise PocSkip(f"缺少 {display_name}，请在仓库根 .env 设置 {name} 后重跑。")


def require_executable(name: str) -> str:
    executable = shutil.which(name)
    if executable is None:
        raise PocSkip(f"缺少命令 {name}，请先安装或确认它在 PATH 中。")
    return executable


def run_command(
    command: Sequence[str],
    *,
    description: str,
    cwd: Path | None = None,
) -> CommandOutput:
    completed = subprocess.run(
        list(command),
        cwd=cwd,
        text=True,
        capture_output=True,
        check=False,
    )
    if completed.returncode != 0:
        tail = (completed.stderr or completed.stdout)[-1600:]
        raise PocError(f"{description}失败，退出码 {completed.returncode}。\n{tail}")
    return CommandOutput(stdout=completed.stdout, stderr=completed.stderr)


def retry[T](
    operation: Callable[[], T],
    *,
    attempts: int = 3,
    initial_delay_s: float = 1.0,
    description: str,
) -> T:
    if attempts < 1:
        raise ValueError("attempts must be >= 1")
    delay = initial_delay_s
    last_error: BaseException | None = None
    for attempt in range(1, attempts + 1):
        try:
            return operation()
        except (httpx.HTTPError, PocError) as exc:
            last_error = exc
            if attempt == attempts:
                break
            print(f"{description} 第 {attempt} 次失败，{delay:.1f}s 后重试：{exc}")
            time.sleep(delay)
            delay *= 2
    raise PocError(f"{description} 多次失败：{last_error}") from last_error


def write_json(path: Path, data: object) -> None:
    ensure_dir(path.parent)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def read_json(path: Path) -> JsonObject:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise PocError(f"{path} 不是 JSON object。")
    return cast(JsonObject, data)


def response_json_object(response: httpx.Response, *, context: str) -> JsonObject:
    try:
        data = response.json()
    except ValueError as exc:
        raise PocError(f"{context} 返回的不是 JSON：{response.text[:800]}") from exc
    if not isinstance(data, dict):
        raise PocError(f"{context} 返回 JSON 顶层不是 object。")
    return cast(JsonObject, data)


def checked_response_json(response: httpx.Response, *, context: str) -> JsonObject:
    if response.status_code < 200 or response.status_code >= 300:
        raise PocError(f"{context} HTTP {response.status_code}：{response.text[:1200]}")
    return response_json_object(response, context=context)


class DashScopeClient:
    def __init__(self, api_key: str, *, timeout_s: float = 60.0) -> None:
        self._client = httpx.Client(timeout=httpx.Timeout(timeout_s, connect=10.0), trust_env=False)
        self._api_key = api_key

    def __enter__(self) -> DashScopeClient:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        self.close()

    def close(self) -> None:
        self._client.close()

    def post_json(
        self,
        url: str,
        payload: Mapping[str, object],
        *,
        extra_headers: Mapping[str, str] | None = None,
        context: str,
    ) -> JsonObject:
        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "Content-Type": "application/json",
        }
        if extra_headers is not None:
            headers.update(extra_headers)
        try:
            response = self._client.post(url, json=dict(payload), headers=headers)
        except httpx.HTTPError as exc:
            raise PocError(f"{context} 请求失败：{exc}") from exc
        return checked_response_json(response, context=context)

    def get_json(
        self,
        url: str,
        *,
        extra_headers: Mapping[str, str] | None = None,
        context: str,
    ) -> JsonObject:
        headers = {"Authorization": f"Bearer {self._api_key}"}
        if extra_headers is not None:
            headers.update(extra_headers)
        try:
            response = self._client.get(url, headers=headers)
        except httpx.HTTPError as exc:
            raise PocError(f"{context} 请求失败：{exc}") from exc
        return checked_response_json(response, context=context)

    def submit_asr_transcription(self, file_url: str) -> JsonObject:
        payload: JsonObject = {
            "model": "paraformer-v2",
            "input": {"file_urls": [file_url]},
            "parameters": {
                "disfluency_removal_enabled": False,
                "timestamp_alignment_enabled": True,
                "language_hints": ["zh"],
            },
        }
        return self.post_json(
            f"{DASHSCOPE_BASE_URL}/api/v1/services/audio/asr/transcription",
            payload,
            extra_headers={"X-DashScope-Async": "enable"},
            context="提交 DashScope ASR 任务",
        )

    def get_task(self, task_id: str) -> JsonObject:
        return self.get_json(
            f"{DASHSCOPE_BASE_URL}/api/v1/tasks/{task_id}",
            context=f"查询 DashScope 任务 {task_id}",
        )

    def chat_completions(self, payload: Mapping[str, object]) -> JsonObject:
        return self.post_json(
            f"{DASHSCOPE_COMPAT_BASE_URL}/chat/completions",
            payload,
            context="调用 DashScope OpenAI 兼容 chat",
        )


def http_get_json(url: str, *, context: str, timeout_s: float = 60.0) -> JsonObject:
    with httpx.Client(timeout=httpx.Timeout(timeout_s, connect=10.0), trust_env=False) as client:
        try:
            response = client.get(url)
        except httpx.HTTPError as exc:
            raise PocError(f"{context} 请求失败：{exc}") from exc
    return checked_response_json(response, context=context)


def http_get_bytes(url: str, *, context: str, timeout_s: float = 60.0) -> bytes:
    with httpx.Client(timeout=httpx.Timeout(timeout_s, connect=10.0), trust_env=False) as client:
        try:
            response = client.get(url)
        except httpx.HTTPError as exc:
            raise PocError(f"{context} 请求失败：{exc}") from exc
    if response.status_code < 200 or response.status_code >= 300:
        raise PocError(f"{context} HTTP {response.status_code}：{response.text[:800]}")
    return response.content


def iter_json_objects(value: object) -> Iterator[Mapping[str, object]]:
    if isinstance(value, Mapping):
        yield cast(Mapping[str, object], value)
        for child in value.values():
            yield from iter_json_objects(child)
    elif isinstance(value, list):
        for child in value:
            yield from iter_json_objects(child)


def first_string(mapping: Mapping[str, object], keys: Sequence[str]) -> str | None:
    for key in keys:
        value = mapping.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def first_list(mapping: Mapping[str, object], keys: Sequence[str]) -> list[object] | None:
    for key in keys:
        value = mapping.get(key)
        if isinstance(value, list):
            return value
    return None


def first_mapping_list(
    mapping: Mapping[str, object],
    keys: Sequence[str],
) -> list[Mapping[str, object]]:
    values = first_list(mapping, keys)
    if values is None:
        return []
    return [cast(Mapping[str, object], value) for value in values if isinstance(value, Mapping)]


def first_ms(mapping: Mapping[str, object], keys: Sequence[str]) -> int | None:
    for key in keys:
        value = mapping.get(key)
        parsed = _parse_time_ms(key, value)
        if parsed is not None:
            return parsed
    return None


def _parse_time_ms(key: str, value: object) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, str):
        stripped = value.strip()
        if not stripped:
            return None
        try:
            number = float(stripped)
        except ValueError:
            return None
    elif isinstance(value, int | float):
        number = float(value)
    else:
        return None
    lower_key = key.lower()
    if (
        lower_key.endswith("_s") or lower_key in {"start", "end", "begin", "offset"}
    ) and number < 1000:
        return round(number * 1000)
    return round(number)


def is_punctuation(text: str) -> bool:
    return bool(text) and all(unicodedata.category(char).startswith("P") for char in text)


def compact_text(text: str) -> str:
    without_say_markup = re.sub(r"\[\[\s*slnc\s+\d+\s*\]\]", "", text, flags=re.IGNORECASE)
    chars = []
    for char in without_say_markup:
        category = unicodedata.category(char)
        if category.startswith("P") or category.startswith("Z") or category.startswith("C"):
            continue
        chars.append(char)
    return "".join(chars)


def lcs_ratio(reference: str, hypothesis: str) -> float:
    if not reference:
        return 0.0
    rows = len(reference) + 1
    cols = len(hypothesis) + 1
    previous = [0] * cols
    for row in range(1, rows):
        current = [0] * cols
        ref_char = reference[row - 1]
        for col in range(1, cols):
            if ref_char == hypothesis[col - 1]:
                current[col] = previous[col - 1] + 1
            else:
                current[col] = max(previous[col], current[col - 1])
        previous = current
    return previous[-1] / len(reference)


def ffprobe_duration_s(path: Path) -> float:
    require_executable("ffprobe")
    output = run_command(
        [
            "ffprobe",
            "-v",
            "error",
            "-show_entries",
            "format=duration",
            "-of",
            "default=noprint_wrappers=1:nokey=1",
            str(path),
        ],
        description=f"读取媒体时长 {path}",
    )
    try:
        return float(output.stdout.strip())
    except ValueError as exc:
        raise PocError(f"ffprobe 未返回有效时长：{output.stdout!r}") from exc


def image_data_url(path: Path) -> str:
    encoded = base64.b64encode(path.read_bytes()).decode("ascii")
    return f"data:image/jpeg;base64,{encoded}"


def strip_json_fence(text: str) -> str:
    stripped = text.strip()
    if not stripped.startswith("```"):
        return stripped
    stripped = re.sub(r"^```(?:json)?\s*", "", stripped, flags=re.IGNORECASE)
    stripped = re.sub(r"\s*```$", "", stripped)
    return stripped.strip()
