"""Content-addressed object store for PRD §3.7."""

from __future__ import annotations

import hashlib
import os
from collections.abc import Collection, Iterator
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import BinaryIO
from uuid import uuid4

from .repositories.objects import ObjectsRepository
from .workspace_paths import WorkspacePaths

CHUNK_SIZE = 1024 * 1024


def sha256_file(path: str | Path) -> str:
    """分块读取整文件算 sha256（内容寻址与 hash job 共用的 canonical 实现）。"""
    digest = hashlib.sha256()
    with Path(path).open("rb") as source_file:
        for chunk in iter(lambda: source_file.read(CHUNK_SIZE), b""):
            digest.update(chunk)
    return digest.hexdigest()


@dataclass(frozen=True, slots=True)
class ObjectRef:
    object_hash: str
    rel_path: str
    size: int


@dataclass(frozen=True, slots=True)
class ObjectGcResult:
    deleted_hashes: tuple[str, ...]
    deleted_rows: int


class ObjectStore:
    def __init__(self, paths: WorkspacePaths, repository: ObjectsRepository | None = None) -> None:
        self._paths = paths.initialize()
        self._repository = repository

    def put_bytes(self, data: bytes) -> ObjectRef:
        object_hash = hashlib.sha256(data).hexdigest()
        destination = self._paths.object_path(object_hash)
        size = len(data)
        if not destination.exists():
            destination.parent.mkdir(parents=True, exist_ok=True)
            tmp_path = self._tmp_path(object_hash)
            try:
                tmp_path.write_bytes(data)
                os.replace(tmp_path, destination)
            finally:
                tmp_path.unlink(missing_ok=True)
        return self._record(object_hash, size)

    def put_file(self, source_path: str | Path) -> ObjectRef:
        source = Path(source_path).expanduser().resolve(strict=True)
        object_hash = self._hash_file(source)
        destination = self._paths.object_path(object_hash)
        size = source.stat().st_size
        if not destination.exists():
            destination.parent.mkdir(parents=True, exist_ok=True)
            tmp_path = self._tmp_path(object_hash)
            try:
                with source.open("rb") as source_file, tmp_path.open("wb") as tmp_file:
                    self._copy_stream(source_file, tmp_file)
                os.replace(tmp_path, destination)
            finally:
                tmp_path.unlink(missing_ok=True)
        return self._record(object_hash, size)

    def open_read(self, object_hash: str) -> BinaryIO:
        return self._paths.object_path(object_hash).open("rb")

    def exists(self, object_hash: str) -> bool:
        return self._paths.object_path(object_hash).exists()

    def gc(self, referenced_hashes: Collection[str]) -> ObjectGcResult:
        referenced = set(referenced_hashes)
        deleted: list[str] = []
        for object_hash, path in self._iter_object_files():
            if object_hash in referenced:
                continue
            path.unlink(missing_ok=True)
            deleted.append(object_hash)

        deleted_rows = 0
        if self._repository is not None:
            deleted_rows = self._repository.delete_unreferenced(referenced)
        return ObjectGcResult(deleted_hashes=tuple(sorted(deleted)), deleted_rows=deleted_rows)

    def _record(self, object_hash: str, size: int) -> ObjectRef:
        rel_path = f"{object_hash[:2]}/{object_hash[2:4]}/{object_hash}"
        if self._repository is not None:
            self._repository.upsert(
                object_hash=object_hash,
                rel_path=rel_path,
                size=size,
                created_at=_now_iso(),
            )
        return ObjectRef(object_hash=object_hash, rel_path=rel_path, size=size)

    def _tmp_path(self, object_hash: str) -> Path:
        return self._paths.tmp_dir / f"{object_hash}.{uuid4().hex}.tmp"

    def _hash_file(self, source: Path) -> str:
        return sha256_file(source)

    def _copy_stream(self, source: BinaryIO, destination: BinaryIO) -> None:
        for chunk in iter(lambda: source.read(CHUNK_SIZE), b""):
            destination.write(chunk)

    def _iter_object_files(self) -> Iterator[tuple[str, Path]]:
        if not self._paths.objects_dir.exists():
            return
        for path in self._paths.objects_dir.glob("*/*/*"):
            if path.is_file() and len(path.name) == 64:
                yield path.name, path


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()
