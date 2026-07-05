from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from contracts.case import CaseState
from contracts.timeline import TimelineState, TimelineValidationReport
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from storage.repositories.timeline_versions import TimelineVersionsRepository
from timeline import (
    get_timeline_version,
    list_timeline_versions,
    restore_timeline_version,
    store_timeline_version,
    update_timeline_validation_report,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_timeline_versions_repository_returns_empty_results(tmp_path: Path) -> None:
    engine = _engine(tmp_path)

    with engine.connect() as connection:
        repository = TimelineVersionsRepository(connection)
        assert repository.get_by_case_version("case_1", 1) is None
        assert repository.list_for_case("case_1") == []

    assert get_timeline_version(engine, "case_1", 1) is None
    assert list_timeline_versions(engine, "case_1") == []


def test_restore_timeline_version_raises_for_missing_source(tmp_path: Path) -> None:
    engine = _engine(tmp_path)

    with pytest.raises(KeyError, match="timeline version not found: 99"):
        restore_timeline_version(engine, _case_state(), source_version=99)


def test_restore_timeline_version_can_start_new_chain_from_zero_version(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        store_timeline_version(
            connection,
            _timeline(version=0, created_by_patch_id="patch_1"),
            created_at=NOW,
        )
        restored = restore_timeline_version(
            connection,
            _case_state(timeline_current_version=None),
            source_version=0,
            created_at=NOW,
        )
    versions = list_timeline_versions(engine, "case_1")

    assert restored.timeline_id == "case_1:v1"
    assert restored.version == 1
    assert restored.parent_version is None
    assert restored.created_by_patch_id is None
    assert [record.version for record in versions] == [0, 1]


def test_store_timeline_version_skips_existing_case_version(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        store_timeline_version(
            connection,
            _timeline(version=1),
            created_at="2026-07-05T01:00:00+00:00",
        )

    versions = list_timeline_versions(engine, "case_1")

    assert len(versions) == 1
    assert versions[0].created_at == NOW


def test_update_validation_report_handles_missing_and_existing_versions(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    report = TimelineValidationReport(
        valid=False,
        checks=[
            {
                "code": "timeline.primary_visual.gap",
                "severity": "error",
                "message": "gap",
                "details": {"start_frame": 0, "end_frame": 10},
            }
        ],
    )

    with engine.begin() as connection:
        update_timeline_validation_report(connection, case_id="case_1", version=404, report=report)
        store_timeline_version(connection, _timeline(version=1), created_at=NOW)
        update_timeline_validation_report(connection, case_id="case_1", version=1, report=report)

    record = get_timeline_version(engine, "case_1", 1)

    assert record is not None
    assert record.validation_report == report
    assert record.timeline.validation_report == report


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
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
                pending_decision_id=None,
                running_jobs=dump_json([]),
                last_error=None,
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                content_plan=None,
                audio_plan=None,
                cut_plan=None,
                candidate_pack_id=None,
                timeline_current_version=None,
                timeline_validated=False,
                preview_current_id=None,
                last_viewed_preview_id=None,
                rough_cut_approved=False,
                rough_cut_approved_version=None,
                postprocess_plan=None,
                export_current_id=None,
                selected_asset_ids=dump_json([]),
                disabled_asset_ids=dump_json([]),
                scratch_memory=dump_json({}),
            )
        )
    return engine


def _case_state(*, timeline_current_version: int | None = None) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "timeline_current_version": timeline_current_version,
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _timeline(
    *,
    version: int,
    created_by_patch_id: str | None = None,
) -> TimelineState:
    payload: dict[str, Any] = {
        "timeline_id": f"case_1:v{version}",
        "case_id": "case_1",
        "version": version,
        "fps": 30,
        "duration_frames": 0,
        "created_by_patch_id": created_by_patch_id,
        "tracks": [
            {"track_id": "visual_base", "track_type": "primary_visual", "clips": []},
            {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
            {"track_id": "original_audio", "track_type": "audio", "clips": []},
            {"track_id": "voiceover", "track_type": "audio", "clips": []},
            {"track_id": "bgm", "track_type": "audio", "clips": []},
            {"track_id": "subtitles", "track_type": "text", "clips": []},
        ],
    }
    return TimelineState.model_validate(payload)
