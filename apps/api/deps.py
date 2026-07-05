"""API dependencies and local security baseline enforcement."""

from __future__ import annotations

import json
import os
import secrets
from collections.abc import Awaitable, Callable, Iterable, Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Literal, NoReturn, cast
from urllib.parse import urlsplit

from fastapi import HTTPException, Request
from fastapi.responses import JSONResponse, Response
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from agent_harness.turn_queue import TurnQueue
from contracts.events import DomainEventBase, SecurityRefusal
from events.event_log import deserialize_event
from events.routing import routes_to_case, routes_to_workspace
from storage.repositories.event_log import EventLogRow

type SecurityReason = Literal[
    "missing_token",
    "bad_token",
    "host_mismatch",
    "origin_mismatch",
    "path_escape",
    "bad_content_type",
]
type SsePredicate = Callable[[DomainEventBase], bool]

MUTATION_METHODS = frozenset({"POST", "PATCH", "DELETE", "PUT"})
MEDIA_EXTENSIONS = frozenset(
    {
        ".3gp",
        ".aac",
        ".aif",
        ".aiff",
        ".avi",
        ".flac",
        ".m4a",
        ".m4v",
        ".mkv",
        ".mov",
        ".mp3",
        ".mp4",
        ".mpeg",
        ".mpg",
        ".ogg",
        ".wav",
        ".webm",
    }
)


@dataclass(frozen=True, slots=True)
class ApiState:
    engine: Engine
    token: str
    fs_roots: tuple[Path, ...]
    turn_queue: TurnQueue
    startup_port: int
    # 仅测试用：SSE 流发出 N 条事件后主动收尾。生产保持 None（无限流，
    # 由客户端断开终止）；同步 TestClient 无法消费无限流（会与服务端互等）。
    sse_max_events: int | None = None


class PathEscapeError(Exception):
    def __init__(self, raw_path: str) -> None:
        super().__init__(raw_path)
        self.raw_path = raw_path


def generate_token() -> str:
    return secrets.token_urlsafe(32)


def state_from_request(request: Request) -> ApiState:
    return cast(ApiState, request.app.state.api_state)


def default_fs_roots() -> tuple[Path, ...]:
    home = Path.home()
    return _canonical_roots((home, home / "Movies", home / "Desktop", Path("/Volumes")))


def configured_fs_roots(roots: Sequence[str | Path] | None) -> tuple[Path, ...]:
    if roots is None:
        return default_fs_roots()
    return _canonical_roots(Path(root) for root in roots)


def canonicalize_allowed_path(raw_path: str, roots: Sequence[Path]) -> Path:
    candidate = Path(raw_path).expanduser().resolve(strict=False)
    if not _is_inside_any_root(candidate, roots):
        raise PathEscapeError(raw_path)
    return candidate


def route_case(case_id: str) -> SsePredicate:
    def _predicate(event: DomainEventBase) -> bool:
        return routes_to_case(event, case_id)

    return _predicate


def route_workspace() -> SsePredicate:
    return routes_to_workspace


def event_row_matches(row: EventLogRow, predicate: SsePredicate) -> bool:
    event = deserialize_event(row)
    return predicate(event)


def encode_sse_row(row: EventLogRow) -> str:
    event = deserialize_event(row)
    data = {
        "event_id": row.event_id,
        "event": event.model_dump(mode="json"),
    }
    return (
        f"id: {row.event_id}\n"
        f"event: {event.event}\n"
        f"data: {json.dumps(data, ensure_ascii=False, separators=(',', ':'))}\n\n"
    )


async def security_baseline_middleware(
    request: Request,
    call_next: Callable[[Request], Awaitable[Response]],
) -> Response:
    if not request.url.path.startswith("/api/"):
        return await call_next(request)

    host_reason = _host_refusal_reason(request)
    if host_reason is not None:
        return _security_refusal_response(request, host_reason, 403)

    origin_reason = _origin_refusal_reason(request)
    if origin_reason is not None:
        return _security_refusal_response(request, origin_reason, 403)

    token_reason = _token_refusal_reason(request)
    if token_reason is not None:
        return _security_refusal_response(request, token_reason, 401)

    content_type_reason = _content_type_refusal_reason(request)
    if content_type_reason is not None:
        return _security_refusal_response(request, content_type_reason, 415)

    return await call_next(request)


def refuse_path_escape(request: Request, raw_path: str) -> NoReturn:
    append_security_refusal(request, "path_escape", path=raw_path)
    raise HTTPException(status_code=403, detail={"reason": "path_escape"})


def append_security_refusal(
    request: Request,
    reason: SecurityReason,
    *,
    path: str | None = None,
    origin: str | None = None,
) -> None:
    state = state_from_request(request)
    route = request.url.path
    event = SecurityRefusal(
        security_refusal_id=f"security_refusal_{secrets.token_hex(12)}",
        route=route,
        path=path,
        origin=origin,
        reason=reason,
        payload={
            "route": route,
            "path": path,
            "origin": origin,
            "reason": reason,
        },
    )
    apply(
        (event,),
        engine=state.engine,
        base_version=None,
        actor="system",
        created_at=datetime.now(UTC).isoformat(),
    )


def startup_port_from_env() -> int:
    raw = os.environ.get("RUSHES_API_PORT", "8000")
    try:
        return int(raw)
    except ValueError:
        return 8000


def _security_refusal_response(
    request: Request,
    reason: SecurityReason,
    status_code: int,
) -> JSONResponse:
    append_security_refusal(
        request,
        reason,
        path=_refusal_path(request),
        origin=request.headers.get("origin"),
    )
    return JSONResponse(
        status_code=status_code,
        content={"error": "SecurityRefusal", "reason": reason},
    )


def _host_refusal_reason(request: Request) -> SecurityReason | None:
    host = request.headers.get("host")
    if host is None:
        return "host_mismatch"
    server = request.scope.get("server")
    server_port = server[1] if isinstance(server, tuple) and len(server) == 2 else None
    expected_port = (
        server_port if isinstance(server_port, int) else state_from_request(request).startup_port
    )
    return None if host.lower() == f"127.0.0.1:{expected_port}" else "host_mismatch"


def _origin_refusal_reason(request: Request) -> SecurityReason | None:
    origin = request.headers.get("origin")
    if origin is None:
        return None
    parsed = urlsplit(origin)
    server = request.scope.get("server")
    server_port = server[1] if isinstance(server, tuple) and len(server) == 2 else None
    expected_port = (
        server_port if isinstance(server_port, int) else state_from_request(request).startup_port
    )
    if parsed.scheme == "http" and parsed.netloc == f"127.0.0.1:{expected_port}":
        return None
    return "origin_mismatch"


def _token_refusal_reason(request: Request) -> SecurityReason | None:
    state = state_from_request(request)
    token = _request_token(request)
    if token is None:
        return "missing_token"
    if token != state.token:
        return "bad_token"
    return None


def _request_token(request: Request) -> str | None:
    authorization = request.headers.get("authorization")
    if authorization is not None:
        scheme, separator, token = authorization.partition(" ")
        if separator and scheme.lower() == "bearer" and token:
            return token
        return ""
    if _is_sse_endpoint(request.url.path):
        query_token = request.query_params.get("token")
        if query_token:
            return query_token
    return None


def _content_type_refusal_reason(request: Request) -> SecurityReason | None:
    if request.method.upper() not in MUTATION_METHODS:
        return None
    content_type = request.headers.get("content-type", "")
    media_type = content_type.split(";", 1)[0].strip().lower()
    if media_type == "application/json":
        return None
    return "bad_content_type"


def _is_sse_endpoint(path: str) -> bool:
    return path == "/api/events" or path.endswith("/events")


def _refusal_path(request: Request) -> str | None:
    if request.url.path.startswith("/api/fs/"):
        return request.query_params.get("path")
    return request.url.path


def _canonical_roots(roots: Iterable[Path]) -> tuple[Path, ...]:
    seen: set[str] = set()
    canonical: list[Path] = []
    for root in roots:
        path = root.expanduser().resolve(strict=False)
        key = str(path)
        if key in seen:
            continue
        seen.add(key)
        canonical.append(path)
    return tuple(canonical)


def _is_inside_any_root(path: Path, roots: Sequence[Path]) -> bool:
    return any(path == root or root in path.parents for root in roots)
