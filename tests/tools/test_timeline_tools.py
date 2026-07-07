from __future__ import annotations

from pathlib import Path
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from contracts.draft import DraftState
from contracts.patch import DeleteRangeOp, TimelinePatchReference, TimelinePatchRequest
from contracts.timeline import TimelineState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json, load_json
from timeline import get_timeline_version, store_timeline_version
from tools import ToolExecutionContext
from tools.specs import (
    ComposeInitialInput,
    TimelineInspectInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
)
from tools.timeline_tools import (
    apply_patch,
    compose_initial,
    restore_version,
    validate,
)
from tools.timeline_tools import (
    inspect as timeline_inspect,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_validate_reports_invalid_timeline(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1")
        timeline = _timeline(
            [
                _timeline_clip("tc_1", 0, 30),
                _timeline_clip("tc_2", 40, 60, source_start=40, source_end=60),
            ],
            duration_frames=60,
        )
        store_timeline_version(connection, timeline, created_at=NOW)
        draft_state = _draft_state(timeline_current_version=1)
        result = validate(TimelineValidateInput(), _context(connection, draft_state))

    assert result.status == "succeeded"
    assert result.data["valid"] is False
    assert result.events[-1]["event"] == "TimelineValidationFailed"


def test_compose_initial_builds_valid_timeline_and_bumps_version(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1")
        _seed_clip(connection, "asset_2")
        _seed_asset_kind(connection, "asset_vo", kind="audio")
        result = compose_initial(
            ComposeInitialInput(
                clips=[
                    {
                        "asset_id": "asset_1",
                        "source_start_s": 0.0,
                        "source_end_s": 1.5,
                        "role": "a_roll",
                    },
                    {
                        "asset_id": "asset_2",
                        "source_start_s": 2.0,
                        "source_end_s": 3.0,
                        "role": "b_roll",
                    },
                ],
                voiceover_asset_id="asset_vo",
            ),
            _context(connection, _draft_state()),
        )
        stored = connection.execute(
            select(schema.timeline_versions).where(schema.timeline_versions.c.version == 1)
        ).one()

    assert result.status == "succeeded"
    assert result.data["timeline_version"] == 1
    assert result.data["validation_report"]["valid"] is True
    visual = _track_clips(result.data["timeline"], "visual_base")
    assert [(clip["timeline_start_frame"], clip["timeline_end_frame"]) for clip in visual] == [
        (0, 45),
        (45, 75),
    ]
    assert len(_track_clips(result.data["timeline"], "voiceover")) == 1
    event_names = {event["event"] for event in result.events}
    assert {"TimelineVersionCreated", "TimelineValidated"} <= event_names
    assert load_json(stored._mapping["document_json"])["version"] == 1


def test_compose_initial_reports_invalid_clip_inputs(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        result = compose_initial(
            ComposeInitialInput(
                clips=[
                    {
                        "asset_id": "ghost",
                        "source_start_s": 0.0,
                        "source_end_s": 1.0,
                        "role": "a_roll",
                    }
                ]
            ),
            _context(connection, _draft_state()),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "timeline_materialization_failed"


def test_inspect_returns_timeline_summary(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1")
        store_timeline_version(
            connection,
            _timeline([_timeline_clip("tc_1", 0, 30)], duration_frames=30),
            created_at=NOW,
        )
        result = timeline_inspect(
            TimelineInspectInput(),
            _context(connection, _draft_state(timeline_current_version=1)),
        )

    assert result.status == "succeeded"
    assert "Timeline v1" in result.data["timeline_summary"]
    assert "asset_1/clip_1" in result.data["timeline_summary"]


def test_restore_version_writes_new_record(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_draft_row(connection, timeline_current_version=2, rough_cut_approved_version=1)
        store_timeline_version(
            connection,
            _timeline([_timeline_clip("tc_1", 0, 30)], duration_frames=30, version=1),
            created_at=NOW,
        )
        store_timeline_version(
            connection,
            _timeline(
                [_timeline_clip("tc_2", 0, 60, source_end=60)],
                duration_frames=60,
                version=2,
            ),
            created_at=NOW,
        )
        draft_state = _draft_state(
            timeline_current_version=2,
            rough_cut_approved=False,
            rough_cut_approved_version=1,
        )
        result = restore_version(
            TimelineRestoreVersionInput(source_version=1),
            _context(connection, draft_state),
        )

    assert result.status == "succeeded"
    assert result.data["timeline_version"] == 3
    assert result.events[-1]["event"] == "TimelineVersionRestored"
    with engine.connect() as connection:
        record = get_timeline_version(connection, "draft_1", 3)
    assert record is not None
    assert record.timeline.version == 3


def test_apply_patch_success_emits_timeline_events(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1")
        store_timeline_version(
            connection,
            _timeline([_timeline_clip("tc_1", 0, 30)], duration_frames=30),
            created_at=NOW,
        )
        result = apply_patch(
            _delete_range_request(0.0, 0.5, version=1),
            _context(connection, _draft_state(timeline_current_version=1)),
        )

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "TimelineVersionCreated",
        "TimelineValidated",
    ]
    assert "visual_base" in result.data["changed_track_ids"]
    assert result.data["timeline_version"] == 2


def _delete_range_request(start: float, end: float, *, version: int) -> TimelinePatchRequest:
    return TimelinePatchRequest(
        draft_id="draft_1",
        reference=TimelinePatchReference(timeline_version=version),
        op=DeleteRangeOp(
            kind="delete_range",
            time_range_sec=(start, end),
            scope="all_tracks",
            ripple=True,
        ),
        reason="delete range",
    )


def _engine(tmp_path: Path):
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)
        connection.execute(
            schema.drafts.insert().values(
                draft_id="draft_1",
                name="Draft",
                state_version=0,
                status="active",
                defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _seed_asset_kind(connection: Connection, asset_id: str, *, kind: str) -> None:
    connection.execute(
        schema.assets.insert().values(
            asset_id=asset_id,
            storage_mode="reference",
            object_hash=None,
            reference_path=f"/tmp/{asset_id}",
            kind=kind,
            source="local_path",
            filename=f"{asset_id}",
            hash=f"hash_{asset_id}",
            mtime=1,
            size=1,
            probe=dump_json({"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id=asset_id,
            linked_at=NOW,
            note="",
            rel_dir=None,
        )
    )


def _track_clips(timeline: dict[str, Any], track_id: str) -> list[dict[str, Any]]:
    for track in timeline["tracks"]:
        if track["track_id"] == track_id:
            return list(track["clips"])
    raise AssertionError(f"missing track {track_id}")


def _context(
    connection: Connection,
    draft_state: DraftState,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=draft_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata={},
    )


def _draft_state(
    *,
    timeline_current_version: int | None = None,
    rough_cut_approved: bool = False,
    rough_cut_approved_version: int | None = None,
) -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
            "audio_plan": {"mode": "silent"},
            "cut_plan": {
                "schema": "CutPlan.v1",
                "slots": [
                    {
                        "slot_id": "slot_1",
                        "brief": "product closeup",
                        "target_duration_sec": [1.0, 4.0],
                    }
                ],
                "total_target_duration_sec": 3.0,
            },
            "timeline_current_version": timeline_current_version,
            "rough_cut_approved": rough_cut_approved,
            "rough_cut_approved_version": rough_cut_approved_version,
        }
    )


def _seed_draft_row(
    connection: Connection,
    *,
    timeline_current_version: int | None,
    rough_cut_approved_version: int | None,
) -> None:
    # 基础夹具已种 draft_1：这里用 DELETE + INSERT 定制字段，避免主键冲突。
    connection.execute(schema.drafts.delete().where(schema.drafts.c.draft_id == "draft_1"))
    connection.execute(
        schema.drafts.insert().values(
            draft_id="draft_1",
            name="Draft",
            state_version=0,
            status="active",
            defaults=dump_json({"aspect_ratio": "9:16", "fps": 30}),
            timeline_current_version=timeline_current_version,
            timeline_validated=False,
            rough_cut_approved=False,
            rough_cut_approved_version=rough_cut_approved_version,
            running_jobs="[]",
            brief=dump_json({"goal": "test", "confirmed_facts": []}),
            audio_plan=dump_json({"mode": "silent"}),
            cut_plan=dump_json(
                {
                    "schema": "CutPlan.v1",
                    "slots": [],
                    "total_target_duration_sec": 0,
                }
            ),
            scratch_memory="{}",
            created_at=NOW,
            updated_at=NOW,
        )
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
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
            probe=dump_json({"duration_sec": 10.0, "fps": 30.0}),
            proxy_object_hash=None,
            ingest_status="indexed",
            usable=True,
            failure=None,
        )
    )
    connection.execute(
        schema.draft_asset_links.insert().values(
            draft_id="draft_1",
            asset_id=asset_id,
            linked_at=NOW,
            note="",
            rel_dir=None,
        )
    )


def _timeline(
    visual_clips: list[dict[str, Any]],
    *,
    duration_frames: int,
    version: int = 1,
) -> TimelineState:
    return TimelineState.model_validate(
        {
            "timeline_id": f"draft_1:v{version}",
            "draft_id": "draft_1",
            "version": version,
            "fps": 30,
            "duration_frames": duration_frames,
            "tracks": [
                {"track_id": "visual_base", "track_type": "primary_visual", "clips": visual_clips},
                {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
                {"track_id": "original_audio", "track_type": "audio", "clips": []},
                {"track_id": "voiceover", "track_type": "audio", "clips": []},
                {"track_id": "bgm", "track_type": "audio", "clips": []},
                {"track_id": "subtitles", "track_type": "text", "clips": []},
            ],
        }
    )


def _timeline_clip(
    timeline_clip_id: str,
    start: int,
    end: int,
    *,
    source_start: int = 0,
    source_end: int = 30,
) -> dict[str, Any]:
    return {
        "timeline_clip_id": timeline_clip_id,
        "track_id": "visual_base",
        "asset_id": "asset_1",
        "clip_id": "clip_1",
        "role": "b_roll",
        "timeline_start_frame": start,
        "timeline_end_frame": end,
        "source_start_frame": source_start,
        "source_end_frame": source_end,
    }
