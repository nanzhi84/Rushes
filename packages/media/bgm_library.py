"""Programmatic default BGM library backed by ffmpeg lavfi synthesis."""

from __future__ import annotations

import hashlib
import os
import subprocess
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Any
from uuid import uuid4

from sqlalchemy import select
from sqlalchemy.dialects.sqlite import insert as sqlite_insert
from sqlalchemy.engine import Connection

from contracts.asset import AssetKind, AssetSource, StorageMode
from storage import schema
from storage.object_store import ObjectStore
from storage.repositories._json import dump_json
from storage.repositories.objects import ObjectsRepository
from storage.workspace_paths import WorkspacePaths


@dataclass(frozen=True, slots=True)
class DefaultBgmTrack:
    bgm_id: str
    display_name: str
    style: str
    duration_sec: int
    synthesis: Mapping[str, Any]


class DefaultBgmSynthesisError(RuntimeError):
    """Raised when ffmpeg cannot synthesize a default BGM file."""


_DEFAULT_BGM_TRACKS: tuple[DefaultBgmTrack, ...] = (
    DefaultBgmTrack(
        bgm_id="default_bgm_calm",
        display_name="平静和弦垫",
        style="低频正弦和弦垫，适合产品说明和慢节奏旁白",
        duration_sec=24,
        synthesis=MappingProxyType({"kind": "calm", "frequencies": (130.81, 164.81, 196.00)}),
    ),
    DefaultBgmTrack(
        bgm_id="default_bgm_upbeat",
        display_name="轻快节奏",
        style="短促节奏方波并做高通，适合轻快剪辑",
        duration_sec=18,
        synthesis=MappingProxyType({"kind": "upbeat", "frequency": 220.0, "pulse_hz": 4.0}),
    ),
    DefaultBgmTrack(
        bgm_id="default_bgm_ambient",
        display_name="环境铺底",
        style="粉噪和低频正弦混合并低通，适合氛围铺底",
        duration_sec=30,
        synthesis=MappingProxyType({"kind": "ambient", "noise": "pink", "frequency": 73.42}),
    ),
)
_DEFAULT_BGM_BY_ID = MappingProxyType({track.bgm_id: track for track in _DEFAULT_BGM_TRACKS})


def list_default_bgm_tracks() -> list[DefaultBgmTrack]:
    return list(_DEFAULT_BGM_TRACKS)


def get_default_bgm_track(bgm_id: str) -> DefaultBgmTrack:
    return _DEFAULT_BGM_BY_ID[bgm_id]


def synthesize_default_bgm(
    track: DefaultBgmTrack,
    out_path: str | Path,
    *,
    ffmpeg_bin: str = "ffmpeg",
) -> None:
    destination = Path(out_path)
    if destination.exists() and destination.stat().st_size > 0:
        return
    destination.parent.mkdir(parents=True, exist_ok=True)
    suffix = destination.suffix or ".m4a"
    tmp_path = destination.with_name(f"{destination.stem}.{uuid4().hex}{suffix}")
    command = _synthesis_command(track, tmp_path, ffmpeg_bin=ffmpeg_bin)
    result = subprocess.run(command, capture_output=True, check=False, text=True)
    if result.returncode != 0:
        tmp_path.unlink(missing_ok=True)
        raise DefaultBgmSynthesisError(_stderr_summary(result.stderr))
    if not tmp_path.exists() or tmp_path.stat().st_size == 0:
        tmp_path.unlink(missing_ok=True)
        raise DefaultBgmSynthesisError("ffmpeg default BGM synthesis produced no output")
    os.replace(tmp_path, destination)


def ensure_default_bgm_asset(
    connection: Connection,
    workspace_paths: WorkspacePaths,
    project_id: str,
    bgm_id: str,
    created_at: str,
) -> str:
    track = get_default_bgm_track(bgm_id)
    asset_id = _default_asset_id(project_id, bgm_id)
    if _project_asset_link_enabled(connection, project_id, asset_id):
        return asset_id
    asset_row = connection.execute(
        select(schema.assets.c.asset_id).where(schema.assets.c.asset_id == asset_id)
    ).first()
    if asset_row is None:
        source_path = workspace_paths.cache_dir / "default_bgm" / f"{bgm_id}.m4a"
        synthesize_default_bgm(track, source_path)
        ref = ObjectStore(workspace_paths, ObjectsRepository(connection)).put_file(source_path)
        stat = source_path.stat()
        connection.execute(
            sqlite_insert(schema.assets)
            .values(
                asset_id=asset_id,
                storage_mode=StorageMode.COPY.value,
                object_hash=ref.object_hash,
                reference_path=None,
                kind=AssetKind.BGM.value,
                source=AssetSource.DEFAULT_LIBRARY.value,
                filename=f"{track.display_name}.m4a",
                hash=ref.object_hash,
                mtime=stat.st_mtime_ns,
                size=ref.size,
                probe=dump_json(
                    {
                        "duration_sec": float(track.duration_sec),
                        "has_audio": True,
                        "default_library": {"bgm_id": bgm_id},
                    }
                ),
                proxy_object_hash=None,
                ingest_status="indexed",
                annotation_status="completed",
                annotation_pass="none",
                index_status="none",
                usable=True,
                failure=None,
            )
            .on_conflict_do_nothing(index_elements=[schema.assets.c.asset_id])
        )
    _link_project_asset(connection, project_id, asset_id, created_at)
    return asset_id


def _synthesis_command(track: DefaultBgmTrack, out_path: Path, *, ffmpeg_bin: str) -> list[str]:
    kind = str(track.synthesis["kind"])
    if kind == "calm":
        return _calm_command(track, out_path, ffmpeg_bin=ffmpeg_bin)
    if kind == "upbeat":
        return _upbeat_command(track, out_path, ffmpeg_bin=ffmpeg_bin)
    if kind == "ambient":
        return _ambient_command(track, out_path, ffmpeg_bin=ffmpeg_bin)
    raise ValueError(f"unknown default BGM synthesis kind: {kind}")


def _calm_command(track: DefaultBgmTrack, out_path: Path, *, ffmpeg_bin: str) -> list[str]:
    duration = track.duration_sec
    fade_out = max(0, duration - 2)
    frequencies = tuple(float(value) for value in track.synthesis["frequencies"])
    return [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency={frequencies[0]}:duration={duration}:sample_rate=48000",
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency={frequencies[1]}:duration={duration}:sample_rate=48000",
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency={frequencies[2]}:duration={duration}:sample_rate=48000",
        "-filter_complex",
        (
            "[0:a]volume=0.14[a0];[1:a]volume=0.10[a1];[2:a]volume=0.08[a2];"
            "[a0][a1][a2]amix=inputs=3:normalize=0,"
            f"lowpass=f=1800,afade=t=in:st=0:d=1.2,afade=t=out:st={fade_out}:d=2[aout]"
        ),
        "-map",
        "[aout]",
        "-c:a",
        "aac",
        "-b:a",
        "128k",
        "-movflags",
        "+faststart",
        str(out_path),
    ]


def _upbeat_command(track: DefaultBgmTrack, out_path: Path, *, ffmpeg_bin: str) -> list[str]:
    duration = track.duration_sec
    fade_out = max(0, duration - 1)
    frequency = float(track.synthesis["frequency"])
    pulse_hz = float(track.synthesis["pulse_hz"])
    pulse = (
        f"aevalsrc=0.12*(2*gt(sin(2*PI*{frequency}*t)\\,0)-1)"
        f"*gt(sin(2*PI*{pulse_hz}*t)\\,0):d={duration}:s=48000"
    )
    return [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "lavfi",
        "-i",
        pulse,
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency=440:duration={duration}:sample_rate=48000",
        "-filter_complex",
        (
            "[0:a]highpass=f=180,volume=0.6[a0];[1:a]volume=0.035[a1];"
            "[a0][a1]amix=inputs=2:normalize=0,"
            f"afade=t=in:st=0:d=0.25,afade=t=out:st={fade_out}:d=1[aout]"
        ),
        "-map",
        "[aout]",
        "-c:a",
        "aac",
        "-b:a",
        "128k",
        "-movflags",
        "+faststart",
        str(out_path),
    ]


def _ambient_command(track: DefaultBgmTrack, out_path: Path, *, ffmpeg_bin: str) -> list[str]:
    duration = track.duration_sec
    fade_out = max(0, duration - 3)
    frequency = float(track.synthesis["frequency"])
    return [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-f",
        "lavfi",
        "-i",
        f"anoisesrc=color=pink:duration={duration}:sample_rate=48000:amplitude=0.08",
        "-f",
        "lavfi",
        "-i",
        f"sine=frequency={frequency}:duration={duration}:sample_rate=48000",
        "-filter_complex",
        (
            "[0:a]lowpass=f=950,volume=0.5[a0];[1:a]volume=0.035[a1];"
            "[a0][a1]amix=inputs=2:normalize=0,"
            f"afade=t=in:st=0:d=2,afade=t=out:st={fade_out}:d=3[aout]"
        ),
        "-map",
        "[aout]",
        "-c:a",
        "aac",
        "-b:a",
        "128k",
        "-movflags",
        "+faststart",
        str(out_path),
    ]


def _default_asset_id(project_id: str, bgm_id: str) -> str:
    digest = hashlib.sha1(project_id.encode("utf-8")).hexdigest()[:12]
    return f"asset_{bgm_id}_{digest}"


def _project_asset_link_enabled(connection: Connection, project_id: str, asset_id: str) -> bool:
    row = connection.execute(
        select(schema.project_asset_links.c.enabled)
        .where(schema.project_asset_links.c.project_id == project_id)
        .where(schema.project_asset_links.c.asset_id == asset_id)
    ).first()
    return row is not None and bool(row._mapping["enabled"])


def _link_project_asset(
    connection: Connection,
    project_id: str,
    asset_id: str,
    created_at: str,
) -> None:
    connection.execute(
        sqlite_insert(schema.project_asset_links)
        .values(
            project_id=project_id,
            asset_id=asset_id,
            enabled=True,
            linked_at=created_at,
            note="默认无版权 BGM",
        )
        .on_conflict_do_update(
            index_elements=[
                schema.project_asset_links.c.project_id,
                schema.project_asset_links.c.asset_id,
            ],
            set_={"enabled": True, "linked_at": created_at, "note": "默认无版权 BGM"},
        )
    )


def _stderr_summary(stderr: str | None) -> str:
    lines = [line.strip() for line in (stderr or "").splitlines() if line.strip()]
    return lines[-1] if lines else "ffmpeg default BGM synthesis failed"
