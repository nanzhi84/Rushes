"""Workspace path layout helpers for the Rushes local workspace."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from sqlalchemy import select
from sqlalchemy.engine import Connection

from . import schema


@dataclass(frozen=True, slots=True)
class WorkspacePaths:
    """Resolved paths for the §3.3 workspace layout."""

    root: Path
    db_path: Path
    objects_dir: Path
    cache_dir: Path
    segments_dir: Path
    tmp_dir: Path
    logs_dir: Path

    @classmethod
    def from_root(cls, root: str | Path) -> WorkspacePaths:
        resolved_root = Path(root).expanduser().resolve(strict=False)
        cache_dir = resolved_root / "cache"
        return cls(
            root=resolved_root,
            db_path=resolved_root / "rushes.db",
            objects_dir=resolved_root / "objects",
            cache_dir=cache_dir,
            segments_dir=cache_dir / "segments",
            tmp_dir=resolved_root / "tmp",
            logs_dir=resolved_root / "logs",
        )

    def initialize(self) -> WorkspacePaths:
        """Create the workspace directories that are expected before runtime starts."""

        self.root.mkdir(parents=True, exist_ok=True)
        self.objects_dir.mkdir(parents=True, exist_ok=True)
        self.segments_dir.mkdir(parents=True, exist_ok=True)
        self.tmp_dir.mkdir(parents=True, exist_ok=True)
        self.logs_dir.mkdir(parents=True, exist_ok=True)
        return self

    def object_path(self, object_hash: str) -> Path:
        """Return objects/ab/cd/<sha256> for a validated SHA-256 hex digest."""

        if len(object_hash) != 64:
            raise ValueError("object_hash must be a 64-character SHA-256 hex digest")
        return self.objects_dir / object_hash[:2] / object_hash[2:4] / object_hash


def resolve_asset_path(
    asset_id: str,
    *,
    connection: Connection,
    paths: WorkspacePaths,
) -> Path:
    """Resolve an AssetRecord source path without exposing copy/reference details."""

    row = connection.execute(
        select(
            schema.assets.c.storage_mode,
            schema.assets.c.object_hash,
            schema.assets.c.reference_path,
        ).where(schema.assets.c.asset_id == asset_id)
    ).first()
    if row is None:
        raise FileNotFoundError(f"asset not found: {asset_id}")
    values = row._mapping
    storage_mode = values["storage_mode"]
    if storage_mode == "reference":
        reference_path = values["reference_path"]
        if not isinstance(reference_path, str) or reference_path == "":
            raise FileNotFoundError(f"reference asset has no path: {asset_id}")
        return Path(reference_path).expanduser().resolve(strict=False)
    if storage_mode == "copy":
        object_hash = values["object_hash"]
        if not isinstance(object_hash, str):
            raise FileNotFoundError(f"copy asset has no object hash: {asset_id}")
        return paths.object_path(object_hash)
    raise ValueError(f"unsupported storage_mode for {asset_id}: {storage_mode}")


def parse_workspace(root: str | Path, *, initialize: bool = False) -> WorkspacePaths:
    paths = WorkspacePaths.from_root(root)
    if initialize:
        paths.initialize()
    return paths
