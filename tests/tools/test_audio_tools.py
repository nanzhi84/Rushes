from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from contracts.asset import AssetKind, AssetProbe, AssetSource, StorageMode
from contracts.case import CaseState
from contracts.project import ProjectState
from media import probe as media_probe
from media import vad as media_vad
from media.audio_extract import ExtractedAudio
from storage import schema
from storage.db import create_workspace_engine
from tools import ToolExecutionContext
from tools.audio import asr_original, inspect_sources
from tools.specs import AudioAsrOriginalInput, AudioInspectSourcesInput

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


def _context(
    workspace: Path,
    *,
    connection: Any | None = None,
    audio_mode: str = "rough_cut",
) -> ToolExecutionContext:
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
                "audio_plan": {
                    "mode": audio_mode,
                    "source_asset_ids": ["asset_1"],
                },
                "selected_asset_ids": ["asset_1"],
                "disabled_asset_ids": [],
                "scratch_memory": {},
            }
        ),
        readonly_connection=connection,
        metadata={"workspace_path": workspace},
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
