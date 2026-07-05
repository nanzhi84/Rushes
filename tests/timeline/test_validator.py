from __future__ import annotations

from pathlib import Path
from typing import Any

from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.timeline import TimelineState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import (
    build_timeline_invariant_hook,
    validate_timeline,
    validate_timeline_invariants,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_validator_rejects_primary_visual_gap(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [
                _clip("tc_1", 0, 30),
                _clip("tc_2", 40, 60, source_start=40, source_end=60),
            ],
            duration_frames=60,
        ),
    )

    assert _codes(report) == {"timeline.primary_visual.gap"}


def test_validator_rejects_primary_visual_overlap(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [
                _clip("tc_1", 0, 40),
                _clip("tc_2", 30, 60, source_start=30, source_end=60),
            ],
            duration_frames=60,
        ),
    )

    assert "timeline.primary_visual.overlap" in _codes(report)


def test_validator_rejects_source_range_out_of_asset_bounds(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1", probe={"duration_sec": 1.0, "fps": 30})
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline([_clip("tc_1", 0, 40, source_start=0, source_end=40)], duration_frames=40),
    )

    assert "timeline.source_range.out_of_bounds" in _codes(report)


def test_validator_rejects_hard_quality_event_overlap(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(
            connection,
            "asset_1",
            hard_events=[
                {
                    "event_id": "q_1",
                    "kind": "blur",
                    "severity": "hard",
                    "start_frame": 10,
                    "end_frame": 20,
                }
            ],
        )
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline([_clip("tc_1", 0, 30)], duration_frames=30),
    )

    assert "timeline.source_range.hard_quality_overlap" in _codes(report)


def test_validator_rejects_unusable_or_disabled_asset_reference(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1", usable=False)
    report = validate_timeline(
        engine,
        _case_state(disabled_asset_ids=["asset_1"]),
        _timeline([_clip("tc_1", 0, 30)], duration_frames=30),
    )

    assert {
        "timeline.asset_reference.unusable",
        "timeline.asset_reference.case_disabled",
    } <= _codes(report)


def test_validator_rejects_fps_mismatch(tmp_path: Path) -> None:
    engine = _engine(tmp_path, fps=24)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline([_clip("tc_1", 0, 30)], duration_frames=30, fps=30),
    )

    assert "timeline.fps.mismatch" in _codes(report)


def test_validator_rejects_identity_mismatch_negative_duration_and_visual_overrun(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [_clip("tc_1", 0, 30)],
            duration_frames=-1,
            case_id="other_case",
        ),
    )

    assert {
        "timeline.identity.case_mismatch",
        "timeline.duration.negative",
        "timeline.primary_visual.overrun",
    } <= _codes(report)


def test_validator_rejects_dangling_subtitle_binding(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    timeline = _timeline(
        [_clip("tc_1", 0, 30)],
        duration_frames=30,
        subtitles=[
            {
                "timeline_clip_id": "sub_1",
                "track_id": "subtitles",
                "text": "hello",
                "timeline_start_frame": 0,
                "timeline_end_frame": 30,
                "style_template_id": "subtitle_default",
                "binding": {"kind": "voiceover", "utterance_id": "u_1"},
                "safe_area_check": "ok",
            }
        ],
    )
    report = validate_timeline(engine, _case_state(), timeline)

    assert "timeline.subtitle.binding_missing" in _codes(report)


def test_validator_rejects_audio_and_subtitle_out_of_bounds(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
        _seed_asset_with_annotation(connection, "asset_vo")
    report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [_clip("tc_1", 0, 30)],
            duration_frames=30,
            voiceover=[_audio_clip("vo_1", "voiceover", 25, 40, asset_id="asset_vo")],
            subtitles=[
                {
                    "timeline_clip_id": "sub_1",
                    "track_id": "subtitles",
                    "text": "late",
                    "timeline_start_frame": -5,
                    "timeline_end_frame": 5,
                    "style_template_id": "subtitle_default",
                    "binding": {"kind": "manual"},
                    "safe_area_check": "ok",
                }
            ],
        ),
    )

    assert {
        "timeline.audio.out_of_bounds",
        "timeline.subtitle.out_of_bounds",
        "timeline.range.negative",
    } <= _codes(report)


def test_validator_accepts_resolved_subtitle_binding_variants(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
        _seed_asset_with_annotation(connection, "asset_vo")
        _seed_asset_with_annotation(connection, "asset_orig")
    timeline = _timeline(
        [_clip("tc_1", 0, 30)],
        duration_frames=30,
        voiceover=[
            _audio_clip(
                "vo_1",
                "voiceover",
                0,
                30,
                asset_id="asset_vo",
                effects=[{"narration_ref": {"utterance_ids": ["u_1"]}}],
            ),
            _audio_clip(
                "vo_2",
                "voiceover",
                0,
                30,
                asset_id="asset_vo",
                effects=[{"narration_ref": {"utterance_id": "u_2"}}],
            ),
        ],
        original_audio=[
            _audio_clip(
                "orig_1",
                "original_audio",
                0,
                30,
                asset_id="asset_orig",
                effects=[{"utterance_id": "u_3"}],
            )
        ],
        subtitles=[
            _subtitle("sub_manual", 0, 5, {"kind": "manual"}),
            _subtitle("sub_vo_any", 0, 5, {"kind": "voiceover"}),
            _subtitle("sub_vo_list", 0, 5, {"kind": "voiceover", "utterance_id": "u_1"}),
            _subtitle("sub_vo_one", 0, 5, {"kind": "voiceover", "utterance_id": "u_2"}),
            _subtitle("sub_orig", 20, 25, {"kind": "original_audio", "utterance_id": "u_3"}),
        ],
    )

    report = validate_timeline(engine, _case_state(), timeline)

    assert "timeline.subtitle.binding_missing" not in _codes(report)


def test_validator_rejects_missing_and_unlinked_asset_references(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    missing_report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [_clip("tc_1", 0, 30, asset_id="missing_asset")],
            duration_frames=30,
        ),
    )

    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_unlinked", link_enabled=False)
    unlinked_report = validate_timeline(
        engine,
        _case_state(),
        _timeline(
            [_clip("tc_1", 0, 30, asset_id="asset_unlinked")],
            duration_frames=30,
        ),
    )

    assert "timeline.asset_reference.missing_or_unlinked" in _codes(missing_report)
    assert "timeline.asset_reference.unlinked" in _codes(unlinked_report)


def test_validator_preserves_warnings_and_hook_formats_errors(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_asset_with_annotation(connection, "asset_1")
    timeline = _timeline(
        [_clip("tc_1", 10, 30, source_start=10, source_end=30)],
        duration_frames=30,
        validation_report={
            "valid": True,
            "checks": [
                {
                    "code": "timeline.materialize.short_clean_span",
                    "severity": "warning",
                    "message": "fixture warning",
                    "details": {"slot_id": "slot_1"},
                },
                {
                    "code": "ignored.error",
                    "severity": "error",
                    "message": "old error",
                    "details": {},
                },
            ],
        },
    )

    report = validate_timeline(engine, _case_state(), timeline)
    with engine.connect() as connection:
        invariant_errors = validate_timeline_invariants(connection, _case_state(), timeline)
        hook_errors = build_timeline_invariant_hook()(connection, _case_state(), timeline)

    assert report.valid is False
    assert report.checks[0]["code"] == "timeline.materialize.short_clean_span"
    assert all("ignored.error" not in item for item in invariant_errors)
    assert invariant_errors == hook_errors
    assert any(error.startswith("timeline.primary_visual.gap:") for error in invariant_errors)


def _engine(tmp_path: Path, *, fps: int = 30):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.projects.insert().values(
                project_id="project_1",
                name="Project",
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": fps}),
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _case_state(*, disabled_asset_ids: list[str] | None = None) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
            "brief": {"goal": "test", "confirmed_facts": []},
            "selected_asset_ids": [],
            "disabled_asset_ids": disabled_asset_ids or [],
            "scratch_memory": {},
        }
    )


def _timeline(
    visual_clips: list[dict[str, Any]],
    *,
    duration_frames: int,
    fps: int = 30,
    case_id: str = "case_1",
    original_audio: list[dict[str, Any]] | None = None,
    voiceover: list[dict[str, Any]] | None = None,
    bgm: list[dict[str, Any]] | None = None,
    subtitles: list[dict[str, Any]] | None = None,
    validation_report: dict[str, Any] | None = None,
) -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": "case_1:v1",
            "case_id": case_id,
            "version": 1,
            "fps": fps,
            "duration_frames": duration_frames,
            "tracks": [
                {"track_id": "visual_base", "track_type": "primary_visual", "clips": visual_clips},
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {
                    "track_id": "original_audio",
                    "track_type": "audio",
                    "clips": original_audio or [],
                },
                {"track_id": "voiceover", "track_type": "audio", "clips": voiceover or []},
                {"track_id": "bgm", "track_type": "audio", "clips": bgm or []},
                {"track_id": "subtitles", "track_type": "text", "clips": subtitles or []},
            ],
            "validation_report": validation_report,
        }
    )


def _clip(
    timeline_clip_id: str,
    start: int,
    end: int,
    *,
    asset_id: str = "asset_1",
    source_start: int | None = None,
    source_end: int | None = None,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "visual_base",
        "asset_id": asset_id,
        "clip_id": "clip_1",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": start if source_start is None else source_start,
        "source_end_frame": end if source_end is None else source_end,
    }


def _audio_clip(
    timeline_clip_id: str,
    track_id: str,
    start: int,
    end: int,
    *,
    asset_id: str,
    effects: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": track_id,
        "asset_id": asset_id,
        "clip_id": None,
        "role": track_id,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": 0,
        "source_end_frame": max(1, end - start),
        "effects": effects or [],
    }


def _subtitle(
    timeline_clip_id: str,
    start: int,
    end: int,
    binding: dict[str, Any],
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "subtitles",
        "text": timeline_clip_id,
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "style_template_id": "subtitle_default",
        "binding": binding,
        "safe_area_check": "ok",
    }


def _seed_asset_with_annotation(
    connection: Connection,
    asset_id: str,
    *,
    usable: bool = True,
    link_enabled: bool = True,
    probe: dict[str, Any] | None = None,
    hard_events: list[dict[str, Any]] | None = None,
) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}.mp4",
            kind="video",
            source="local_path",
            filename=f"{asset_id}.mp4",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json(probe or {"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            annotation_status="completed",
            annotation_pass="cheap",
            index_status="ready",
            usable=usable,
            failure=None,
        )
    )
    connection.execute(
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=link_enabled,
            linked_at=NOW,
            note="",
        )
    )
    document = {
        "schema": "AnnotationDocument.v1",
        "annotation_id": f"ann_{asset_id}",
        "asset_id": asset_id,
        "asset_kind": "video",
        "status": "completed",
        "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
        "clips": [],
        "quality_events": hard_events or [],
        "created_at": NOW,
    }
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=f"ann_{asset_id}",
            asset_id=asset_id,
            schema="AnnotationDocument.v1",
            status="completed",
            document_json=dump_json(document),
            created_at=NOW,
            updated_at=NOW,
        )
    )


def _codes(report: Any) -> set[str]:
    return {
        str(check["code"])
        for check in report.checks
        if isinstance(check, dict) and check.get("severity") == "error"
    }
