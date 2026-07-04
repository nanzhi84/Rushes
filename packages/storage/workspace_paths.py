"""Workspace path layout helpers for the Rushes local workspace."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path


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


def parse_workspace(root: str | Path, *, initialize: bool = False) -> WorkspacePaths:
    paths = WorkspacePaths.from_root(root)
    if initialize:
        paths.initialize()
    return paths
