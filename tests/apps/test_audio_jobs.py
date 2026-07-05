from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from apps.worker.audio_jobs import build_asr_handler
from apps.worker.job_registry import JobExecutionError, JobExecutionResult

from contracts.asset import AssetKind, AssetSource, StorageMode
from contracts.events import ProviderCallRecorded
from contracts.jobs import Job
from contracts.provider import ProviderError, ProviderResult
from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord, VadSegment
from media.asr_upload import OssConfigError, OssUpload, OssUploadError
from media.audio_extract import ExtractedAudio
from media.vad import SileroModelMissing, VadResult
from providers import ASR_TRANSCRIBE
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import EventLogRepository, TranscriptsRepository
from storage.workspace_paths import WorkspacePaths

NOW = "2026-07-05T00:00:00+00:00"


async def test_asr_job_handler_persists_transcript_and_deletes_oss(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()
    deleted: list[str] = []

    class FakeBucket:
        def delete_object(self, key: str) -> None:
            deleted.append(key)

    class FakeGateway:
        async def call(self, request: Any, **kwargs: Any) -> ProviderGatewayResult:
            assert kwargs["require_raw_transcript"] is True
            assert kwargs["provider_id"] == "aliyun_paraformer_v2"
            assert request.payload["audio_url"] == "https://oss.example/audio.wav"
            assert request.payload["asset_id"] == "asset_1"
            return ProviderGatewayResult(
                result=ProviderResult(
                    provider_id="aliyun_paraformer_v2",
                    capability=ASR_TRANSCRIBE,
                    request_id=request.request_id,
                    model="paraformer-v2",
                    latency_ms=1,
                    normalized_output=_document().model_dump(mode="json", by_alias=True),
                )
            )

    def fake_extract(_source: Path, *, paths: WorkspacePaths) -> ExtractedAudio:
        return ExtractedAudio(path=paths.tmp_dir / "audio.wav", stderr_summary="")

    def fake_vad(_wav: Path, *, paths: WorkspacePaths) -> VadResult:
        del paths
        return VadResult(
            segments=[VadSegment(start_ms=0, end_ms=500, kind="speech")],
            speech_ratio=1.0,
        )

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        assert key_prefix == "rushes/asr/job_1"
        return OssUpload(
            bucket=FakeBucket(),
            key="key_1",
            signed_url="https://oss.example/audio.wav",
        )

    handler = build_asr_handler(
        engine,
        paths,
        gateway=FakeGateway(),
        extractor=fake_extract,
        vad_runner=fake_vad,
        uploader=fake_upload,
    )

    result = await handler(_job())

    assert isinstance(result, JobExecutionResult)
    assert result.result_json["transcript_id"] == "tr_1"
    assert result.result_json["vad_segment_count"] == 1
    assert deleted == ["key_1"]
    with engine.connect() as connection:
        row = TranscriptsRepository(connection).get("tr_1")
    assert row is not None
    assert row["utterances"][0]["words"][0]["type"] == "filler"
    assert row["vad_segments"] == [{"start_ms": 0, "end_ms": 500, "kind": "speech"}]


async def test_asr_job_handler_reports_extract_failure(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    def broken_extract(_source: Path, *, paths: WorkspacePaths) -> ExtractedAudio:
        del paths
        raise RuntimeError("ffmpeg failed")

    handler = build_asr_handler(engine, paths, gateway=_SuccessGateway(), extractor=broken_extract)

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="asset_1"))

    assert exc_info.value.error_code == "audio_extract_failed"
    assert exc_info.value.retryable is False
    assert exc_info.value.details["source_path"].endswith("source.mp4")


async def test_asr_job_handler_reports_oss_upload_failure(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        assert key_prefix == "rushes/asr/job_1"
        raise OssUploadError("upload unavailable")

    handler = build_asr_handler(
        engine,
        paths,
        gateway=_SuccessGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=fake_upload,
    )

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="asset_1"))

    assert exc_info.value.error_code == "oss_upload_failed"
    assert exc_info.value.retryable is True


async def test_asr_job_handler_reports_oss_config_failure(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        del key_prefix
        raise OssConfigError("missing config")

    handler = build_asr_handler(
        engine,
        paths,
        gateway=_SuccessGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=fake_upload,
    )

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="asset_1"))

    assert exc_info.value.error_code == "oss_config_error"
    assert exc_info.value.retryable is False


async def test_asr_job_handler_deletes_upload_when_provider_errors(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()
    deleted: list[str] = []

    class FakeBucket:
        def delete_object(self, key: str) -> None:
            deleted.append(key)

    class ErrorGateway:
        async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
            return ProviderGatewayResult(
                result=ProviderResult(
                    provider_id="aliyun_paraformer_v2",
                    capability=ASR_TRANSCRIBE,
                    request_id=request.request_id,
                    model="paraformer-v2",
                    latency_ms=1,
                    error=ProviderError(
                        error_code="asr_task_failed",
                        message="provider failed",
                        retryable=False,
                        details={"task_id": "task_1"},
                    ),
                ),
                events=(
                    ProviderCallRecorded(provider_call_id="provider_call_1").model_dump(
                        mode="json"
                    ),
                ),
            )

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        del key_prefix
        return OssUpload(bucket=FakeBucket(), key="key_1", signed_url="https://oss.example/a.wav")

    handler = build_asr_handler(
        engine,
        paths,
        gateway=ErrorGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=fake_upload,
    )

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="asset_1"))

    assert exc_info.value.error_code == "asr_task_failed"
    assert exc_info.value.details == {"task_id": "task_1"}
    assert deleted == ["key_1"]
    assert "ProviderCallRecorded" in _event_types(engine)


async def test_asr_job_handler_reports_unknown_provider_event(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()
    deleted: list[str] = []

    class FakeBucket:
        def delete_object(self, key: str) -> None:
            deleted.append(key)

    class EventGateway:
        async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
            return ProviderGatewayResult(
                result=ProviderResult(
                    provider_id="aliyun_paraformer_v2",
                    capability=ASR_TRANSCRIBE,
                    request_id=request.request_id,
                    model="paraformer-v2",
                    latency_ms=1,
                    normalized_output=_document().model_dump(mode="json", by_alias=True),
                ),
                events=({"event": "ProviderWentSideways"},),
            )

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        del key_prefix
        return OssUpload(bucket=FakeBucket(), key="key_1", signed_url="https://oss.example/a.wav")

    handler = build_asr_handler(
        engine,
        paths,
        gateway=EventGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=fake_upload,
    )

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="asset_1"))

    assert exc_info.value.error_code == "provider_event_unknown"
    assert deleted == ["key_1"]


async def test_asr_job_handler_records_vad_degradation_and_project_lookup(
    tmp_path: Path,
) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    def missing_vad(_wav: Path, *, paths: WorkspacePaths) -> VadResult:
        del paths
        raise SileroModelMissing("Silero model missing")

    handler = build_asr_handler(
        engine,
        paths,
        gateway=_SuccessGateway(),
        extractor=_fake_extract,
        vad_runner=missing_vad,
        uploader=_fake_upload,
    )

    result = await handler(_job(project_id=None, asset_id="asset_1"))

    assert result.result_json["vad_segment_count"] == 0
    assert "CapabilityDegraded" in _event_types(engine)
    with engine.connect() as connection:
        row = TranscriptsRepository(connection).get("tr_1")
    assert row is not None
    assert row["vad_segments"] == []


async def test_asr_job_handler_returns_cleanup_error_without_failing_job(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    class BrokenDeleteBucket:
        def delete_object(self, _key: str) -> None:
            raise RuntimeError("delete failed")

    def fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
        del key_prefix
        return OssUpload(
            bucket=BrokenDeleteBucket(),
            key="key_1",
            signed_url="https://oss.example/a.wav",
        )

    handler = build_asr_handler(
        engine,
        paths,
        gateway=_SuccessGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=fake_upload,
    )

    result = await handler(_job(asset_id="asset_1"))

    assert result.result_json["cleanup_error"] == "delete failed"
    with engine.connect() as connection:
        assert TranscriptsRepository(connection).get("tr_1") is not None


async def test_asr_job_handler_reports_invalid_provider_transcript(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()

    class InvalidTranscriptGateway:
        async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
            return ProviderGatewayResult(
                result=ProviderResult(
                    provider_id="aliyun_paraformer_v2",
                    capability=ASR_TRANSCRIBE,
                    request_id=request.request_id,
                    model="paraformer-v2",
                    latency_ms=1,
                    normalized_output={"schema": "not-a-transcript"},
                )
            )

    handler = build_asr_handler(
        engine,
        paths,
        gateway=InvalidTranscriptGateway(),
        extractor=_fake_extract,
        vad_runner=_empty_vad,
        uploader=_fake_upload,
    )

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(
            _job(asset_id="asset_1", payload_json={"provider_id": "aliyun_paraformer_v2"})
        )

    assert exc_info.value.error_code == "transcript_schema_error"
    assert exc_info.value.details == {"provider_id": "aliyun_paraformer_v2"}


async def test_asr_job_handler_requires_existing_asset(tmp_path: Path) -> None:
    engine = _engine_with_asset(tmp_path)
    paths = WorkspacePaths.from_root(tmp_path).initialize()
    handler = build_asr_handler(engine, paths, gateway=_SuccessGateway())

    with pytest.raises(JobExecutionError) as exc_info:
        await handler(_job(asset_id="missing_asset"))

    assert exc_info.value.error_code == "asset_not_found"
    assert exc_info.value.details == {"asset_id": "missing_asset"}


def _engine_with_asset(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
        connection.execute(
            schema.cases.insert().values(
                case_id="case_1",
                project_id="project_1",
                name="Case",
                state_version=0,
                status="active",
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief='{"goal": "test", "confirmed_facts": []}',
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
        connection.execute(
            schema.assets.insert().values(
                asset_id="asset_1",
                storage_mode=StorageMode.REFERENCE.value,
                object_hash=None,
                reference_path=str(tmp_path / "source.mp4"),
                kind=AssetKind.VIDEO.value,
                source=AssetSource.LOCAL_PATH.value,
                filename="source.mp4",
                hash="hash",
                mtime=1,
                size=1,
                probe=None,
                proxy_object_hash=None,
                ingest_status="imported",
                annotation_status="pending",
                annotation_pass="none",
                index_status="none",
                usable=True,
                failure=None,
            )
        )
        connection.execute(
            schema.project_asset_links.insert().values(
                project_id="project_1",
                asset_id="asset_1",
                enabled=True,
                linked_at=NOW,
                note="",
            )
        )
    return engine


def _job(**overrides: Any) -> Job:
    values: dict[str, Any] = {
        "job_id": "job_1",
        "kind": "asr",
        "status": "running",
        "project_id": "project_1",
        "case_id": "case_1",
        "requested_by_case_id": "case_1",
        "asset_id": None,
        "idempotency_key": "idem",
        "payload_json": {
            "tool_name": "audio.asr_original",
            "arguments": {
                "asset_id": "asset_1",
                "provider_id": "aliyun_paraformer_v2",
            },
        },
        "attempts": 0,
        "max_retries": 2,
        "created_at": NOW,
    }
    values.update(overrides)
    return Job.model_validate(values)


class _SuccessGateway:
    async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="aliyun_paraformer_v2",
                capability=ASR_TRANSCRIBE,
                request_id=request.request_id,
                model="paraformer-v2",
                latency_ms=1,
                normalized_output=_document().model_dump(mode="json", by_alias=True),
            )
        )


class _DeleteBucket:
    def delete_object(self, _key: str) -> None:
        return None


def _fake_extract(_source: Path, *, paths: WorkspacePaths) -> ExtractedAudio:
    return ExtractedAudio(path=paths.tmp_dir / "audio.wav", stderr_summary="")


def _empty_vad(_wav: Path, *, paths: WorkspacePaths) -> VadResult:
    del paths
    return VadResult(segments=[], speech_ratio=0.0)


def _fake_upload(_path: Path, *, key_prefix: str) -> OssUpload:
    del key_prefix
    return OssUpload(bucket=_DeleteBucket(), key="key_1", signed_url="https://oss.example/a.wav")


def _event_types(engine: Any) -> list[str]:
    with engine.connect() as connection:
        return [row.event_type for row in EventLogRepository(connection).read_after(0)]


def _document() -> TranscriptDocument:
    return TranscriptDocument(
        transcript_id="tr_1",
        asset_id="asset_1",
        language="zh",
        provider_id="aliyun_paraformer_v2",
        raw_preserved=True,
        utterances=[
            TranscriptUtterance(
                utterance_id="u_001",
                text="呃",
                start_ms=0,
                end_ms=100,
                words=[TranscriptWord(w="呃", start_ms=0, end_ms=100, type="filler")],
            )
        ],
    )
