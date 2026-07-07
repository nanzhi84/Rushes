"""Cheap local index job: cover frame, shots, VAD, peaks, and font metadata.

The index is best-effort and never blocks asset usability: any failure emits an
``AssetIndexFailed`` event and the job still succeeds (Spec C §C1).
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path
from typing import Any
from uuid import uuid4

from sqlalchemy.engine import Engine

from contracts.asset import AssetKind
from contracts.events import AssetIndexFailed, AssetIndexReady
from contracts.jobs import Job
from media.font_meta import read_font_meta
from media.probe import probe_media
from media.shots import split_shots
from media.thumbnails import extract_video_thumbnail, render_image_thumbnail
from media.vad import SileroModelMissing, default_model_path, run_silero_vad
from media.waveform import compute_waveform_peaks
from storage.object_store import ObjectStore
from storage.workspace_paths import WorkspacePaths

from .job_registry import JobExecutionResult, JobHandler
from .media_jobs import _apply_or_raise, _asset_source, _job_asset_id


def build_index_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        try:
            source_path, kind = _asset_source(engine, paths, asset_id)
            index_json, thumbnail_object_hash = _build_index(kind, source_path, paths)
        except Exception as exc:  # cheap best-effort index degrades, never blocks the asset
            _apply_or_raise(
                engine,
                AssetIndexFailed(
                    draft_id=job.draft_id,
                    asset_id=asset_id,
                    payload={
                        "failure": {"error_code": "index_failed", "message": str(exc)},
                    },
                ),
            )
            return JobExecutionResult({"asset_id": asset_id, "index_status": "failed"})
        _apply_or_raise(
            engine,
            AssetIndexReady(
                draft_id=job.draft_id,
                asset_id=asset_id,
                payload={
                    "index_json": index_json,
                    "thumbnail_object_hash": thumbnail_object_hash,
                    "ingest_status": "indexed",
                },
            ),
        )
        return JobExecutionResult(
            {
                "asset_id": asset_id,
                "thumbnail_object_hash": thumbnail_object_hash,
                "index_status": "indexed",
            }
        )

    return _handler


def _build_index(
    kind: str,
    source_path: Path,
    paths: WorkspacePaths,
) -> tuple[dict[str, Any], str | None]:
    if kind == AssetKind.VIDEO.value:
        return _index_video(source_path, paths)
    if kind == AssetKind.AUDIO.value:
        return _index_audio(source_path, paths)
    if kind == AssetKind.IMAGE.value:
        return _index_image(source_path, paths)
    if kind == AssetKind.FONT.value:
        return _index_font(source_path)
    raise ValueError(f"unsupported asset kind for index: {kind}")


def _index_video(source_path: Path, paths: WorkspacePaths) -> tuple[dict[str, Any], str | None]:
    duration_sec = probe_media(source_path).duration_sec
    cover_sec = 1.0 if duration_sec >= 2.0 else max(0.0, duration_sec / 10.0)
    thumbnail = extract_video_thumbnail(source_path, seconds=cover_sec)
    thumbnail_object_hash = ObjectStore(paths).put_bytes(thumbnail).object_hash
    shots = split_shots(source_path)
    index_json: dict[str, Any] = {
        "duration_sec": duration_sec,
        "shots": [{"start_sec": shot.start_sec, "end_sec": shot.end_sec} for shot in shots],
    }
    return index_json, thumbnail_object_hash


def _index_audio(source_path: Path, paths: WorkspacePaths) -> tuple[dict[str, Any], str | None]:
    duration_sec = probe_media(source_path).duration_sec
    peaks = compute_waveform_peaks(source_path)
    vad = _run_vad(source_path, paths)
    index_json: dict[str, Any] = {
        "duration_sec": duration_sec,
        "vad": vad,
        "peaks": peaks,
    }
    return index_json, None


def _index_image(source_path: Path, paths: WorkspacePaths) -> tuple[dict[str, Any], str | None]:
    thumbnail = render_image_thumbnail(source_path)
    thumbnail_object_hash = ObjectStore(paths).put_bytes(thumbnail).object_hash
    return {"duration_sec": 0.0}, thumbnail_object_hash


def _index_font(source_path: Path) -> tuple[dict[str, Any], str | None]:
    meta = read_font_meta(source_path)
    index_json: dict[str, Any] = {
        "font_meta": {
            "family": meta.family,
            "style": meta.style,
            "full_name": meta.full_name,
        }
    }
    return index_json, None


def _run_vad(source_path: Path, paths: WorkspacePaths) -> list[dict[str, Any]]:
    # 模型缺失时优雅降级为空数组，且不做无谓的转码（Spec C §C1）。
    if not _vad_model_present(paths):
        return []
    wav_path = paths.tmp_dir / f"vad_{uuid4().hex}.wav"
    try:
        _transcode_wav_16k_mono(source_path, wav_path)
        try:
            result = run_silero_vad(wav_path, paths=paths)
        except SileroModelMissing:
            return []
        return [
            {"start_ms": segment.start_ms, "end_ms": segment.end_ms, "kind": segment.kind}
            for segment in result.segments
        ]
    finally:
        wav_path.unlink(missing_ok=True)


def _vad_model_present(paths: WorkspacePaths) -> bool:
    env_path = os.environ.get("RUSHES_SILERO_VAD_MODEL")
    if env_path:
        return Path(env_path).expanduser().exists()
    return default_model_path(paths).exists()


def _transcode_wav_16k_mono(
    source_path: Path,
    dest_path: Path,
    *,
    ffmpeg_bin: str = "ffmpeg",
) -> None:
    command = [
        ffmpeg_bin,
        "-y",
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        str(source_path),
        "-ac",
        "1",
        "-ar",
        "16000",
        "-c:a",
        "pcm_s16le",
        str(dest_path),
    ]
    result = subprocess.run(command, capture_output=True, check=False, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"ffmpeg wav transcode failed: {source_path}")
