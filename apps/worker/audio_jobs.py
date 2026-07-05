"""Audio job handlers."""

from __future__ import annotations

import os
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any, Protocol

from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.events import CapabilityDegraded, DomainEventBase, event_registry
from contracts.jobs import Job
from contracts.provider import ProviderResult
from contracts.transcript import TranscriptDocument, VadSegment
from media.asr_upload import OssConfigError, OssUpload, OssUploadError, upload_audio_to_oss
from media.audio_extract import ExtractedAudio, extract_audio_to_wav
from media.vad import SileroModelMissing, VadResult, run_silero_vad
from providers import ASR_TRANSCRIBE, ProviderCallRecord, ProviderGateway, ProviderRegistry
from providers.aliyun import AliyunParaformerASRProvider, aliyun_paraformer_asr_descriptor
from providers.capabilities import ProviderRequest
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import begin_immediate
from storage.repositories import ProviderCallsRepository, TranscriptsRepository
from storage.workspace_paths import WorkspacePaths, resolve_asset_path

from .job_registry import JobExecutionError, JobExecutionResult, JobHandler


class _Gateway(Protocol):
    def call(
        self,
        request: ProviderRequest,
        *,
        provider_id: str | None = None,
        require_raw_transcript: bool = False,
    ) -> Awaitable[ProviderGatewayResult]:
        """ProviderGateway-compatible call shape."""


class _Uploader(Protocol):
    def __call__(self, audio_path: str | Path, *, key_prefix: str) -> OssUpload:
        """Upload an audio file and return a cleanup handle."""


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


def build_asr_handler(
    engine: Engine,
    paths: WorkspacePaths,
    *,
    gateway: _Gateway | None = None,
    extractor: Callable[..., ExtractedAudio] = extract_audio_to_wav,
    vad_runner: Callable[..., VadResult] = run_silero_vad,
    uploader: _Uploader = upload_audio_to_oss,
) -> JobHandler:
    provider_gateway = gateway or build_default_asr_gateway(engine)

    async def _handler(job: Job) -> JobExecutionResult:
        asset_id = _job_asset_id(job)
        project_id = job.project_id or _project_id_for_asset(engine, asset_id)
        source_path = _asset_source_path(engine, paths, asset_id)
        extracted = _extract_audio(extractor, source_path, paths=paths)
        vad_segments: list[VadSegment] = []
        warnings: list[str] = []
        try:
            vad = vad_runner(extracted.path, paths=paths)
            vad_segments = list(vad.segments)
        except SileroModelMissing as exc:
            warnings.append(str(exc))
            _apply_many_or_raise(
                engine,
                (
                    CapabilityDegraded(
                        degradation_id=f"degraded_{job.job_id}_silero_vad",
                        project_id=project_id,
                        case_id=job.case_id,
                        capability="audio.vad",
                        provider_id="silero_onnx",
                        reason=str(exc),
                        payload={"asset_id": asset_id, "job_id": job.job_id},
                    ),
                ),
            )
        upload: OssUpload | None = None
        cleanup_error: str | None = None
        try:
            upload = _upload_audio(uploader, extracted.path, job=job)
            gateway_result = await provider_gateway.call(
                ProviderRequest(
                    capability=ASR_TRANSCRIBE,
                    request_id=f"asr_{job.job_id}",
                    payload={"audio_url": upload.signed_url, "asset_id": asset_id},
                    case_id=job.case_id,
                    job_id=job.job_id,
                    metadata={"asset_id": asset_id},
                ),
                provider_id=_job_payload_str(job, "provider_id"),
                require_raw_transcript=True,
            )
        finally:
            if upload is not None:
                try:
                    upload.delete()
                except Exception as exc:
                    cleanup_error = str(exc)
        _apply_event_dicts_or_raise(engine, gateway_result.events)
        result = gateway_result.result
        if result.error is not None:
            raise JobExecutionError(
                result.error.message,
                error_code=result.error.error_code,
                retryable=result.error.retryable,
                details=result.error.details,
            )
        transcript = _transcript_from_result(result)
        if vad_segments:
            transcript = transcript.model_copy(update={"vad_segments": vad_segments})
        if warnings:
            transcript = transcript.model_copy(
                update={"warnings": [*transcript.warnings, *warnings]}
            )
        with begin_immediate(engine) as connection:
            TranscriptsRepository(connection).insert_document(transcript)
        result_json = {
            "asset_id": asset_id,
            "transcript_id": transcript.transcript_id,
            "provider_id": transcript.provider_id,
            "raw_preserved": transcript.raw_preserved,
            "vad_segment_count": len(transcript.vad_segments),
        }
        if cleanup_error is not None:
            result_json["cleanup_error"] = cleanup_error
        return JobExecutionResult(result_json)

    return _handler


def build_default_asr_gateway(engine: Engine) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        aliyun_paraformer_asr_descriptor(),
        AliyunParaformerASRProvider(api_key=os.environ.get("RUSHES_DASHSCOPE_API_KEY")),
    )
    return ProviderGateway(registry=registry, recorder=StorageProviderCallRecorder(engine))


def _extract_audio(
    extractor: Callable[..., ExtractedAudio],
    source_path: Path,
    *,
    paths: WorkspacePaths,
) -> ExtractedAudio:
    try:
        return extractor(source_path, paths=paths)
    except Exception as exc:
        raise JobExecutionError(
            str(exc),
            error_code="audio_extract_failed",
            retryable=False,
            details={"source_path": str(source_path)},
        ) from exc


def _upload_audio(uploader: _Uploader, path: Path, *, job: Job) -> OssUpload:
    try:
        return uploader(path, key_prefix=f"rushes/asr/{job.job_id}")
    except OssConfigError as exc:
        raise JobExecutionError(
            str(exc),
            error_code="oss_config_error",
            retryable=False,
        ) from exc
    except OssUploadError as exc:
        raise JobExecutionError(
            str(exc),
            error_code="oss_upload_failed",
            retryable=True,
        ) from exc


def _transcript_from_result(result: ProviderResult) -> TranscriptDocument:
    try:
        return TranscriptDocument.model_validate(result.normalized_output)
    except Exception as exc:
        raise JobExecutionError(
            "provider returned invalid TranscriptDocument",
            error_code="transcript_schema_error",
            retryable=False,
            details={"provider_id": result.provider_id},
        ) from exc


def _job_asset_id(job: Job) -> str:
    if job.asset_id is not None:
        return job.asset_id
    asset_id = _job_payload_str(job, "asset_id")
    if asset_id is None:
        raise JobExecutionError(
            "asr job requires asset_id",
            error_code="invalid_asr_job",
            retryable=False,
        )
    return asset_id


def _asset_source_path(engine: Engine, paths: WorkspacePaths, asset_id: str) -> Path:
    with engine.connect() as connection:
        try:
            return resolve_asset_path(asset_id, connection=connection, paths=paths)
        except FileNotFoundError as exc:
            raise JobExecutionError(
                str(exc),
                error_code="asset_not_found",
                retryable=False,
                details={"asset_id": asset_id},
            ) from exc


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


def _apply_event_dicts_or_raise(engine: Engine, events: tuple[dict[str, Any], ...]) -> None:
    if not events:
        return
    registry = event_registry()
    parsed: list[DomainEventBase] = []
    for event in events:
        event_type = str(event.get("event", ""))
        event_class = registry.get(event_type)
        if event_class is None:
            raise JobExecutionError(
                f"unknown provider event: {event_type}",
                error_code="provider_event_unknown",
                retryable=False,
            )
        parsed.append(event_class.model_validate(event))
    _apply_many_or_raise(engine, tuple(parsed))


def _apply_many_or_raise(engine: Engine, events: tuple[DomainEventBase, ...]) -> None:
    if not events:
        return
    result = apply(events, engine=engine, base_version=None, actor="job")
    if result.status != "applied":
        raise JobExecutionError(
            f"reducer rejected audio job events: {result.status}",
            error_code="audio_job_reducer_rejected",
            retryable=True,
        )


def _payload_str(payload: dict[str, Any], key: str) -> str | None:
    value = payload.get(key)
    return value if isinstance(value, str) and value else None


def _job_payload_str(job: Job, key: str) -> str | None:
    value = _payload_str(job.payload_json, key)
    if value is not None:
        return value
    arguments = job.payload_json.get("arguments")
    if isinstance(arguments, dict):
        return _payload_str(arguments, key)
    return None
