from __future__ import annotations

from pathlib import Path

import httpx
import pytest

from media.url_import import UrlImportError, download_url_to_object
from storage.workspace_paths import WorkspacePaths


async def test_download_url_to_object_downloads_only_requested_file(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()
    seen_paths: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen_paths.append(request.url.path)
        return httpx.Response(
            200,
            headers={"content-type": "video/mp4", "content-length": "5"},
            content=b"video",
        )

    result = await download_url_to_object(
        "https://example.test/files/clip.mp4",
        paths=paths,
        transport=httpx.MockTransport(handler),
    )

    assert seen_paths == ["/files/clip.mp4"]
    assert result.filename == "clip.mp4"
    assert paths.object_path(result.object_ref.object_hash).read_bytes() == b"video"


async def test_download_url_to_object_rejects_html_content_type(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "text/html"}, text="<html></html>")

    with pytest.raises(UrlImportError, match="html content-type"):
        await download_url_to_object(
            "https://example.test/page",
            paths=paths,
            transport=httpx.MockTransport(handler),
        )


async def test_download_url_to_object_rejects_declared_size_over_limit(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"content-type": "video/mp4", "content-length": "6"},
            content=b"video!",
        )

    with pytest.raises(UrlImportError, match="size limit"):
        await download_url_to_object(
            "https://example.test/clip.mp4",
            paths=paths,
            max_bytes=5,
            transport=httpx.MockTransport(handler),
        )


async def test_download_url_to_object_rejects_stream_over_limit(tmp_path: Path) -> None:
    paths = WorkspacePaths.from_root(tmp_path / "workspace").initialize()

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"content-type": "video/mp4"}, content=b"video!")

    with pytest.raises(UrlImportError, match="size limit"):
        await download_url_to_object(
            "https://example.test/clip.mp4",
            paths=paths,
            max_bytes=5,
            transport=httpx.MockTransport(handler),
        )
