from __future__ import annotations

from pathlib import Path
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from agent_harness.policy_gate import PolicyContext, PolicyGate
from agent_harness.reducer import apply
from contracts.case import CaseState
from contracts.timeline import TimelineState
from domain.preconditions import PreconditionContext, ProjectArtifactStats
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import CasesRepository
from storage.repositories._json import dump_json, load_json
from timeline import store_timeline_version
from tools import ToolExecutionContext, build_default_tool_registry
from tools.specs import (
    PATCH_OP_REGISTRY,
    TimelineInspectInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
    tool_specs,
)
from tools.timeline_tools import (
    inspect as timeline_inspect,
)
from tools.timeline_tools import (
    restore_version,
    validate,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_validate_invalid_timeline_and_render_preview_not_allowed(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup")
        timeline = _timeline(
            [
                _timeline_clip("tc_1", 0, 30),
                _timeline_clip("tc_2", 40, 60, source_start=40, source_end=60),
            ],
            duration_frames=60,
        )
        store_timeline_version(connection, timeline, created_at=NOW)
        case_state = _case_state(timeline_current_version=1)
        result = validate(TimelineValidateInput(), _context(connection, case_state))
    registry = build_default_tool_registry()
    gate = PolicyGate(
        tool_specs={spec.name: spec for spec in tool_specs()},
        patch_op_specs=PATCH_OP_REGISTRY.as_mapping(),
    )
    allowed = gate.compute_allowed_tools(
        PolicyContext(
            preconditions=PreconditionContext(
                case_state=case_state.model_copy(update={"timeline_validated": False}),
                project_artifacts=ProjectArtifactStats(usable_asset_count=1),
            )
        )
    )

    assert registry.get("render.preview") is not None
    assert result.data["valid"] is False
    assert "render.preview" not in {spec.name for spec in allowed}


def test_inspect_returns_timeline_summary(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup")
        store_timeline_version(
            connection,
            _timeline([_timeline_clip("tc_1", 0, 30)], duration_frames=30),
            created_at=NOW,
        )
        result = timeline_inspect(
            TimelineInspectInput(),
            _context(connection, _case_state(timeline_current_version=1)),
        )

    assert result.status == "succeeded"
    assert "Timeline v1" in result.data["timeline_summary"]
    assert "asset_1/clip_1" in result.data["timeline_summary"]


def test_restore_version_writes_new_record_and_reducer_restores_rough_cut(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_case_row(connection, timeline_current_version=2, rough_cut_approved_version=1)
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
        case_state = _case_state(
            timeline_current_version=2,
            rough_cut_approved=False,
            rough_cut_approved_version=1,
        )
        result = restore_version(
            TimelineRestoreVersionInput(source_version=1),
            _context(connection, case_state),
        )

    applied = apply(result.events, engine=engine, base_version=0, actor="agent", created_at=NOW)
    with engine.connect() as connection:
        case_row = CasesRepository(connection).get("case_1")
        restored = connection.execute(
            select(schema.timeline_versions).where(schema.timeline_versions.c.version == 3)
        ).one()

    assert result.status == "succeeded"
    assert applied.status == "applied"
    assert case_row is not None
    assert case_row["timeline_current_version"] == 3
    assert case_row["rough_cut_approved"] is True
    assert case_row["rough_cut_approved_version"] == 3
    assert load_json(restored._mapping["document_json"])["version"] == 3


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
                timeline_validated=False,
                rough_cut_approved=False,
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                selected_asset_ids="[]",
                disabled_asset_ids="[]",
                scratch_memory="{}",
            )
        )
    return engine


def _context(
    connection: Connection,
    case_state: CaseState,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=case_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata={},
    )


def _case_state(
    *,
    timeline_current_version: int | None = None,
    rough_cut_approved: bool = False,
    rough_cut_approved_version: int | None = None,
) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": "project_1",
            "name": "Case",
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
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _seed_case_row(
    connection: Connection,
    *,
    timeline_current_version: int | None,
    rough_cut_approved_version: int | None,
) -> None:
    # 基础夹具已种 case_1：这里用 UPDATE 定制字段，避免 UNIQUE 冲突
    connection.execute(schema.cases.delete().where(schema.cases.c.case_id == "case_1"))
    connection.execute(
        schema.cases.insert().values(
            case_id="case_1",
            project_id="project_1",
            name="Case",
            state_version=0,
            status="active",
            timeline_validated=False,
            timeline_current_version=timeline_current_version,
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
            selected_asset_ids="[]",
            disabled_asset_ids="[]",
            scratch_memory="{}",
        )
    )


def _seed_clip(
    connection: Connection,
    asset_id: str,
    clip_id: str,
    summary: str,
) -> None:
    annotation_id = f"ann_{asset_id}"
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
        schema.project_asset_links.insert().values(
            project_id="project_1",
            asset_id=asset_id,
            enabled=True,
            linked_at=NOW,
            note="",
        )
    )
    document = {
        "schema": "AnnotationDocument.v1",
        "annotation_id": annotation_id,
        "asset_id": asset_id,
        "asset_kind": "video",
        "status": "completed",
        "generator": {"pipeline_version": "annotation.video.v1", "pass": "cheap"},
        "clips": [],
        "quality_events": [],
        "created_at": NOW,
    }
    connection.execute(
        schema.annotations_table.insert().values(
            annotation_id=annotation_id,
            asset_id=asset_id,
            schema="AnnotationDocument.v1",
            status="completed",
            document_json=dump_json(document),
            created_at=NOW,
            updated_at=NOW,
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
            "timeline_id": f"case_1:v{version}",
            "case_id": "case_1",
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
