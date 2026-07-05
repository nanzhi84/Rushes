from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from agent_harness.reducer import apply
from contracts.asset import AssetKind, AssetProbe, AssetSource, StorageMode
from contracts.case import CaseState
from contracts.events import DecisionAnswered
from contracts.project import ProjectState
from contracts.provider import ProviderError, ProviderResult
from contracts.transcript import TranscriptDocument, TranscriptUtterance, TranscriptWord, VadSegment
from media import probe as media_probe
from media import vad as media_vad
from media.audio_extract import ExtractedAudio
from providers import LLM_CHAT
from providers.gateway import ProviderGatewayResult
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import CasesRepository, TranscriptsRepository
from tools import ToolExecutionContext
from tools.audio import (
    align_uploaded_voiceover,
    asr_original,
    generate_tts,
    inspect_sources,
    rough_cut_speech,
)
from tools.specs import (
    AudioAlignUploadedVoiceoverInput,
    AudioAsrOriginalInput,
    AudioGenerateTtsInput,
    AudioInspectSourcesInput,
    AudioRoughCutSpeechInput,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_audio_inspect_sources_degrades_when_silero_model_missing(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    engine = _engine_with_asset(tmp_path)

    def fake_probe(_path: Path) -> Any:
        return AssetProbe(duration_sec=10.0, fps=30.0, width=1920, height=1080, has_audio=True)

    def fake_extract(_path: Path, *, paths: Any) -> ExtractedAudio:
        return ExtractedAudio(path=paths.tmp_dir / "audio.wav", stderr_summary="")

    def fake_vad(_path: Path, *, paths: Any) -> Any:
        raise media_vad.SileroModelMissing("missing model")

    monkeypatch.setattr(media_probe, "probe_media", fake_probe)
    monkeypatch.setattr("media.audio_extract.extract_audio_to_wav", fake_extract)
    monkeypatch.setattr(media_vad, "run_silero_vad", fake_vad)
    with engine.connect() as connection:
        result = inspect_sources(
            AudioInspectSourcesInput(),
            _context(tmp_path, connection=connection),
        )

    assert result.status == "succeeded"
    assert result.data["sources"][0]["has_audio"] is True
    assert result.data["sources"][0]["speech_ratio"] is None
    assert result.data["degraded"][0]["capability"] == "audio.vad"
    assert [event["event"] for event in result.events] == ["AssetProbed", "CapabilityDegraded"]


def test_audio_asr_original_queues_asr_job() -> None:
    result = asr_original(AudioAsrOriginalInput(), _context(Path("/tmp")))

    assert result.status == "running"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "asr"
    assert result.events[0]["payload"]["job_payload"]["asset_id"] == "asset_1"
    assert result.data["asset_id"] == "asset_1"


def test_audio_asr_original_rejects_tts_mode() -> None:
    context = _context(Path("/tmp"), audio_mode="tts")

    result = asr_original(AudioAsrOriginalInput(asset_id="asset_1"), context)

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "audio_mode_not_supported"


def test_rough_cut_speech_creates_decision_without_mutating_cut_plan(tmp_path: Path) -> None:
    engine = _engine_with_case_and_transcript(tmp_path, _rough_cut_document())
    with engine.connect() as connection:
        result = rough_cut_speech(
            AudioRoughCutSpeechInput(filler_words=["呃"]),
            _context(tmp_path, connection=connection, cut_plan=_initial_cut_plan()),
        )

    assert result.status == "requires_user"
    assert result.events[-1]["event"] == "DecisionCreated"
    assert result.data["rough_cut_proposal"]
    with engine.connect() as connection:
        case_row = CasesRepository(connection).get("case_1")
    assert case_row is not None
    assert case_row["cut_plan"]["removed_ranges"] == []

    created = apply([result.events[-1]], engine=engine, base_version=0, actor="agent")
    assert created.status == "applied"
    with engine.connect() as connection:
        case_after_decision = CasesRepository(connection).get("case_1")
    assert case_after_decision is not None
    assert case_after_decision["pending_decision_id"] == result.data["decision_id"]
    assert case_after_decision["cut_plan"]["removed_ranges"] == []

    answered = apply(
        [
            DecisionAnswered(
                decision_id=str(result.data["decision_id"]),
                scope_type="case",
                project_id="project_1",
                case_id="case_1",
                payload={
                    "answer": {
                        "option_id": "apply_all",
                        "answered_via": "button",
                    }
                },
            )
        ],
        engine=engine,
        base_version=1,
        actor="user",
    )
    assert answered.status == "applied"
    with engine.connect() as connection:
        case_after_answer = CasesRepository(connection).get("case_1")
    assert case_after_answer is not None
    assert case_after_answer["cut_plan"]["removed_ranges"]


def test_rough_cut_speech_raw_not_preserved_degrades_to_pause_and_repeat(
    tmp_path: Path,
) -> None:
    engine = _engine_with_case_and_transcript(
        tmp_path,
        _rough_cut_document(raw_preserved=False),
    )
    with engine.connect() as connection:
        result = rough_cut_speech(
            AudioRoughCutSpeechInput(filler_words=["呃"]),
            _context(tmp_path, connection=connection, cut_plan=_initial_cut_plan()),
        )

    kinds = {proposal["kind"] for proposal in result.data["rough_cut_proposal"]}
    assert result.status == "requires_user"
    assert kinds == {"pause", "repeat"}
    assert [event["event"] for event in result.events] == [
        "CapabilityDegraded",
        "DecisionCreated",
    ]
    assert result.events[0]["capability"] == "audio.rough_cut_speech.filler_detection"


def test_rough_cut_speech_uses_mocked_llm_structured_output(tmp_path: Path) -> None:
    engine = _engine_with_case_and_transcript(tmp_path, _rough_cut_document())
    with engine.connect() as connection:
        result = rough_cut_speech(
            AudioRoughCutSpeechInput(),
            _context(
                tmp_path,
                connection=connection,
                cut_plan=_initial_cut_plan(),
                metadata={"llm_gateway": _StructuredLlmGateway()},
            ),
        )

    semantic = [
        proposal
        for proposal in result.data["rough_cut_proposal"]
        if proposal["kind"] == "off_topic"
    ]
    assert len(semantic) == 1
    assert semantic[0]["range_ms"] == {"start_ms": 600, "end_ms": 1600}


def test_rough_cut_speech_degrades_when_llm_provider_errors(tmp_path: Path) -> None:
    engine = _engine_with_case_and_transcript(tmp_path, _rough_cut_document())
    with engine.connect() as connection:
        result = rough_cut_speech(
            AudioRoughCutSpeechInput(),
            _context(
                tmp_path,
                connection=connection,
                cut_plan=_initial_cut_plan(),
                metadata={"llm_gateway": _ErrorLlmGateway()},
            ),
        )

    degraded = [event for event in result.events if event["event"] == "CapabilityDegraded"]
    assert degraded
    assert degraded[0]["capability"] == "audio.rough_cut_speech.semantic"
    assert "llm_down" in degraded[0]["reason"]


def test_rough_cut_speech_requires_transcript_with_vad(tmp_path: Path) -> None:
    engine = _engine_with_case_and_transcript(
        tmp_path,
        _rough_cut_document(vad_segments=[]),
    )
    with engine.connect() as connection:
        result = rough_cut_speech(
            AudioRoughCutSpeechInput(),
            _context(tmp_path, connection=connection),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "transcript_missing"


def test_audio_tts_and_align_tools_enqueue_jobs() -> None:
    tts_result = generate_tts(
        AudioGenerateTtsInput(provider_id="volcengine", asr_provider_id="asr", voice_type="voice"),
        _context(Path("/tmp"), audio_mode="tts", content_plan={"slots": [{"narration": "你好"}]}),
    )
    align_result = align_uploaded_voiceover(
        AudioAlignUploadedVoiceoverInput(script_text="你好。", asset_id="asset_vo"),
        _context(Path("/tmp"), audio_mode="uploaded_voiceover", voiceover_asset_id="asset_vo"),
    )

    assert tts_result.status == "running"
    assert tts_result.events[0]["payload"]["kind"] == "tts"
    assert align_result.status == "running"
    assert align_result.events[0]["payload"]["kind"] == "align"


def test_audio_tts_and_align_tools_require_case() -> None:
    bare = ToolExecutionContext(tool_call_id="tc_x", turn_id="turn_x")

    tts_result = generate_tts(AudioGenerateTtsInput(), bare)
    align_result = align_uploaded_voiceover(
        AudioAlignUploadedVoiceoverInput(script_text="你好。"),
        bare,
    )

    assert tts_result.status == "failed"
    assert align_result.status == "failed"
    assert tts_result.error is not None
    assert tts_result.error.error_code == "missing_case"
    assert align_result.error is not None
    assert align_result.error.error_code == "missing_case"


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


def _engine_with_case_and_transcript(
    tmp_path: Path,
    document: TranscriptDocument,
) -> Any:
    engine = _engine_with_asset(tmp_path)
    with engine.begin() as connection:
        CasesRepository(connection).insert(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "state_version": 0,
                "status": "active",
                "pending_decision_id": None,
                "running_jobs": [],
                "last_error": None,
                "brief": {"goal": "test", "confirmed_facts": []},
                "content_plan": None,
                "audio_plan": {
                    "mode": "rough_cut",
                    "source_asset_ids": ["asset_1"],
                    "transcript_id": document.transcript_id,
                },
                "cut_plan": _initial_cut_plan(),
                "candidate_pack_id": None,
                "timeline_current_version": None,
                "timeline_validated": False,
                "preview_current_id": None,
                "last_viewed_preview_id": None,
                "rough_cut_approved": False,
                "rough_cut_approved_version": None,
                "postprocess_plan": None,
                "export_current_id": None,
                "selected_asset_ids": ["asset_1"],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        )
        TranscriptsRepository(connection).insert_document(document)
    return engine


def _context(
    workspace: Path,
    *,
    connection: Any | None = None,
    audio_mode: str = "rough_cut",
    content_plan: dict[str, Any] | None = None,
    cut_plan: dict[str, Any] | None = None,
    voiceover_asset_id: str | None = None,
    metadata: dict[str, Any] | None = None,
) -> ToolExecutionContext:
    audio_plan: dict[str, Any] = {
        "mode": audio_mode,
        "source_asset_ids": ["asset_1"],
        "transcript_id": "tr_rough",
    }
    if voiceover_asset_id is not None:
        audio_plan["voiceover_asset_id"] = voiceover_asset_id
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        project_state=ProjectState.model_validate(
            {
                "project_id": "project_1",
                "name": "Project",
                "status": "active",
                "asset_links": [],
                "case_ids": ["case_1"],
                "memory_ids": [],
                "created_at": NOW,
                "updated_at": NOW,
            }
        ),
        case_state=CaseState.model_validate(
            {
                "case_id": "case_1",
                "project_id": "project_1",
                "name": "Case",
                "brief": {"goal": "test", "confirmed_facts": []},
                "content_plan": content_plan,
                "audio_plan": audio_plan,
                "cut_plan": cut_plan,
                "selected_asset_ids": ["asset_1"],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        ),
        readonly_connection=connection,
        metadata={"workspace_path": workspace, **(metadata or {})},
    )


def test_asr_original_failure_branches(tmp_path: Path) -> None:
    bare = ToolExecutionContext(tool_call_id="tc_x", turn_id="turn_x")
    assert asr_original(AudioAsrOriginalInput(), bare).status == "failed"

    from dataclasses import replace

    context = _context(tmp_path)
    case_without_plan = context.case_state.model_copy(update={"audio_plan": None})
    try:
        stripped = replace(context, case_state=case_without_plan)
    except TypeError:
        stripped = ToolExecutionContext(
            tool_call_id="tc_1",
            turn_id="turn_1",
            project_state=context.project_state,
            case_state=case_without_plan,
        )
    result = asr_original(AudioAsrOriginalInput(), stripped)
    assert result.status == "failed"
    assert result.error is not None
    assert "audio_plan" in str(result.error)


def test_inspect_sources_requires_case(tmp_path: Path) -> None:
    bare = ToolExecutionContext(tool_call_id="tc_x", turn_id="turn_x")
    result = inspect_sources(AudioInspectSourcesInput(), bare)
    assert result.status == "failed"


def _initial_cut_plan() -> dict[str, Any]:
    return {
        "schema": "CutPlan.v1",
        "slots": [
            {
                "slot_id": "slot_001",
                "brief": "intro",
                "target_duration_sec": (1.0, 2.0),
                "narration_ref": None,
            }
        ],
        "removed_ranges": [],
        "total_target_duration_sec": 2.0,
    }


def _rough_cut_document(
    *,
    raw_preserved: bool = True,
    vad_segments: list[VadSegment] | None = None,
) -> TranscriptDocument:
    return TranscriptDocument(
        transcript_id="tr_rough",
        asset_id="asset_1",
        language="zh",
        provider_id="asr",
        raw_preserved=raw_preserved,
        utterances=[
            TranscriptUtterance(
                utterance_id="u_001",
                text="呃 我们开始",
                start_ms=0,
                end_ms=500,
                words=[
                    TranscriptWord(w="呃", start_ms=0, end_ms=100, type="filler"),
                    TranscriptWord(w="我们开始", start_ms=100, end_ms=500, type="word"),
                ],
            ),
            TranscriptUtterance(
                utterance_id="u_002",
                text="这个产品适合新手",
                start_ms=600,
                end_ms=1600,
                words=[
                    TranscriptWord(
                        w="这个产品适合新手",
                        start_ms=600,
                        end_ms=1600,
                        type="word",
                    )
                ],
            ),
            TranscriptUtterance(
                utterance_id="u_003",
                text="这个产品适合新手这个产品适合新手",
                start_ms=1600,
                end_ms=2600,
                words=[
                    TranscriptWord(
                        w="这个产品适合新手这个产品适合新手",
                        start_ms=1600,
                        end_ms=2600,
                        type="word",
                    )
                ],
            ),
        ],
        vad_segments=(
            vad_segments
            if vad_segments is not None
            else [
                VadSegment(start_ms=0, end_ms=2600, kind="speech"),
                VadSegment(start_ms=2600, end_ms=3400, kind="silence"),
            ]
        ),
    )


class _StructuredLlmGateway:
    async def call(self, request: Any, **kwargs: Any) -> ProviderGatewayResult:
        assert request.capability == LLM_CHAT
        assert kwargs["provider_id"] is None
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_llm",
                capability=LLM_CHAT,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                normalized_output={
                    "tool_call": {
                        "function": {
                            "arguments": (
                                '{"suggestions":[{"utterance_id":"u_002",'
                                '"reason":"off topic","confidence":0.91}]}'
                            )
                        }
                    }
                },
            )
        )


class _ErrorLlmGateway:
    async def call(self, request: Any, **_kwargs: Any) -> ProviderGatewayResult:
        return ProviderGatewayResult(
            result=ProviderResult(
                provider_id="mock_llm",
                capability=LLM_CHAT,
                request_id=request.request_id,
                model="mock",
                latency_ms=1,
                error=ProviderError(
                    error_code="llm_down",
                    message="provider unavailable",
                    retryable=True,
                ),
            )
        )
