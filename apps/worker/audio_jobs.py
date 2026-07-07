"""Audio job handlers."""

from __future__ import annotations

import os
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

from sqlalchemy import select
from sqlalchemy.engine import Engine

from agent_harness.reducer import apply
from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.case import AudioPlan, CaseState, CutPlan
from contracts.events import (
    AssetImported,
    AssetLinked,
    AudioPlanUpdated,
    CapabilityDegraded,
    CutPlanUpdated,
    DomainEventBase,
    event_registry,
)
from contracts.jobs import Job
from contracts.provider import ProviderResult
from contracts.transcript import TranscriptDocument, VadSegment
from media.align import VoiceoverAlignment, align_script_to_transcript
from media.asr_upload import OssConfigError, OssUpload, OssUploadError, upload_audio_to_oss
from media.audio_extract import ExtractedAudio, extract_audio_to_wav
from media.vad import SileroModelMissing, VadResult, run_silero_vad
from providers import (
    ASR_TRANSCRIBE,
    TTS_SPEECH,
    ProviderCallRecord,
    ProviderGateway,
    ProviderRegistry,
)
from providers.aliyun import AliyunParaformerASRProvider, aliyun_paraformer_asr_descriptor
from providers.capabilities import ProviderRequest
from providers.gateway import ProviderGatewayResult
from providers.volcengine import VolcengineTTSProvider, volcengine_tts_descriptor
from storage import schema
from storage.db import begin_immediate
from storage.object_store import ObjectStore
from storage.repositories import ObjectsRepository, ProviderCallsRepository, TranscriptsRepository
from storage.repositories._json import load_json
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


@dataclass(frozen=True, slots=True)
class _ContentSlot:
    slot_id: str
    narration: str
    brief: str


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


def build_tts_handler(
    engine: Engine,
    paths: WorkspacePaths,
    *,
    gateway: _Gateway | None = None,
    extractor: Callable[..., ExtractedAudio] = extract_audio_to_wav,
    uploader: _Uploader = upload_audio_to_oss,
) -> JobHandler:
    provider_gateway = gateway or build_default_tts_gateway(engine)

    async def _handler(job: Job) -> JobExecutionResult:
        case_state = _job_case_state(engine, job)
        slots = _content_slots(case_state.content_plan)
        if not slots:
            raise JobExecutionError(
                "audio.generate_tts requires content_plan.slots with narration text",
                error_code="invalid_content_plan",
                retryable=False,
            )
        text = "\n".join(slot.narration for slot in slots)
        tts_result = await provider_gateway.call(
            ProviderRequest(
                capability=TTS_SPEECH,
                request_id=f"tts_{job.job_id}",
                payload={
                    "text": text,
                    "voice_type": _job_payload_str(job, "voice_type"),
                },
                case_id=case_state.case_id,
                job_id=job.job_id,
            ),
            provider_id=_job_payload_str(job, "provider_id"),
        )
        _apply_event_dicts_or_raise(engine, tts_result.events)
        if tts_result.result.error is not None:
            raise JobExecutionError(
                tts_result.result.error.message,
                error_code=tts_result.result.error.error_code,
                retryable=tts_result.result.error.retryable,
                details=tts_result.result.error.details,
            )
        audio_bytes = _tts_audio_bytes(tts_result.result)
        if not audio_bytes:
            raise JobExecutionError(
                "TTS provider returned no audio bytes",
                error_code="tts_audio_missing",
                retryable=False,
            )
        voiceover_asset_id = (
            _job_payload_str(job, "voiceover_asset_id") or f"asset_{job.job_id}_tts"
        )
        object_ref = _store_audio_bytes(engine, paths, audio_bytes)
        audio_path = _write_tmp_audio(paths, job.job_id, audio_bytes, suffix=".mp3")
        extracted = _extract_audio(extractor, audio_path, paths=paths)
        transcript = await _asr_fallback_transcript(
            engine,
            job,
            provider_gateway,
            uploader,
            extracted.path,
            asset_id=voiceover_asset_id,
            provider_id=_job_payload_str(job, "asr_provider_id"),
            request_prefix="tts_asr",
        )
        transcript = _retarget_transcript(
            transcript,
            transcript_id=f"tr_{job.job_id}_tts",
            asset_id=voiceover_asset_id,
        )
        if not _has_word_timestamps(transcript):
            raise JobExecutionError(
                "ASR fallback did not return word timestamps",
                error_code="tts_timestamp_fallback_failed",
                retryable=False,
            )
        asset_events: tuple[DomainEventBase, ...] = (
            _voiceover_asset_imported(
                job,
                asset_id=voiceover_asset_id,
                object_hash=object_ref.object_hash,
                object_size=object_ref.size,
                filename=f"{job.job_id}_voiceover.mp3",
            ),
            AssetLinked(project_id=case_state.project_id, asset_id=voiceover_asset_id),
        )
        _apply_many_or_raise(engine, asset_events)
        _replace_transcript(engine, transcript)
        cut_plan = _cut_plan_from_content_slots(slots, transcript)
        audio_plan = _audio_plan_payload(
            case_state,
            voiceover_asset_id=voiceover_asset_id,
            transcript_id=transcript.transcript_id,
            fallback_mode="tts",
        )
        plan_events: tuple[DomainEventBase, ...] = (
            AudioPlanUpdated(
                case_id=case_state.case_id,
                project_id=case_state.project_id,
                payload={"audio_plan": audio_plan},
            ),
            CutPlanUpdated(
                case_id=case_state.case_id,
                project_id=case_state.project_id,
                payload={"cut_plan": cut_plan},
            ),
        )
        _apply_many_or_raise(engine, plan_events, base_version=case_state.state_version)
        return JobExecutionResult(
            {
                "voiceover_asset_id": voiceover_asset_id,
                "transcript_id": transcript.transcript_id,
                "cut_plan_slot_count": len(cut_plan["slots"]),
                "object_hash": object_ref.object_hash,
            }
        )

    return _handler


def build_align_handler(
    engine: Engine,
    paths: WorkspacePaths,
    *,
    gateway: _Gateway | None = None,
    extractor: Callable[..., ExtractedAudio] = extract_audio_to_wav,
    uploader: _Uploader = upload_audio_to_oss,
) -> JobHandler:
    provider_gateway = gateway or build_default_asr_gateway(engine)

    async def _handler(job: Job) -> JobExecutionResult:
        case_state = _job_case_state(engine, job)
        script_text = _job_payload_str(job, "script_text")
        if script_text is None:
            raise JobExecutionError(
                "audio.align_uploaded_voiceover requires script_text",
                error_code="invalid_align_job",
                retryable=False,
            )
        asset_id = _job_payload_str(job, "asset_id")
        if asset_id is None and case_state.audio_plan is not None:
            asset_id = case_state.audio_plan.voiceover_asset_id
        if asset_id is None:
            raise JobExecutionError(
                "uploaded voiceover alignment requires a voiceover asset",
                error_code="missing_voiceover_asset",
                retryable=False,
            )
        source_path = _asset_source_path(engine, paths, asset_id)
        extracted = _extract_audio(extractor, source_path, paths=paths)
        transcript = await _asr_fallback_transcript(
            engine,
            job,
            provider_gateway,
            uploader,
            extracted.path,
            asset_id=asset_id,
            provider_id=_job_payload_str(job, "provider_id"),
            request_prefix="align_asr",
        )
        transcript = _retarget_transcript(
            transcript,
            transcript_id=f"tr_{job.job_id}_align",
            asset_id=asset_id,
        )
        if not _has_word_timestamps(transcript):
            raise JobExecutionError(
                "uploaded voiceover ASR did not return word timestamps",
                error_code="align_timestamp_missing",
                retryable=False,
            )
        alignment = align_script_to_transcript(script_text, transcript)
        if not alignment.sentences:
            raise JobExecutionError(
                "script and transcript could not be aligned",
                error_code="alignment_empty",
                retryable=False,
                details={"warnings": list(alignment.warnings)},
            )
        transcript = transcript.model_copy(
            update={"warnings": [*transcript.warnings, *alignment.warnings]}
        )
        _replace_transcript(engine, transcript)
        cut_plan = _cut_plan_from_alignment(alignment)
        audio_plan = _audio_plan_payload(
            case_state,
            voiceover_asset_id=asset_id,
            transcript_id=transcript.transcript_id,
            fallback_mode="uploaded_voiceover",
        )
        events = (
            AudioPlanUpdated(
                case_id=case_state.case_id,
                project_id=case_state.project_id,
                payload={"audio_plan": audio_plan},
            ),
            CutPlanUpdated(
                case_id=case_state.case_id,
                project_id=case_state.project_id,
                payload={"cut_plan": cut_plan},
            ),
        )
        _apply_many_or_raise(engine, events, base_version=case_state.state_version)
        return JobExecutionResult(
            {
                "voiceover_asset_id": asset_id,
                "transcript_id": transcript.transcript_id,
                "cut_plan_slot_count": len(cut_plan["slots"]),
                "warnings": list(alignment.warnings),
            }
        )

    return _handler


def build_default_asr_gateway(engine: Engine) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(
        aliyun_paraformer_asr_descriptor(),
        AliyunParaformerASRProvider(api_key=os.environ.get("RUSHES_DASHSCOPE_API_KEY")),
    )
    return ProviderGateway(registry=registry, recorder=StorageProviderCallRecorder(engine))


def build_default_tts_gateway(engine: Engine) -> ProviderGateway:
    registry = ProviderRegistry()
    registry.register(volcengine_tts_descriptor(), VolcengineTTSProvider())
    registry.register(
        aliyun_paraformer_asr_descriptor(priority=20),
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


async def _asr_fallback_transcript(
    engine: Engine,
    job: Job,
    provider_gateway: _Gateway,
    uploader: _Uploader,
    wav_path: Path,
    *,
    asset_id: str,
    provider_id: str | None,
    request_prefix: str,
) -> TranscriptDocument:
    upload: OssUpload | None = None
    try:
        upload = _upload_audio(uploader, wav_path, job=job)
        gateway_result = await provider_gateway.call(
            ProviderRequest(
                capability=ASR_TRANSCRIBE,
                request_id=f"{request_prefix}_{job.job_id}",
                payload={"audio_url": upload.signed_url, "asset_id": asset_id},
                case_id=job.case_id,
                job_id=job.job_id,
                metadata={"asset_id": asset_id, "timestamp_source": "asr_fallback"},
            ),
            provider_id=provider_id,
            require_raw_transcript=True,
        )
    finally:
        if upload is not None:
            upload.delete()
    _apply_event_dicts_or_raise(engine, gateway_result.events)
    result = gateway_result.result
    if result.error is not None:
        raise JobExecutionError(
            result.error.message,
            error_code=result.error.error_code,
            retryable=result.error.retryable,
            details=result.error.details,
        )
    return _transcript_from_result(result)


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


def _job_case_state(engine: Engine, job: Job) -> CaseState:
    if job.case_id is None:
        raise JobExecutionError(
            "audio job requires case_id",
            error_code="invalid_audio_job",
            retryable=False,
        )
    with engine.connect() as connection:
        row = connection.execute(
            select(schema.cases).where(schema.cases.c.case_id == job.case_id)
        ).first()
    if row is None:
        raise JobExecutionError(
            "case not found for audio job",
            error_code="case_not_found",
            retryable=False,
            details={"case_id": job.case_id},
        )
    values = dict(row._mapping)
    for key in (
        "running_jobs",
        "last_error",
        "brief",
        "content_plan",
        "audio_plan",
        "cut_plan",
        "postprocess_plan",
        "selected_asset_ids",
        "disabled_asset_ids",
        "scratch_memory",
    ):
        raw = values.get(key)
        if isinstance(raw, str):
            values[key] = load_json(raw)
    return CaseState.model_validate(values)


def _content_slots(content_plan: dict[str, Any] | None) -> list[_ContentSlot]:
    if not isinstance(content_plan, dict):
        return []
    raw_slots = content_plan.get("slots")
    slots: list[_ContentSlot] = []
    if isinstance(raw_slots, list):
        for index, item in enumerate(raw_slots, start=1):
            if not isinstance(item, dict):
                continue
            narration = _first_text(item, ("narration", "text", "script", "voiceover"))
            if narration is None:
                continue
            slot_id = _first_text(item, ("slot_id", "id")) or f"slot_{index:03d}"
            brief = _first_text(item, ("brief", "title")) or narration
            slots.append(_ContentSlot(slot_id=slot_id, narration=narration, brief=brief))
    if slots:
        return slots
    narration = _first_text(content_plan, ("narration", "text", "script"))
    if narration is not None:
        return [_ContentSlot(slot_id="slot_001", narration=narration, brief=narration)]
    outline = content_plan.get("outline")
    if isinstance(outline, list):
        for index, item in enumerate(outline, start=1):
            if isinstance(item, str) and item.strip():
                slots.append(
                    _ContentSlot(
                        slot_id=f"slot_{index:03d}",
                        narration=item.strip(),
                        brief=item.strip(),
                    )
                )
    return slots


def _first_text(values: dict[str, Any], keys: tuple[str, ...]) -> str | None:
    for key in keys:
        value = values.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return None


def _tts_audio_bytes(result: ProviderResult) -> bytes:
    value = result.normalized_output.get("audio_bytes")
    if isinstance(value, bytes):
        return value
    if isinstance(value, str):
        return value.encode()
    return b""


def _store_audio_bytes(engine: Engine, paths: WorkspacePaths, audio_bytes: bytes) -> Any:
    with begin_immediate(engine) as connection:
        return ObjectStore(paths, ObjectsRepository(connection)).put_bytes(audio_bytes)


def _write_tmp_audio(
    paths: WorkspacePaths,
    job_id: str,
    audio_bytes: bytes,
    *,
    suffix: str,
) -> Path:
    paths.initialize()
    paths.tmp_dir.mkdir(parents=True, exist_ok=True)
    path = paths.tmp_dir / f"{job_id}{suffix}"
    path.write_bytes(audio_bytes)
    return path


def _retarget_transcript(
    transcript: TranscriptDocument,
    *,
    transcript_id: str,
    asset_id: str,
) -> TranscriptDocument:
    payload = transcript.model_dump(mode="json", by_alias=True)
    payload["transcript_id"] = transcript_id
    payload["asset_id"] = asset_id
    return TranscriptDocument.model_validate(payload)


def _has_word_timestamps(transcript: TranscriptDocument) -> bool:
    return any(utterance.words for utterance in transcript.utterances)


def _replace_transcript(engine: Engine, transcript: TranscriptDocument) -> None:
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.transcripts.delete().where(
                schema.transcripts.c.transcript_id == transcript.transcript_id
            )
        )
        TranscriptsRepository(connection).insert_document(transcript)


def _cut_plan_from_content_slots(
    slots: list[_ContentSlot],
    transcript: TranscriptDocument,
) -> dict[str, Any]:
    utterances_by_slot = _assign_utterances_to_slots(slots, transcript)
    cut_slots: list[dict[str, Any]] = []
    for slot in slots:
        utterances = utterances_by_slot.get(slot.slot_id, [])
        start_ms, end_ms = _utterance_range(utterances)
        duration_sec = max(0.1, (end_ms - start_ms) / 1000)
        cut_slots.append(
            {
                "slot_id": slot.slot_id,
                "brief": _brief(slot.brief),
                "target_duration_sec": _duration_window(duration_sec),
                "narration_ref": {
                    "utterance_ids": [utterance.utterance_id for utterance in utterances],
                    "transcript_id": transcript.transcript_id,
                    "text": slot.narration,
                    "start_ms": start_ms,
                    "end_ms": end_ms,
                },
            }
        )
    total_duration = _transcript_duration_sec(transcript)
    return CutPlan.model_validate(
        {
            "schema": "CutPlan.v1",
            "slots": cut_slots,
            "removed_ranges": [],
            "total_target_duration_sec": total_duration,
        }
    ).model_dump(mode="json", by_alias=True)


def _assign_utterances_to_slots(
    slots: list[_ContentSlot],
    transcript: TranscriptDocument,
) -> dict[str, list[Any]]:
    slot_lengths = [max(1, len(_normalize_text(slot.narration))) for slot in slots]
    total_slot_chars = sum(slot_lengths)
    boundaries: list[tuple[str, float, float]] = []
    cursor = 0
    for slot, length in zip(slots, slot_lengths, strict=True):
        start = cursor / total_slot_chars
        cursor += length
        end = cursor / total_slot_chars
        boundaries.append((slot.slot_id, start, end))
    utterance_lengths = [max(1, len(_normalize_text(item.text))) for item in transcript.utterances]
    total_utterance_chars = max(1, sum(utterance_lengths))
    assigned: dict[str, list[Any]] = {slot.slot_id: [] for slot in slots}
    cursor = 0
    for utterance, length in zip(transcript.utterances, utterance_lengths, strict=True):
        midpoint = (cursor + length / 2) / total_utterance_chars
        cursor += length
        slot_id = boundaries[-1][0]
        for candidate_slot_id, start, end in boundaries:
            if start <= midpoint <= end:
                slot_id = candidate_slot_id
                break
        assigned[slot_id].append(utterance)
    return assigned


def _cut_plan_from_alignment(alignment: VoiceoverAlignment) -> dict[str, Any]:
    slots: list[dict[str, Any]] = []
    for sentence in alignment.sentences:
        duration_sec = max(0.1, (sentence.end_ms - sentence.start_ms) / 1000)
        slots.append(
            {
                "slot_id": sentence.sentence_id,
                "brief": _brief(sentence.text),
                "target_duration_sec": _duration_window(duration_sec),
                "narration_ref": {
                    "utterance_ids": list(sentence.utterance_ids),
                    "script_text": sentence.text,
                    "start_ms": sentence.start_ms,
                    "end_ms": sentence.end_ms,
                    "alignment_confidence": sentence.alignment_confidence,
                },
            }
        )
    first_start = min(sentence.start_ms for sentence in alignment.sentences)
    last_end = max(sentence.end_ms for sentence in alignment.sentences)
    return CutPlan.model_validate(
        {
            "schema": "CutPlan.v1",
            "slots": slots,
            "removed_ranges": [],
            "total_target_duration_sec": max(0.0, (last_end - first_start) / 1000),
        }
    ).model_dump(mode="json", by_alias=True)


def _audio_plan_payload(
    case_state: CaseState,
    *,
    voiceover_asset_id: str,
    transcript_id: str,
    fallback_mode: str,
) -> dict[str, Any]:
    payload = (
        case_state.audio_plan.model_dump(mode="json")
        if case_state.audio_plan is not None
        else {"mode": fallback_mode}
    )
    payload["voiceover_asset_id"] = voiceover_asset_id
    payload["transcript_id"] = transcript_id
    return AudioPlan.model_validate(payload).model_dump(mode="json")


def _voiceover_asset_imported(
    job: Job,
    *,
    asset_id: str,
    object_hash: str,
    object_size: int,
    filename: str,
) -> AssetImported:
    return AssetImported(
        project_id=job.project_id,
        # 素材池事件不得携带 case 作用域（§4.6-5，StateValidator 强制）；
        # 该配音与 Case 的关联由 job 事件与 cut_plan 表达。
        case_id=None,
        asset_id=asset_id,
        job_id=job.job_id,
        payload={
            "storage_mode": StorageMode.COPY.value,
            "object_hash": object_hash,
            "object_size": object_size,
            "reference_path": None,
            "kind": AssetKind.AUDIO.value,
            "source": AssetSource.UPLOAD.value,
            "filename": filename,
            "hash": object_hash,
            "mtime": time.time_ns(),
            "size": object_size,
            "probe": None,
            "proxy_object_hash": None,
            "ingest_status": "imported",
            "usable": True,
            "failure": None,
        },
    )


def _utterance_range(utterances: list[Any]) -> tuple[int, int]:
    if not utterances:
        return (0, 100)
    return (
        min(int(utterance.start_ms) for utterance in utterances),
        max(int(utterance.end_ms) for utterance in utterances),
    )


def _transcript_duration_sec(transcript: TranscriptDocument) -> float:
    if not transcript.utterances:
        return 0.0
    start_ms = min(utterance.start_ms for utterance in transcript.utterances)
    end_ms = max(utterance.end_ms for utterance in transcript.utterances)
    return max(0.0, (end_ms - start_ms) / 1000)


def _duration_window(duration_sec: float) -> tuple[float, float]:
    low = max(0.1, duration_sec * 0.9)
    high = max(low + 0.1, duration_sec * 1.1)
    return (round(low, 3), round(high, 3))


def _brief(value: str) -> str:
    stripped = value.strip()
    if len(stripped) <= 48:
        return stripped
    return stripped[:45] + "..."


def _normalize_text(value: str) -> str:
    return "".join(char.lower() for char in value if char.isalnum() or "\u4e00" <= char <= "\u9fff")


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


def _apply_many_or_raise(
    engine: Engine,
    events: tuple[DomainEventBase, ...],
    *,
    base_version: int | None = None,
) -> None:
    if not events:
        return
    result = apply(events, engine=engine, base_version=base_version, actor="job")
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
