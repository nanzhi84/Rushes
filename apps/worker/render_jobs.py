"""Render preview/final jobs."""

from __future__ import annotations

import os
import time
from collections.abc import Mapping
from pathlib import Path
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.case import CaseState
from contracts.events import DomainEventBase, ExportCompleted, JobProgress, PreviewRendered
from contracts.jobs import Job
from contracts.subtitle import SubtitleStyleTemplate
from contracts.timeline import TimelineMediaClip, TimelineState
from domain.subtitle_templates import list_subtitle_templates
from media.final_mp4 import FINAL_MP4_PROFILE, render_final_mp4
from media.preview import PREVIEW_PROFILE, render_preview
from media.render_cache import DEFAULT_MAX_BYTES, SegmentRenderCache
from media.segment_render import MediaSource, SegmentRenderError
from storage import schema
from storage.db import begin_immediate
from storage.object_store import ObjectStore
from storage.repositories import CasesRepository, ObjectsRepository
from storage.workspace_paths import WorkspacePaths, resolve_asset_path
from timeline import get_timeline_version

from .job_registry import JobExecutionError, JobExecutionResult, JobHandler


def build_render_preview_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        return await _run_render_job(engine, paths, job, final=False)

    return _handler


def build_render_final_handler(engine: Engine, paths: WorkspacePaths) -> JobHandler:
    async def _handler(job: Job) -> JobExecutionResult:
        return await _run_render_job(engine, paths, job, final=True)

    return _handler


async def _run_render_job(
    engine: Engine,
    paths: WorkspacePaths,
    job: Job,
    *,
    final: bool,
) -> JobExecutionResult:
    case_state = _load_case_state(engine, job)
    timeline_version = _timeline_version_from_job(job, case_state)
    if not case_state.timeline_validated:
        raise JobExecutionError(
            "render job requires a validated timeline",
            error_code="render_precondition_failed",
            retryable=False,
        )
    if final:
        _ensure_current_preview_exists(engine, case_state, timeline_version)
    record = get_timeline_version(engine, case_state.case_id, timeline_version)
    if record is None:
        raise JobExecutionError(
            f"timeline v{timeline_version} not found",
            error_code="render_timeline_not_found",
            retryable=False,
            details={"case_id": case_state.case_id, "timeline_version": timeline_version},
        )
    timeline = record.timeline
    sources = _timeline_sources(engine, paths, timeline)
    output_path = paths.initialize().tmp_dir / f"{job.job_id}_{'final' if final else 'preview'}.mp4"
    reporter = _JobProgressReporter(engine, job)
    try:
        render_output = (
            await render_final_mp4(
                timeline,
                sources=sources,
                paths=paths,
                output_path=output_path,
                cache=SegmentRenderCache(paths, max_bytes=_cache_max_bytes()),
                subtitle_templates=_subtitle_template_map(),
                progress_callback=reporter.emit,
            )
            if final
            else await render_preview(
                timeline,
                sources=sources,
                paths=paths,
                output_path=output_path,
                cache=SegmentRenderCache(paths, max_bytes=_cache_max_bytes()),
                subtitle_templates=_subtitle_template_map(),
                progress_callback=reporter.emit,
            )
        )
        object_ref = _put_render_output(engine, paths, render_output.output_path)
        event = _completed_event(
            job,
            case_state=case_state,
            timeline=timeline,
            object_hash=object_ref.object_hash,
            object_size=object_ref.size,
            final=final,
        )
        _apply_or_raise(engine, event)
        await reporter.emit(1.0, force=True)
        return JobExecutionResult(
            {
                "case_id": case_state.case_id,
                "timeline_version": timeline.version,
                "artifact_id": event.artifact_id,
                "object_hash": object_ref.object_hash,
                "quality": "final_mp4" if final else "preview",
                "segments": [
                    {
                        "segment_id": item.segment.segment_id,
                        "cache_key": item.cache_key,
                        "cache_hit": item.cache_hit,
                    }
                    for item in render_output.rendered_segments
                ],
            }
        )
    except SegmentRenderError as exc:
        raise JobExecutionError(
            str(exc),
            error_code="render_failed",
            retryable=False,
            stderr_summary=exc.stderr_summary,
            details={"case_id": case_state.case_id, "timeline_version": timeline_version},
        ) from exc
    finally:
        output_path.unlink(missing_ok=True)


class _JobProgressReporter:
    def __init__(self, engine: Engine, job: Job, *, min_interval_seconds: float = 1.0) -> None:
        self._engine = engine
        self._job = job
        self._min_interval_seconds = min_interval_seconds
        self._last_emit = float("-inf")

    async def emit(self, progress: float, *, force: bool = False) -> None:
        now = time.monotonic()
        value = max(0.0, min(1.0, progress))
        if not force and value < 1.0 and now - self._last_emit < self._min_interval_seconds:
            return
        self._last_emit = now
        event = JobProgress(
            job_id=self._job.job_id,
            project_id=self._job.project_id,
            case_id=self._job.case_id,
            requested_by_case_id=self._job.requested_by_case_id,
            progress=value,
            payload={"kind": self._job.kind, "progress": value},
        )
        apply(
            (event,),
            engine=self._engine,
            base_version=None,
            actor="job",
        )


def _load_case_state(engine: Engine, job: Job) -> CaseState:
    case_id = job.case_id or job.requested_by_case_id or _payload_case_id(job.payload_json)
    if case_id is None:
        raise JobExecutionError(
            "render job requires case_id",
            error_code="invalid_render_job",
            retryable=False,
        )
    with engine.connect() as connection:
        row = CasesRepository(connection).get(case_id)
    if row is None:
        raise JobExecutionError(
            f"case not found: {case_id}",
            error_code="render_case_not_found",
            retryable=False,
        )
    return CaseState.model_validate(row)


def _payload_case_id(payload: Mapping[str, Any]) -> str | None:
    value = payload.get("case_id")
    if isinstance(value, str):
        return value
    arguments = payload.get("arguments")
    if isinstance(arguments, Mapping):
        argument_value = arguments.get("case_id")
        if isinstance(argument_value, str):
            return argument_value
    return None


def _timeline_version_from_job(job: Job, case_state: CaseState) -> int:
    payload = job.payload_json
    arguments = payload.get("arguments")
    raw_version = payload.get("timeline_version")
    if raw_version is None and isinstance(arguments, Mapping):
        raw_version = arguments.get("timeline_version")
    if raw_version is None:
        raw_version = case_state.timeline_current_version
    if not isinstance(raw_version, int):
        raise JobExecutionError(
            "render job requires timeline_version",
            error_code="invalid_render_job",
            retryable=False,
        )
    return raw_version


def _ensure_current_preview_exists(
    engine: Engine,
    case_state: CaseState,
    timeline_version: int,
) -> None:
    preview_id = case_state.preview_current_id
    if preview_id is None:
        raise JobExecutionError(
            "final export requires preview for current timeline version",
            error_code="render_precondition_failed",
            retryable=False,
        )
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.previews.c.timeline_version).where(
                schema.previews.c.preview_id == preview_id,
                schema.previews.c.case_id == case_state.case_id,
            )
        ).first()
    if row is None or int(row._mapping["timeline_version"]) != timeline_version:
        raise JobExecutionError(
            "final export requires preview for current timeline version",
            error_code="render_precondition_failed",
            retryable=False,
        )


def _timeline_sources(
    engine: Engine,
    paths: WorkspacePaths,
    timeline: TimelineState,
) -> dict[str, MediaSource]:
    asset_ids = sorted(_timeline_asset_ids(timeline))
    if not asset_ids:
        return {}
    with engine.connect() as connection:
        rows = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id.in_(asset_ids))
        ).all()
        rows_by_asset = {str(row._mapping["asset_id"]): dict(row._mapping) for row in rows}
        sources: dict[str, MediaSource] = {}
        for asset_id in asset_ids:
            row = rows_by_asset.get(asset_id)
            if row is None:
                raise JobExecutionError(
                    f"asset not found for render: {asset_id}",
                    error_code="render_asset_not_found",
                    retryable=False,
                )
            sources[asset_id] = MediaSource(
                asset_id=asset_id,
                path=resolve_asset_path(asset_id, connection=connection, paths=paths),
                asset_hash=str(row["hash"]),
                kind=str(row["kind"]),
            )
    return sources


def _timeline_asset_ids(timeline: TimelineState) -> set[str]:
    asset_ids: set[str] = set()
    for track in timeline.tracks:
        for clip in track.clips:
            if isinstance(clip, TimelineMediaClip):
                asset_ids.add(clip.asset_id)
    return asset_ids


def _subtitle_template_map() -> dict[str, SubtitleStyleTemplate]:
    return {template.template_id: template for template in list_subtitle_templates()}


def _put_render_output(engine: Engine, paths: WorkspacePaths, output_path: Path) -> Any:
    with begin_immediate(engine) as connection:
        return ObjectStore(paths, ObjectsRepository(connection)).put_file(output_path)


def _completed_event(
    job: Job,
    *,
    case_state: CaseState,
    timeline: TimelineState,
    object_hash: str,
    object_size: int,
    final: bool,
) -> PreviewRendered | ExportCompleted:
    prefix = "export" if final else "preview"
    artifact_id = f"{prefix}_{case_state.case_id}_v{timeline.version}_{object_hash[:12]}"
    payload = {
        "object_hash": object_hash,
        "object_size": object_size,
        "timeline_version": timeline.version,
        "quality": FINAL_MP4_PROFILE.cache_payload() if final else PREVIEW_PROFILE.cache_payload(),
        "job_id": job.job_id,
    }
    if final:
        return ExportCompleted(
            project_id=case_state.project_id,
            case_id=case_state.case_id,
            timeline_version=timeline.version,
            artifact_id=artifact_id,
            payload=payload,
        )
    return PreviewRendered(
        project_id=case_state.project_id,
        case_id=case_state.case_id,
        timeline_version=timeline.version,
        artifact_id=artifact_id,
        payload=payload,
    )


def _apply_or_raise(engine: Engine, event: DomainEventBase) -> None:
    result = apply((event,), engine=engine, base_version=None, actor="job")
    if result.status != "applied":
        raise JobExecutionError(
            f"reducer rejected render event: {result.status}",
            error_code="render_reducer_rejected",
            retryable=True,
        )


def _cache_max_bytes() -> int:
    raw = os.environ.get("RUSHES_RENDER_CACHE_MAX_BYTES")
    if raw is None:
        return DEFAULT_MAX_BYTES
    try:
        value = int(raw)
    except ValueError:
        return DEFAULT_MAX_BYTES
    return max(0, value)
