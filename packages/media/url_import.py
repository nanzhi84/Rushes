"""URL import download helper."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import unquote, urlsplit
from uuid import uuid4

import httpx

from storage.object_store import ObjectRef, ObjectStore
from storage.workspace_paths import WorkspacePaths

DEFAULT_MAX_BYTES = 1024 * 1024 * 1024
HTML_CONTENT_TYPES = frozenset({"text/html", "application/xhtml+xml"})


class UrlImportError(RuntimeError):
    def __init__(self, message: str, *, retryable: bool = False) -> None:
        super().__init__(message)
        self.retryable = retryable


@dataclass(frozen=True, slots=True)
class UrlImportResult:
    object_ref: ObjectRef
    filename: str
    content_type: str | None


async def download_url_to_object(
    url: str,
    *,
    paths: WorkspacePaths,
    filename: str | None = None,
    max_bytes: int | None = None,
    transport: httpx.AsyncBaseTransport | None = None,
) -> UrlImportResult:
    paths.initialize()
    limit = max_bytes or _max_bytes_from_env()
    tmp_path = paths.tmp_dir / f"url_import_{uuid4().hex}.download"
    content_type: str | None = None
    try:
        async with (
            httpx.AsyncClient(
                follow_redirects=True,
                timeout=httpx.Timeout(60.0),
                transport=transport,
            ) as client,
            client.stream("GET", url) as response,
        ):
            if response.status_code >= 400:
                raise UrlImportError(
                    f"url import failed with HTTP {response.status_code}",
                    retryable=response.status_code >= 500,
                )
            content_type = _normalized_content_type(response.headers.get("content-type"))
            if content_type in HTML_CONTENT_TYPES:
                raise UrlImportError("html content-type is not importable")
            content_length = response.headers.get("content-length")
            if content_length is not None:
                try:
                    parsed_content_length = int(content_length)
                except ValueError as exc:
                    raise UrlImportError("invalid content-length header") from exc
                if parsed_content_length > limit:
                    raise UrlImportError("url import exceeds configured size limit")
            total = 0
            with tmp_path.open("wb") as file:
                async for chunk in response.aiter_bytes():
                    total += len(chunk)
                    if total > limit:
                        raise UrlImportError("url import exceeds configured size limit")
                    file.write(chunk)
        object_ref = ObjectStore(paths).put_file(tmp_path)
        return UrlImportResult(
            object_ref=object_ref,
            filename=filename or _filename_from_url(url),
            content_type=content_type,
        )
    except httpx.HTTPError as exc:
        raise UrlImportError(str(exc), retryable=True) from exc
    finally:
        tmp_path.unlink(missing_ok=True)


def _normalized_content_type(value: str | None) -> str | None:
    if value is None:
        return None
    return value.split(";", 1)[0].strip().lower()


def _filename_from_url(url: str) -> str:
    path = unquote(urlsplit(url).path)
    name = Path(path).name
    return name or "downloaded-asset"


def _max_bytes_from_env() -> int:
    raw = os.environ.get("RUSHES_IMPORT_URL_MAX_BYTES")
    if raw is None:
        return DEFAULT_MAX_BYTES
    try:
        return int(raw)
    except ValueError:
        return DEFAULT_MAX_BYTES
