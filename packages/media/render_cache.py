"""Segment render cache with conservative cache keys and LRU pruning."""

from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from storage.workspace_paths import WorkspacePaths

DEFAULT_MAX_BYTES = 20 * 1024 * 1024 * 1024


@dataclass(frozen=True, slots=True)
class CachePruneResult:
    deleted_paths: tuple[Path, ...]
    deleted_bytes: int
    remaining_bytes: int


class SegmentRenderCache:
    """Filesystem cache under workspace/cache/segments; safe to clear at any time."""

    def __init__(
        self,
        paths: WorkspacePaths,
        *,
        max_bytes: int = DEFAULT_MAX_BYTES,
        suffix: str = ".mp4",
    ) -> None:
        self._paths = paths.initialize()
        self._max_bytes = max_bytes
        self._suffix = suffix
        self._paths.segments_dir.mkdir(parents=True, exist_ok=True)

    @property
    def max_bytes(self) -> int:
        return self._max_bytes

    @property
    def root(self) -> Path:
        return self._paths.segments_dir

    def path_for_key(self, cache_key: str) -> Path:
        _validate_cache_key(cache_key)
        return self.root / cache_key[:2] / f"{cache_key}{self._suffix}"

    def get(self, cache_key: str) -> Path | None:
        path = self.path_for_key(cache_key)
        if not path.exists():
            return None
        os.utime(path, None)
        return path

    def put_file(self, cache_key: str, source_path: Path) -> Path:
        destination = self.path_for_key(cache_key)
        destination.parent.mkdir(parents=True, exist_ok=True)
        tmp_path = destination.with_suffix(f"{destination.suffix}.tmp")
        try:
            os.replace(source_path, tmp_path)
            os.replace(tmp_path, destination)
        finally:
            tmp_path.unlink(missing_ok=True)
        os.utime(destination, None)
        self.prune()
        return destination

    def prune(self) -> CachePruneResult:
        files = [path for path in self.root.glob("*/*") if path.is_file()]
        total = sum(path.stat().st_size for path in files)
        if total <= self._max_bytes:
            return CachePruneResult((), 0, total)

        deleted: list[Path] = []
        deleted_bytes = 0
        for path in sorted(files, key=_lru_sort_key):
            if total <= self._max_bytes:
                break
            size = path.stat().st_size
            path.unlink(missing_ok=True)
            deleted.append(path)
            deleted_bytes += size
            total -= size
        _remove_empty_dirs(self.root)
        return CachePruneResult(tuple(deleted), deleted_bytes, total)


def segment_cache_key(payload: Mapping[str, Any]) -> str:
    """Return sha256 for a canonical segment cache payload."""

    encoded = json.dumps(
        payload,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(encoded.encode("utf-8")).hexdigest()


def _validate_cache_key(cache_key: str) -> None:
    if len(cache_key) != 64 or any(char not in "0123456789abcdef" for char in cache_key):
        raise ValueError("cache_key must be a SHA-256 hex digest")


def _lru_sort_key(path: Path) -> tuple[int, int, str]:
    stat = path.stat()
    return (stat.st_atime_ns, stat.st_mtime_ns, path.name)


def _remove_empty_dirs(root: Path) -> None:
    for path in sorted(root.glob("*"), reverse=True):
        if path.is_dir():
            try:
                path.rmdir()
            except OSError:
                continue
