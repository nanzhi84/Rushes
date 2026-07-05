from __future__ import annotations

from array import array
from pathlib import Path
from typing import Any

from sqlalchemy import select
from sqlalchemy.engine import Connection

from agent_harness.policy_gate import PolicyContext, PolicyGate
from agent_harness.reducer import apply
from contracts.candidate import CandidatePack
from contracts.case import CaseState
from contracts.timeline import TimelineState
from domain.preconditions import PreconditionContext, ProjectArtifactStats
from indexing import build_candidate_pack
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories import CasesRepository
from storage.repositories._json import dump_json, load_json
from timeline import store_timeline_version
from tools import ToolExecutionContext, build_default_tool_registry
from tools.specs import (
    PATCH_OP_REGISTRY,
    TimelineInspectInput,
    TimelinePlanFromCandidatesInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
    tool_specs,
)
from tools.timeline_tools import (
    inspect as timeline_inspect,
)
from tools.timeline_tools import (
    plan_from_candidates,
    restore_version,
    validate,
)

NOW = "2026-07-05T00:00:00+00:00"


def test_plan_from_candidates_happy_path_creates_valid_timeline_version(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup")
        case_state = _case_state()
        pack = build_candidate_pack(connection, case_state, case_state.cut_plan, {})
        _persist_pack(connection, pack)
        result = plan_from_candidates(
            TimelinePlanFromCandidatesInput(
                selections=[_selection_for_asset(pack, "asset_1")],
            ),
            _context(
                connection,
                case_state.model_copy(update={"candidate_pack_id": pack.candidate_pack_id}),
                pack,
            ),
        )
        rows = connection.execute(select(schema.timeline_versions)).all()

    assert result.status == "succeeded"
    assert [event["event"] for event in result.events] == [
        "TimelineVersionCreated",
        "TimelineValidated",
    ]
    assert len(rows) == 1
    assert rows[0]._mapping["version"] == 1


def test_plan_from_candidates_removes_stale_unselected_candidate_and_observes(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_keep", "clip_keep", "product closeup")
        _seed_clip(connection, "asset_drop", "clip_drop", "product closeup")
        base_state = _case_state()
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
        _persist_pack(connection, pack)
        case_state = base_state.model_copy(
            update={
                "candidate_pack_id": pack.candidate_pack_id,
                "disabled_asset_ids": ["asset_drop"],
            }
        )
        result = plan_from_candidates(
            TimelinePlanFromCandidatesInput(
                selections=[_selection_for_asset(pack, "asset_keep")],
            ),
            _context(connection, case_state, pack),
        )

    assert result.status == "succeeded"
    assert result.data["removed_candidates"][0]["asset_id"] == "asset_drop"
    assert "removed 1 stale candidate" in result.observation


def test_plan_from_candidates_selected_candidate_invalid_requires_user(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup")
        base_state = _case_state()
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
        _persist_pack(connection, pack)
        case_state = base_state.model_copy(
            update={
                "candidate_pack_id": pack.candidate_pack_id,
                "disabled_asset_ids": ["asset_1"],
            }
        )
        result = plan_from_candidates(
            TimelinePlanFromCandidatesInput(selections=[_selection_for_asset(pack, "asset_1")]),
            _context(connection, case_state, pack),
        )

    assert result.status == "requires_user"
    assert result.events[0]["event"] == "DecisionCreated"
    assert result.data["invalid_selected_candidates"][0]["asset_id"] == "asset_1"


def test_plan_from_candidates_fails_when_scope_changes_without_candidate_removal(
    tmp_path: Path,
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1", "product closeup")
        case_state = _case_state()
        pack = build_candidate_pack(connection, case_state, case_state.cut_plan, {})
        _persist_pack(connection, pack)
        _seed_clip(connection, "asset_new", "clip_new", "product closeup")
        result = plan_from_candidates(
            TimelinePlanFromCandidatesInput(selections=[_selection_for_asset(pack, "asset_1")]),
            _context(
                connection,
                case_state.model_copy(update={"candidate_pack_id": pack.candidate_pack_id}),
                pack,
            ),
        )

    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "candidate_pack_scope_changed"


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
        result = validate(TimelineValidateInput(), _context(connection, case_state, None))
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
            _context(connection, _case_state(timeline_current_version=1), None),
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
            _context(connection, case_state, None),
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
    pack: CandidatePack | None,
) -> ToolExecutionContext:
    metadata: dict[str, Any] = {}
    if pack is not None:
        metadata["candidate_pack"] = pack
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=case_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata,
    )


def _case_state(
    *,
    candidate_pack_id: str | None = None,
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
            "candidate_pack_id": candidate_pack_id,
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


def _persist_pack(connection: Connection, pack: CandidatePack) -> None:
    connection.execute(
        schema.candidate_packs.insert().values(
            candidate_pack_id=pack.candidate_pack_id,
            case_id=pack.case_id,
            slots=dump_json([slot.model_dump(mode="json") for slot in pack.slots]),
            created_at=pack.snapshot.generated_at,
        )
    )


def _selection_for_asset(pack: CandidatePack, asset_id: str) -> dict[str, str]:
    for slot in pack.slots:
        for candidate in slot.candidates:
            if candidate.asset_id == asset_id:
                return {"slot_id": slot.slot_id, "candidate_id": candidate.candidate_id}
    raise AssertionError(f"candidate not found for {asset_id}")


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
            annotation_status="completed",
            annotation_pass="cheap",
            index_status="ready",
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
    connection.execute(
        schema.annotation_clip_projection.insert().values(
            clip_id=clip_id,
            annotation_id=annotation_id,
            asset_id=asset_id,
            start_frame=0,
            end_frame=90,
            role="b_roll_candidate",
            summary=summary,
            keywords_json=dump_json(summary.split()),
            quality_score=0.9,
            usable=True,
            embedding=array("f", [1.0, 0.0]).tobytes(),
        )
    )
    connection.exec_driver_sql(
        (
            "INSERT INTO clip_fts "
            "(clip_id, summary, keywords, retrieval_sentence, ocr_text) "
            "VALUES (?, ?, ?, ?, ?)"
        ),
        (clip_id, summary, " ".join(summary.split()), summary, ""),
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
