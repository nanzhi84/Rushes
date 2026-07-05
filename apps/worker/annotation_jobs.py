"""Annotation job handler."""

from __future__ import annotations

import os
from collections.abc import Mapping
from pathlib import Path
from typing import Any, Literal

from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from annotation.pipelines.audio import run_audio_annotation
from annotation.pipelines.bgm import run_bgm_annotation
from annotation.pipelines.image import run_image_annotation
from annotation.pipelines.video import run_video_annotation
from annotation.projection import build_annotation_projection, persist_annotation_projection
from contracts.annotation import AnnotationDocument
from contracts.events import (
    AnnotationCompleted,
    AnnotationFailed,
    CapabilityDegraded,
    DomainEventBase,
)
from contracts.jobs import Job
from providers import ProviderCallRecord, ProviderGateway, ProviderRegistry
from providers.openai_compatible import (
    OpenAICompatibleEmbeddingProvider,
    OpenAICompatibleVLMProvider,
    openai_compatible_embedding_descriptor,
    openai_compatible_vlm_descriptor,
)
from storage import schema
from storage.db import begin_immediate
from storage.repositories import ProviderCallsRepository
from storage.repositories._json import load_json
from storage.workspace_paths import WorkspacePaths, resolve_asset_path

from .job_registry import JobExecutionError, JobExecutionResult, JobHandler


class StorageProviderCallRecorder:
    def __init__(self, engine: Engine) -> None:
        self._engine = engine

    def record_provider_call(self, record: ProviderCallRecord) -> None:
        with begin_immediate(self._engine) as connection:
            ProviderCallsRepository(connection).insert(
                {
                    "call_id": record.call_id,
                    "provider_id": record.provider_id,
                    "capability": record.capability,
                    "model": record.model,
                    "case_id": record.case_id,
                    "job_id": record.job_id,
                    "latency_ms": record.latency_ms,
                    "usage_json": record.usage_json,
                    "cost_estimate": record.cost_estimate,
                    "status": record.status,
                }
            )


def build_annotation_handler(
    engine: Engine,
    paths: WorkspacePaths,
    *,
    gateway: ProviderGateway | None = None,
) -> JobHandler:
    provider_gateway = gateway or build_default_annotation_gateway(engine)

    async def _handler(job: Job) -> JobExecutionResult:
        pass_ = _annotation_pass(job)
        asset = _asset_row(engine, job)
        asset_id = str(asset["asset_id"])
        project_id = job.project_id or _project_id_for_asset(engine, asset_id)
        if project_id is not None:
            _raise_if_budget_exceeded(engine, job, project_id)
        try:
            document, events = await _run_pipeline(
                engine,
                paths,
                job,
                asset,
                pass_=pass_,
                gateway=provider_gateway,
            )
            projection = await build_annotation_projection(
                document,
                gateway=provider_gateway,
                job_id=job.job_id,
                case_id=job.case_id,
            )
            usable = any(row.usable for row in projection.clips)
            with begin_immediate(engine) as connection:
                persist_annotation_projection(connection, document, projection)
            _apply_many_or_raise(
                engine,
                (
                    *(_event_from_dict(event) for event in events),
                    AnnotationCompleted(
                        project_id=project_id,
                        case_id=job.case_id,
                        asset_id=asset_id,
                        job_id=job.job_id,
                        annotation_id=document.annotation_id,
                        payload={
                            "annotation_pass": document.generator.pass_,
                            "index_status": "ready",
                            "usable": usable,
                        },
                    ),
                ),
            )
            return JobExecutionResult(
                {
                    "asset_id": asset_id,
                    "annotation_id": document.annotation_id,
                    "annotation_pass": document.generator.pass_,
                    "clip_count": len(document.clips),
                }
            )
        except JobExecutionError:
            raise
        except NotImplementedError as exc:
            _mark_annotation_failed(
                engine,
                job,
                asset_id=asset_id,
                project_id=project_id,
                error_code="annotation_pipeline_not_implemented",
                message=str(exc),
                retryable=False,
            )
            raise JobExecutionError(
                str(exc),
                error_code="annotation_pipeline_not_implemented",
                retryable=False,
                details={"asset_id": asset_id, "pass": pass_},
            ) from exc
        except Exception as exc:
            _mark_annotation_failed(
                engine,
                job,
                asset_id=asset_id,
                project_id=project_id,
                error_code="annotation_failed",
                message=str(exc),
                retryable=True,
            )
            raise JobExecutionError(
                str(exc),
                error_code="annotation_failed",
                retryable=True,
                details={"asset_id": asset_id, "pass": pass_},
            ) from exc

    return _handler


def build_default_annotation_gateway(engine: Engine) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        openai_compatible_vlm_descriptor(),
        OpenAICompatibleVLMProvider(api_key=os.environ.get("RUSHES_VLM_API_KEY")),
    )
    registry.register(
        openai_compatible_embedding_descriptor(),
        OpenAICompatibleEmbeddingProvider(api_key=os.environ.get("RUSHES_EMBEDDING_API_KEY")),
    )
    return ProviderGateway(registry=registry, recorder=StorageProviderCallRecorder(engine))


async def _run_pipeline(
    engine: Engine,
    paths: WorkspacePaths,
    job: Job,
    asset: Mapping[str, Any],
    *,
    pass_: Literal["cheap", "deep"],
    gateway: ProviderGateway,
) -> tuple[AnnotationDocument, tuple[dict[str, Any], ...]]:
    asset_id = str(asset["asset_id"])
    kind = str(asset["kind"])
    path = _annotation_source_path(engine, paths, asset_id, asset)
    if kind == "video":
        existing = _existing_document(engine, asset_id) if pass_ == "deep" else None
        result = await run_video_annotation(
            path,
            asset_id=asset_id,
            gateway=gateway,
            pass_="deep" if pass_ == "deep" else "cheap",
            existing_document=existing,
            job_id=job.job_id,
            case_id=job.case_id,
        )
        return result.document, result.events
    if kind == "image":
        document = await run_image_annotation(
            path,
            asset_id=asset_id,
            gateway=gateway,
            job_id=job.job_id,
            case_id=job.case_id,
        )
        return document, ()
    if kind in {"audio", "voiceover"}:
        run_audio_annotation(path, asset_id=asset_id)
    if kind == "bgm":
        run_bgm_annotation(path, asset_id=asset_id)
    raise NotImplementedError(f"annotation pipeline for asset kind {kind} is not implemented")


def _annotation_source_path(
    engine: Engine,
    paths: WorkspacePaths,
    asset_id: str,
    asset: Mapping[str, Any],
) -> Path:
    proxy_hash = asset.get("proxy_object_hash")
    if isinstance(proxy_hash, str) and proxy_hash:
        return paths.object_path(proxy_hash)
    with engine.connect() as connection:
        return resolve_asset_path(asset_id, connection=connection, paths=paths)


def _asset_row(engine: Engine, job: Job) -> Mapping[str, Any]:
    asset_id = job.asset_id
    if asset_id is None:
        payload_asset_id = job.payload_json.get("asset_id")
        asset_id = payload_asset_id if isinstance(payload_asset_id, str) else None
    if asset_id is None:
        raise JobExecutionError(
            "annotation job requires asset_id",
            error_code="invalid_annotation_job",
            retryable=False,
        )
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.assets).where(schema.assets.c.asset_id == asset_id)
        ).first()
    if row is None:
        raise JobExecutionError(
            f"asset not found: {asset_id}",
            error_code="asset_not_found",
            retryable=False,
        )
    return dict(row._mapping)


def _project_id_for_asset(engine: Engine, asset_id: str) -> str | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.project_asset_links.c.project_id)
            .where(schema.project_asset_links.c.asset_id == asset_id)
            .order_by(schema.project_asset_links.c.linked_at.desc())
            .limit(1)
        ).first()
    if row is None:
        return None
    return str(row._mapping["project_id"])


def _existing_document(engine: Engine, asset_id: str) -> AnnotationDocument | None:
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.annotations_table.c.document_json)
            .where(schema.annotations_table.c.asset_id == asset_id)
            .order_by(schema.annotations_table.c.updated_at.desc())
            .limit(1)
        ).first()
    if row is None:
        return None
    return AnnotationDocument.model_validate(load_json(str(row._mapping["document_json"])))


def _annotation_pass(job: Job) -> Literal["cheap", "deep"]:
    value = job.payload_json.get("pass", "cheap")
    if value not in {"cheap", "deep"}:
        raise JobExecutionError(
            "annotation pass must be cheap or deep",
            error_code="invalid_annotation_pass",
            retryable=False,
        )
    return "deep" if value == "deep" else "cheap"


def _raise_if_budget_exceeded(engine: Engine, job: Job, project_id: str) -> None:
    with engine.connect() as connection:
        project = connection.execute(
            select(schema.projects.c.defaults).where(schema.projects.c.project_id == project_id)
        ).first()
        budget = None
        if project is not None:
            defaults = load_json(str(project._mapping["defaults"]))
            if isinstance(defaults, Mapping):
                raw_budget = defaults.get("annotation_budget_cny")
                if raw_budget is not None:
                    budget = float(raw_budget)
        spent = ProviderCallsRepository(connection).sum_cost_for_project(project_id)
    if budget is None or spent < budget:
        return
    asset_id = job.asset_id or _payload_asset_id(job)
    _apply_many_or_raise(
        engine,
        (
            CapabilityDegraded(
                degradation_id=f"degraded_{job.job_id}_budget",
                project_id=project_id,
                case_id=job.case_id,
                capability="vlm.annotation",
                reason="annotation budget exceeded",
                payload={"budget_cny": budget, "spent_cny": spent, "job_id": job.job_id},
            ),
            AnnotationFailed(
                project_id=project_id,
                case_id=job.case_id,
                asset_id=asset_id or "",
                job_id=job.job_id,
                payload={
                    "failure": {
                        "error_code": "budget_exceeded",
                        "message": "annotation budget exceeded",
                        "retryable": False,
                    }
                },
            ),
        ),
    )
    raise JobExecutionError(
        "annotation budget exceeded",
        error_code="budget_exceeded",
        retryable=False,
        details={"project_id": project_id, "budget_cny": budget, "spent_cny": spent},
    )


def _mark_annotation_failed(
    engine: Engine,
    job: Job,
    *,
    asset_id: str,
    project_id: str | None,
    error_code: str,
    message: str,
    retryable: bool,
) -> None:
    _apply_many_or_raise(
        engine,
        (
            AnnotationFailed(
                project_id=project_id,
                case_id=job.case_id,
                asset_id=asset_id,
                job_id=job.job_id,
                payload={
                    "failure": {
                        "error_code": error_code,
                        "message": message,
                        "retryable": retryable,
                    }
                },
            ),
        ),
    )


def _apply_many_or_raise(engine: Engine, events: tuple[DomainEventBase, ...]) -> None:
    result = apply(events, engine=engine, base_version=None, actor="job")
    if result.status != "applied":
        raise JobExecutionError(
            f"reducer rejected annotation events: {result.status}",
            error_code="annotation_reducer_rejected",
            retryable=True,
        )


def _event_from_dict(event: dict[str, Any]) -> DomainEventBase:
    from events.event_log import validate_domain_event

    return validate_domain_event(event)


def _payload_asset_id(job: Job) -> str | None:
    value = job.payload_json.get("asset_id")
    return value if isinstance(value, str) else None
