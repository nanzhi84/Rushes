"""Shared synchronous REST/SSE client and driver helpers for M9 path scripts."""

from __future__ import annotations

import contextlib
import json
import os
import signal
import subprocess
import sys
import time
import uuid
from collections.abc import Callable, Iterable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Literal, cast
from urllib.parse import urlsplit, urlunsplit

import httpx

JsonObject = dict[str, Any]
Scenario = Literal["path1", "path2", "scenery"]

REPO_ROOT = Path(__file__).resolve().parents[2]
PACKAGES_ROOT = REPO_ROOT / "packages"
DEFAULT_API_URL = "http://127.0.0.1:8000"
DEFAULT_TOKEN = "e2e-token"
SKIP_OR_REJECT_IDS = frozenset(
    {
        "skip",
        "reject",
        "rejected",
        "deny",
        "denied",
        "no",
        "cancel",
        "discard",
        "discarded",
    }
)


class RunError(RuntimeError):
    """A manual driver stage failed."""


@dataclass(frozen=True, slots=True)
class CommandResult:
    stdout: str
    stderr: str


@dataclass(frozen=True, slots=True)
class SseEvent:
    event_id: int
    event_type: str
    data: JsonObject

    @property
    def event(self) -> JsonObject:
        raw = self.data.get("event")
        return dict(raw) if isinstance(raw, Mapping) else {}


@dataclass(frozen=True, slots=True)
class DecisionChoice:
    option_id: str | None
    free_text: str | None
    answered_via: Literal["button", "natural_language"]
    payload: JsonObject
    reason: str

    def to_answer(self) -> JsonObject:
        answer: JsonObject = {
            "answered_via": self.answered_via,
            "payload": dict(self.payload),
        }
        if self.option_id is not None:
            answer["option_id"] = self.option_id
        if self.free_text is not None:
            answer["free_text"] = self.free_text
        return answer


class SseLineParser:
    """Incremental parser for W3C-style SSE lines."""

    def __init__(self) -> None:
        self._event_id: str | None = None
        self._event_type = "message"
        self._data_lines: list[str] = []

    def feed_line(self, raw_line: str) -> SseEvent | None:
        line = raw_line.rstrip("\r\n")
        if line == "":
            return self.flush()
        if line.startswith(":"):
            return None
        field_name, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        if field_name == "id":
            self._event_id = value
        elif field_name == "event":
            self._event_type = value or "message"
        elif field_name == "data":
            self._data_lines.append(value)
        return None

    def flush(self) -> SseEvent | None:
        if not self._data_lines:
            self._event_type = "message"
            return None
        raw_data = "\n".join(self._data_lines)
        try:
            decoded = json.loads(raw_data)
        except json.JSONDecodeError:
            decoded = {"raw": raw_data}
        data = dict(decoded) if isinstance(decoded, Mapping) else {"data": decoded}
        event_id = _int_or_zero(self._event_id)
        event = SseEvent(event_id=event_id, event_type=self._event_type, data=data)
        self._event_type = "message"
        self._data_lines = []
        return event


def ensure_import_paths() -> None:
    for path in (str(PACKAGES_ROOT), str(REPO_ROOT)):
        if path not in sys.path:
            sys.path.insert(0, path)


ensure_import_paths()


def load_dotenv(path: Path | None = None) -> None:
    env_path = path or REPO_ROOT / ".env"
    if not env_path.exists():
        return
    for line in env_path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("export "):
            stripped = stripped.removeprefix("export ").strip()
        key, separator, value = stripped.partition("=")
        key = key.strip()
        if separator != "=" or not key or key in os.environ:
            continue
        os.environ[key] = _clean_env_value(value.strip())


def _clean_env_value(value: str) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value.split(" #", maxsplit=1)[0].strip()


def stage_log(message: str) -> None:
    stamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"[{stamp}] {message}", flush=True)


def unique_id(prefix: str) -> str:
    return f"{prefix}_{datetime.now().strftime('%Y%m%d%H%M%S')}_{uuid.uuid4().hex[:8]}"


def parse_sse_events(lines: Iterable[str]) -> list[SseEvent]:
    parser = SseLineParser()
    events: list[SseEvent] = []
    for line in lines:
        event = parser.feed_line(line)
        if event is not None:
            events.append(event)
    tail = parser.flush()
    if tail is not None:
        events.append(tail)
    return events


def choose_decision_answer(
    decision: Mapping[str, Any],
    *,
    scenario: Scenario,
    draft_state: Mapping[str, Any] | None = None,
) -> DecisionChoice:
    decision_type = str(decision.get("type") or "")
    options = _decision_options(decision)

    if decision_type == "audio_mode":
        if scenario == "scenery":
            mode = "silent"
        elif scenario == "path1":
            mode = "rough_cut"
        else:
            mode = "tts"
        option = _find_option(
            options,
            preferred_ids=(mode,),
            payload_key="mode",
            payload_value=mode,
        )
        if option is not None:
            return _choice_from_option(option, f"选择音频模式 {mode}")
        return _natural_choice(f"选择 {mode}", {"mode": mode}, f"用自然语言选择 {mode}")

    if decision_type == "approve_content_plan":
        return _approval_choice(options, "确认内容计划")

    if decision_type == "approve_speech_cut":
        option = _find_option(options, preferred_ids=("apply_all", "approve", "confirm"))
        if option is not None:
            return _choice_from_option(option, "应用口播粗剪候选")
        return _natural_choice(
            "确认应用口播粗剪候选",
            {"removed_ranges": [], "total_target_duration_sec": 0.0},
            "兜底确认口播粗剪",
        )

    if decision_type == "approve_rough_cut":
        choice = _approval_choice(options, "确认粗剪预览")
        payload = dict(choice.payload)
        if "timeline_version" not in payload:
            timeline_version = _draft_timeline_version(draft_state)
            if timeline_version is not None:
                payload["timeline_version"] = timeline_version
        return DecisionChoice(
            option_id=choice.option_id,
            free_text=choice.free_text,
            answered_via=choice.answered_via,
            payload=payload,
            reason=choice.reason,
        )

    if decision_type == "subtitle":
        if scenario in {"path1", "scenery"}:
            return _skip_choice(options, "跳过字幕")
        option = _find_option(options, preferred_ids=("clean_bottom",))
        if option is None:
            option = _first_non_skip_option(options)
        if option is not None:
            return _choice_from_option(option, "选择字幕模板")
        return _natural_choice("选择干净底部字幕", {"enabled": True}, "兜底选择字幕")

    if decision_type == "bgm":
        # gate 只把项目里的音频素材列为「使用素材：<文件名>」选项；有就选第一个，否则跳过。
        option = _find_option_by_label_prefix(options, "使用素材：")
        if option is not None:
            return _choice_from_option(option, "选择上传的 BGM 素材")
        return _skip_choice(options, "跳过 BGM")

    if decision_type == "export":
        return _approval_choice(options, "确认最终导出")

    if decision_type == "memory_scope":
        return _skip_choice(options, "跳过 memory 写入")

    if decision_type == "url_import":
        return _approval_choice(options, "确认 URL 导入")

    option = _first_non_skip_option(options)
    if option is not None:
        return _choice_from_option(option, f"未知 decision {decision_type} 选择首个非跳过项")
    if options:
        return _choice_from_option(options[0], f"未知 decision {decision_type} 选择首项")
    return _natural_choice("确认", {}, f"未知 decision {decision_type} 使用自然语言确认")


def summarize_draft_state(draft_state: Mapping[str, Any]) -> str:
    running = draft_state.get("running_jobs")
    running_jobs = running if isinstance(running, list) else []
    running_text = ", ".join(
        f"{_mapping_str(job, 'kind')}:{_mapping_str(job, 'status')}:{_mapping_str(job, 'job_id')}"
        for job in running_jobs
        if isinstance(job, Mapping)
    )
    last_error = draft_state.get("last_error")
    error_text = (
        json.dumps(last_error, ensure_ascii=False) if isinstance(last_error, Mapping) else "无"
    )
    return (
        f"draft={draft_state.get('draft_id')} version={draft_state.get('state_version')} "
        f"pending={draft_state.get('pending_decision_id')} "
        f"timeline={draft_state.get('timeline_current_version')} "
        f"preview={draft_state.get('preview_current_id')} "
        f"export={draft_state.get('export_current_id')} "
        f"running=[{running_text or '无'}] last_error={error_text}"
    )


def summarize_event(event: SseEvent) -> str:
    payload = event.event
    event_name = str(payload.get("event") or event.event_type)
    parts = [f"#{event.event_id}", event_name]
    for key in (
        "decision_id",
        "job_id",
        "draft_id",
        "asset_id",
        "timeline_version",
        "artifact_id",
    ):
        value = payload.get(key)
        if value is not None:
            parts.append(f"{key}={value}")
    event_payload = payload.get("payload")
    if isinstance(event_payload, Mapping):
        kind = event_payload.get("kind")
        if kind is not None:
            parts.append(f"kind={kind}")
    return " ".join(parts)


def require_executable(name: str) -> str:
    from shutil import which

    executable = which(name)
    if executable is None:
        raise RunError(f"缺少命令 {name}，请先安装或确认它在 PATH 中。")
    return executable


def run_command(
    command: Sequence[str],
    *,
    description: str,
    cwd: Path | None = None,
) -> CommandResult:
    completed = subprocess.run(
        list(command),
        cwd=cwd,
        text=True,
        capture_output=True,
        check=False,
    )
    if completed.returncode != 0:
        tail = (completed.stderr or completed.stdout)[-2000:]
        raise RunError(f"{description}失败，退出码 {completed.returncode}。\n{tail}")
    return CommandResult(stdout=completed.stdout, stderr=completed.stderr)


def ffprobe_duration_s(path: Path) -> float:
    require_executable("ffprobe")
    result = run_command(
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
        description=f"ffprobe 读取时长 {path}",
    )
    try:
        duration = float(result.stdout.strip())
    except ValueError as exc:
        raise RunError(f"ffprobe 未返回可解析时长：{result.stdout!r}") from exc
    if duration <= 0:
        raise RunError(f"ffprobe 时长无效：{path} duration={duration}")
    return duration


def normalize_api_url(api_url: str) -> str:
    raw = api_url if "://" in api_url else f"http://{api_url}"
    parsed = urlsplit(raw.rstrip("/"))
    host = parsed.hostname or "127.0.0.1"
    if host in {"localhost", "::1"}:
        host = "127.0.0.1"
    netloc = host if parsed.port is None else f"{host}:{parsed.port}"
    return urlunsplit((parsed.scheme or "http", netloc, parsed.path.rstrip("/"), "", ""))


def api_port(api_url: str) -> int:
    parsed = urlsplit(normalize_api_url(api_url))
    return parsed.port or (443 if parsed.scheme == "https" else 80)


class RushesClient:
    def __init__(self, api_url: str, token: str, *, timeout_s: float = 30.0) -> None:
        self.api_url = normalize_api_url(api_url)
        self.token = token
        self._client = httpx.Client(
            base_url=self.api_url,
            headers={"Authorization": f"Bearer {token}"},
            timeout=httpx.Timeout(timeout_s, connect=5.0),
            trust_env=False,
            transport=httpx.HTTPTransport(local_address="0.0.0.0"),
        )

    def __enter__(self) -> RushesClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def close(self) -> None:
        self._client.close()

    def wait_ready(self, *, timeout_s: float = 60.0) -> None:
        deadline = time.monotonic() + timeout_s
        last_error: BaseException | None = None
        while time.monotonic() <= deadline:
            try:
                self.get_json("/api/drafts", context="等待 API 就绪")
                return
            except (httpx.HTTPError, RunError) as exc:
                last_error = exc
                time.sleep(0.5)
        raise RunError(f"等待 API 就绪超时：{last_error}")

    def get_json(self, path: str, *, context: str) -> JsonObject:
        try:
            response = self._client.get(path)
        except httpx.HTTPError as exc:
            raise RunError(f"{context} 请求失败：{exc}") from exc
        return _checked_json(response, context=context)

    def post_json(self, path: str, payload: Mapping[str, Any], *, context: str) -> JsonObject:
        try:
            response = self._client.post(path, json=dict(payload))
        except httpx.HTTPError as exc:
            raise RunError(f"{context} 请求失败：{exc}") from exc
        return _checked_json(response, context=context)

    def create_draft(self, *, draft_id: str, name: str, goal: str | None = None) -> JsonObject:
        payload: JsonObject = {"draft_id": draft_id, "name": name}
        if goal is not None:
            payload["goal"] = goal
        return self.post_json(
            "/api/drafts",
            payload,
            context="创建 Draft",
        )

    def import_local_material(
        self,
        *,
        draft_id: str,
        asset_id: str,
        path: Path,
    ) -> JsonObject:
        return self.post_json(
            f"/api/drafts/{draft_id}/materials/import-local",
            {
                "asset_id": asset_id,
                "path": str(path.resolve()),
                "storage_mode": "reference",
            },
            context=f"导入素材 {path.name}",
        )

    def enqueue_message(
        self,
        *,
        draft_id: str,
        content: str,
        message_id: str | None = None,
    ) -> JsonObject:
        payload: JsonObject = {"content": content}
        if message_id is not None:
            payload["message_id"] = message_id
        return self.post_json(
            f"/api/drafts/{draft_id}/messages",
            payload,
            context="发送用户消息",
        )

    def get_draft(self, *, draft_id: str) -> JsonObject:
        payload = self.get_json(
            f"/api/drafts/{draft_id}",
            context="读取 Draft",
        )
        draft = payload.get("draft")
        if not isinstance(draft, Mapping):
            raise RunError("读取 Draft 返回缺少 draft object。")
        return dict(draft)

    def current_decision(self, *, draft_id: str) -> JsonObject | None:
        payload = self.get_json(
            f"/api/drafts/{draft_id}/decisions/current",
            context="读取当前 decision",
        )
        decision = payload.get("decision")
        return dict(decision) if isinstance(decision, Mapping) else None

    def answer_decision(
        self,
        decision: Mapping[str, Any],
        choice: DecisionChoice,
    ) -> JsonObject:
        decision_id = str(decision.get("decision_id") or "")
        if not decision_id:
            raise RunError("decision 缺少 decision_id。")
        payload: JsonObject = {"answer": choice.to_answer()}
        draft_id = decision.get("draft_id")
        if isinstance(draft_id, str):
            payload["draft_id"] = draft_id
        return self.post_json(
            f"/api/decisions/{decision_id}/answer",
            payload,
            context=f"回答 decision {decision_id}",
        )

    def mark_preview_viewed(
        self,
        *,
        draft_id: str,
        preview_id: str,
    ) -> JsonObject:
        return self.post_json(
            f"/api/drafts/{draft_id}/previews/{preview_id}/viewed",
            {},
            context=f"标记预览已看 {preview_id}",
        )

    def download_export(self, *, export_id: str, output_path: Path) -> None:
        output_path.parent.mkdir(parents=True, exist_ok=True)
        try:
            with self._client.stream("GET", f"/api/media/export/{export_id}") as response:
                _raise_for_status(response, context=f"下载导出 {export_id}")
                with output_path.open("wb") as handle:
                    for chunk in response.iter_bytes():
                        if chunk:
                            handle.write(chunk)
        except httpx.HTTPError as exc:
            raise RunError(f"下载导出 {export_id} 请求失败：{exc}") from exc

    def poll_draft_events(
        self,
        *,
        draft_id: str,
        last_event_id: int,
        read_timeout_s: float = 1.0,
        max_events: int = 50,
    ) -> list[SseEvent]:
        return self._poll_sse(
            f"/api/drafts/{draft_id}/events",
            last_event_id=last_event_id,
            read_timeout_s=read_timeout_s,
            max_events=max_events,
        )

    def _poll_sse(
        self,
        path: str,
        *,
        last_event_id: int,
        read_timeout_s: float,
        max_events: int,
    ) -> list[SseEvent]:
        timeout = httpx.Timeout(
            5.0,
            connect=5.0,
            read=read_timeout_s,
            write=5.0,
            pool=5.0,
        )
        params = {"token": self.token, "last_event_id": str(max(0, last_event_id))}
        parser = SseLineParser()
        events: list[SseEvent] = []
        try:
            with self._client.stream(
                "GET",
                path,
                params=params,
                timeout=timeout,
            ) as response:
                _raise_for_status(response, context="消费 SSE")
                for line in response.iter_lines():
                    event = parser.feed_line(line)
                    if event is not None:
                        events.append(event)
                    if len(events) >= max_events:
                        break
        except httpx.ReadTimeout:
            pass
        except httpx.HTTPError as exc:
            raise RunError(f"消费 SSE 请求失败：{exc}") from exc
        tail = parser.flush()
        if tail is not None:
            events.append(tail)
        return events


@dataclass
class DraftDriver:
    client: RushesClient
    draft_id: str
    scenario: Scenario
    event_cursor: int = 0
    recent_events: list[SseEvent] = field(default_factory=list)
    seen_decision_types: set[str] = field(default_factory=set)
    seen_event_types: set[str] = field(default_factory=set)
    decision_rounds: int = 0

    def poll_events(self) -> None:
        events = self.client.poll_draft_events(
            draft_id=self.draft_id,
            last_event_id=self.event_cursor,
        )
        for event in events:
            self.event_cursor = max(self.event_cursor, event.event_id)
            self.recent_events.append(event)
            self.seen_event_types.add(str(event.event.get("event") or event.event_type))
            if len(self.recent_events) > 30:
                self.recent_events = self.recent_events[-30:]
            stage_log(f"SSE {summarize_event(event)}")

    def current_draft(self) -> JsonObject:
        return self.client.get_draft(draft_id=self.draft_id)

    def answer_current_decision(self, draft_state: Mapping[str, Any]) -> bool:
        decision = self.client.current_decision(draft_id=self.draft_id)
        if decision is None:
            return False
        self.decision_rounds += 1
        if self.decision_rounds > 20:
            raise RunError("decision 自动答复超过 20 轮，疑似死循环。")
        decision_type = str(decision.get("type") or "unknown")
        self.seen_decision_types.add(decision_type)
        choice = choose_decision_answer(decision, scenario=self.scenario, draft_state=draft_state)
        decision_id = str(decision.get("decision_id") or "")
        stage_log(
            "自动回答 decision "
            f"type={decision_type} id={decision_id} answer={choice.option_id or choice.free_text} "
            f"reason={choice.reason}"
        )
        response = self.client.answer_decision(decision, choice)
        replays = int(response.get("replays_enqueued") or 0)
        if replays == 0 and decision_type != "export":
            self.nudge("继续按当前确认结果推进。")
        return True

    def wait_until(
        self,
        description: str,
        predicate: Callable[[Mapping[str, Any]], bool],
        *,
        timeout_s: float,
        idle_nudge: str = "继续推进下一步。",
        poll_interval_s: float = 1.0,
    ) -> JsonObject:
        stage_log(f"等待：{description}")
        deadline = time.monotonic() + timeout_s
        last_nudge_at = time.monotonic()
        while time.monotonic() <= deadline:
            self.poll_events()
            self._raise_on_failed_job()
            draft_state = self.current_draft()
            if predicate(draft_state):
                stage_log(f"完成：{description}；{summarize_draft_state(draft_state)}")
                return draft_state
            if self.answer_current_decision(draft_state):
                last_nudge_at = time.monotonic()
                time.sleep(0.5)
                continue
            if _draft_last_error(draft_state) is not None:
                raise RunError(f"{description} 失败：{summarize_draft_state(draft_state)}")
            if (
                not _draft_has_running_jobs(draft_state)
                and time.monotonic() - last_nudge_at >= 20.0
            ):
                self.nudge(idle_nudge)
                last_nudge_at = time.monotonic()
            time.sleep(poll_interval_s)
        draft_state = self.current_draft()
        raise RunError(self.timeout_diagnostic(description, draft_state))

    def nudge(self, content: str) -> None:
        stage_log(f"发送继续消息：{content}")
        self.client.enqueue_message(
            draft_id=self.draft_id,
            content=content,
            message_id=unique_id("msg"),
        )

    def require_decisions_seen(self, decision_types: Sequence[str]) -> None:
        missing = [item for item in decision_types if item not in self.seen_decision_types]
        if missing:
            raise RunError(f"缺少期望 decision type：{', '.join(missing)}")

    def timeout_diagnostic(self, description: str, draft_state: Mapping[str, Any]) -> str:
        lines = [f"{description} 超时。", summarize_draft_state(draft_state), "最近 SSE 事件："]
        lines.extend(f"- {summarize_event(event)}" for event in self.recent_events[-10:])
        return "\n".join(lines)

    def _raise_on_failed_job(self) -> None:
        for event in self.recent_events[-10:]:
            if str(event.event.get("event") or event.event_type) == "JobFailed":
                raise RunError(f"job 失败：{summarize_event(event)}")


class ManagedProcessGroup:
    def __init__(self, *, workspace: Path) -> None:
        self.workspace = workspace
        self._processes: list[subprocess.Popen[bytes]] = []

    def __enter__(self) -> ManagedProcessGroup:
        return self

    def __exit__(self, *_: object) -> None:
        self.stop()

    def start(
        self,
        name: str,
        command: str,
        args: Sequence[str],
        *,
        env: Mapping[str, str],
        cwd: Path,
    ) -> None:
        logs_dir = self.workspace / "logs"
        logs_dir.mkdir(parents=True, exist_ok=True)
        log_path = logs_dir / f"{name}.log"
        stage_log(f"启动 {name}，日志：{log_path}")
        with log_path.open("ab") as log_file:
            process = subprocess.Popen(
                [command, *args],
                cwd=cwd,
                env=dict(env),
                stdin=subprocess.DEVNULL,
                stdout=log_file,
                stderr=log_file,
                start_new_session=True,
            )
        self._processes.append(process)

    def stop(self) -> None:
        for process in reversed(self._processes):
            if process.poll() is not None:
                continue
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                continue
            try:
                process.wait(timeout=10.0)
            except subprocess.TimeoutExpired:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5.0)


def start_autostart(
    *,
    api_url: str,
    token: str,
    workspace: Path,
    fs_roots: Sequence[Path],
) -> ManagedProcessGroup:
    workspace.mkdir(parents=True, exist_ok=True)
    group = ManagedProcessGroup(workspace=workspace)
    port = api_port(api_url)
    uv = require_executable("uv")
    env = os.environ.copy()
    env.update(
        {
            "RUSHES_WORKSPACE_PATH": str(workspace),
            "RUSHES_API_TOKEN": token,
            "RUSHES_API_PORT": str(port),
            "RUSHES_FS_ROOTS": os.pathsep.join(_unique_paths([*fs_roots, workspace])),
        }
    )
    try:
        group.start(
            "api",
            uv,
            [
                "run",
                "uvicorn",
                "apps.api.main:create_app_from_env",
                "--factory",
                "--host",
                "127.0.0.1",
                "--port",
                str(port),
            ],
            env=env,
            cwd=REPO_ROOT,
        )
        with RushesClient(api_url, token) as client:
            client.wait_ready(timeout_s=60.0)
        group.start(
            "worker",
            uv,
            [
                "run",
                "python",
                "-m",
                "apps.worker.main",
                str(workspace),
                "--worker-id",
                "e2e-paths-worker",
                "--poll-interval",
                "0.5",
            ],
            env=env,
            cwd=REPO_ROOT,
        )
    except BaseException:
        group.stop()
        raise
    return group


def _checked_json(response: httpx.Response, *, context: str) -> JsonObject:
    _raise_for_status(response, context=context)
    try:
        payload = response.json()
    except ValueError as exc:
        raise RunError(f"{context} 返回非 JSON：{response.text[:800]}") from exc
    if not isinstance(payload, Mapping):
        raise RunError(f"{context} 返回 JSON 顶层不是 object。")
    return dict(payload)


def _raise_for_status(response: httpx.Response, *, context: str) -> None:
    if 200 <= response.status_code < 300:
        return
    try:
        body = response.text
    except httpx.ResponseNotRead:
        body = response.read().decode("utf-8", errors="replace")
    raise RunError(f"{context} HTTP {response.status_code}：{body[:1200]}")


def _decision_options(decision: Mapping[str, Any]) -> list[Mapping[str, Any]]:
    options = decision.get("options")
    if not isinstance(options, list):
        return []
    return [cast(Mapping[str, Any], item) for item in options if isinstance(item, Mapping)]


def _find_option(
    options: Sequence[Mapping[str, Any]],
    *,
    preferred_ids: Sequence[str],
    payload_key: str | None = None,
    payload_value: object | None = None,
) -> Mapping[str, Any] | None:
    preferred = set(preferred_ids)
    for option in options:
        if _option_id(option) in preferred:
            return option
    if payload_key is None:
        return None
    for option in options:
        payload = _option_payload(option)
        if payload.get(payload_key) == payload_value:
            return option
    return None


def _find_option_by_label_prefix(
    options: Sequence[Mapping[str, Any]],
    prefix: str,
) -> Mapping[str, Any] | None:
    for option in options:
        if str(option.get("label") or "").startswith(prefix):
            return option
    return None


def _choice_from_option(option: Mapping[str, Any], reason: str) -> DecisionChoice:
    return DecisionChoice(
        option_id=_option_id(option),
        free_text=None,
        answered_via="button",
        payload=_option_payload(option),
        reason=reason,
    )


def _approval_choice(options: Sequence[Mapping[str, Any]], reason: str) -> DecisionChoice:
    option = _find_option(options, preferred_ids=("approve", "confirm", "ok", "yes"))
    if option is None:
        option = _first_non_skip_option(options)
    if option is not None:
        payload = _option_payload(option)
        payload.setdefault("approved", True)
        return DecisionChoice(
            option_id=_option_id(option),
            free_text=None,
            answered_via="button",
            payload=payload,
            reason=reason,
        )
    return _natural_choice("确认", {"approved": True}, reason)


def _skip_choice(options: Sequence[Mapping[str, Any]], reason: str) -> DecisionChoice:
    option = _find_option(options, preferred_ids=("skip", "discard", "no"))
    if option is not None:
        return _choice_from_option(option, reason)
    return _natural_choice("跳过", {"enabled": False}, reason)


def _natural_choice(free_text: str, payload: Mapping[str, Any], reason: str) -> DecisionChoice:
    return DecisionChoice(
        option_id=None,
        free_text=free_text,
        answered_via="natural_language",
        payload=dict(payload),
        reason=reason,
    )


def _first_non_skip_option(
    options: Sequence[Mapping[str, Any]],
) -> Mapping[str, Any] | None:
    for option in options:
        if _option_id(option) not in SKIP_OR_REJECT_IDS:
            return option
    return None


def _option_id(option: Mapping[str, Any]) -> str:
    return str(option.get("option_id") or "")


def _option_payload(option: Mapping[str, Any]) -> JsonObject:
    payload = option.get("payload")
    return dict(payload) if isinstance(payload, Mapping) else {}


def _draft_timeline_version(draft_state: Mapping[str, Any] | None) -> int | None:
    if draft_state is None:
        return None
    value = draft_state.get("timeline_current_version")
    return value if isinstance(value, int) else None


def _draft_has_running_jobs(draft_state: Mapping[str, Any]) -> bool:
    running = draft_state.get("running_jobs")
    return isinstance(running, list) and bool(running)


def _draft_last_error(draft_state: Mapping[str, Any]) -> Mapping[str, Any] | None:
    error = draft_state.get("last_error")
    return error if isinstance(error, Mapping) else None


def _mapping_str(mapping: object, key: str) -> str:
    if not isinstance(mapping, Mapping):
        return "-"
    value = mapping.get(key)
    return "-" if value is None else str(value)


def _int_or_zero(value: str | None) -> int:
    try:
        return int(value or "0")
    except ValueError:
        return 0


def _unique_paths(paths: Sequence[Path]) -> list[str]:
    result: list[str] = []
    seen: set[str] = set()
    for path in paths:
        resolved = str(path.expanduser().resolve(strict=False))
        if resolved in seen:
            continue
        seen.add(resolved)
        result.append(resolved)
    return result
