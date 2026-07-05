"""timeline 工具 handler 的守卫分支与 candidate pack 回退加载路径。"""

from __future__ import annotations

from array import array
from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.patch import DeleteRangeOp, TimelinePatchRequest
from contracts.project import ProjectState
from indexing import build_candidate_pack
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import MaterializationError, PatchApplyError, PatchOutcome
from tools import ToolExecutionContext
from tools.specs import (
    TimelineInspectInput,
    TimelinePlanFromCandidatesInput,
    TimelineRestoreVersionInput,
    TimelineValidateInput,
)
from tools.timeline_tools import handlers

NOW = "2026-07-05T00:00:00+00:00"


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


def _case_state(
    *,
    project_id: str = "project_1",
    candidate_pack_id: str | None = None,
    timeline_current_version: int | None = None,
) -> CaseState:
    return CaseState.model_validate(
        {
            "case_id": "case_1",
            "project_id": project_id,
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
            "selected_asset_ids": [],
            "disabled_asset_ids": [],
            "scratch_memory": {},
        }
    )


def _context(
    *,
    case_state: CaseState | None = None,
    connection: Connection | None = None,
    project_state: ProjectState | None = None,
    metadata: dict[str, Any] | None = None,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        case_state=case_state,
        project_state=project_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata or {},
    )


def _patch_request() -> TimelinePatchRequest:
    return TimelinePatchRequest(
        case_id="case_1",
        op=DeleteRangeOp(
            kind="delete_range",
            time_range_sec=(0.0, 1.0),
            scope="all_tracks",
            ripple=True,
        ),
        reason="test",
    )


def _seed_clip(connection: Connection, asset_id: str, clip_id: str) -> None:
    summary = "product closeup"
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


# ---------------------------------------------------------------------------
# 守卫分支：missing_case / missing_connection / timeline_missing / not_found
# ---------------------------------------------------------------------------


def test_all_handlers_fail_without_case_or_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    calls = [
        (handlers.plan_from_candidates, TimelinePlanFromCandidatesInput(selections=[])),
        (handlers.apply_patch, _patch_request()),
        (handlers.validate, TimelineValidateInput()),
        (handlers.inspect, TimelineInspectInput()),
        (handlers.restore_version, TimelineRestoreVersionInput(source_version=1)),
    ]
    with engine.connect() as connection:
        for handler, input_model in calls:
            no_case = handler(input_model, _context(connection=connection))
            assert no_case.status == "failed"
            assert no_case.error is not None
            assert no_case.error.error_code == "missing_case"

            no_conn = handler(input_model, _context(case_state=_case_state()))
            assert no_conn.status == "failed"
            assert no_conn.error is not None
            assert no_conn.error.error_code == "missing_connection"


def test_handlers_fail_without_current_timeline(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    calls = [
        (handlers.apply_patch, _patch_request()),
        (handlers.validate, TimelineValidateInput()),
        (handlers.inspect, TimelineInspectInput()),
        (handlers.restore_version, TimelineRestoreVersionInput(source_version=1)),
    ]
    with engine.connect() as connection:
        for handler, input_model in calls:
            result = handler(input_model, _context(case_state=_case_state(), connection=connection))
            assert result.status == "failed"
            assert result.error is not None
            assert result.error.error_code == "timeline_missing"


def test_validate_and_inspect_fail_when_version_record_missing(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(timeline_current_version=5)
    with engine.connect() as connection:
        context = _context(case_state=case_state, connection=connection)
        validated = handlers.validate(TimelineValidateInput(), context)
        inspected = handlers.inspect(TimelineInspectInput(version=5), context)
    assert validated.error is not None
    assert validated.error.error_code == "timeline_not_found"
    assert inspected.error is not None
    assert inspected.error.error_code == "timeline_not_found"


def test_restore_version_fails_when_source_version_missing(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(timeline_current_version=1)
    with engine.begin() as connection:
        result = handlers.restore_version(
            TimelineRestoreVersionInput(source_version=99),
            _context(case_state=case_state, connection=connection),
        )
    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "timeline_source_version_not_found"


def test_plan_from_candidates_requires_pack_id_and_existing_pack(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.connect() as connection:
        no_pack_id = handlers.plan_from_candidates(
            TimelinePlanFromCandidatesInput(selections=[]),
            _context(case_state=_case_state(), connection=connection),
        )
        # metadata/event log/candidate_packs 行三级回退全部落空
        not_found = handlers.plan_from_candidates(
            TimelinePlanFromCandidatesInput(selections=[]),
            _context(
                case_state=_case_state(candidate_pack_id="pack_ghost"),
                connection=connection,
            ),
        )
    assert no_pack_id.error is not None
    assert no_pack_id.error.error_code == "candidate_pack_missing"
    assert not_found.error is not None
    assert not_found.error.error_code == "candidate_pack_not_found"


# ---------------------------------------------------------------------------
# apply_patch / plan_from_candidates 的引擎级错误分支
# ---------------------------------------------------------------------------


def test_apply_patch_maps_engine_error_and_empty_outcome(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(timeline_current_version=1)

    def _fail(*args: Any, **kwargs: Any) -> PatchOutcome:
        return PatchOutcome(
            status="failed",
            error=PatchApplyError("patch_boom", "boom", details={"why": "test"}),
        )

    def _empty(*args: Any, **kwargs: Any) -> PatchOutcome:
        return PatchOutcome(status="succeeded")

    with engine.connect() as connection:
        context = _context(case_state=case_state, connection=connection)
        monkeypatch.setattr(handlers, "apply_timeline_patch", _fail)
        failed = handlers.apply_patch(_patch_request(), context)
        monkeypatch.setattr(handlers, "apply_timeline_patch", _empty)
        empty = handlers.apply_patch(_patch_request(), context)

    assert failed.error is not None
    assert failed.error.error_code == "patch_boom"
    assert failed.error.details == {"why": "test"}
    assert empty.error is not None
    assert empty.error.error_code == "timeline_patch_failed"


def test_plan_from_candidates_maps_materialization_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        _seed_clip(connection, "asset_1", "clip_1")
        base_state = _case_state()
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
        case_state = _case_state(candidate_pack_id=pack.candidate_pack_id)
        selection = {
            "slot_id": pack.slots[0].slot_id,
            "candidate_id": pack.slots[0].candidates[0].candidate_id,
        }

        def _boom(*args: Any, **kwargs: Any) -> Any:
            raise MaterializationError("materialize boom")

        monkeypatch.setattr(handlers, "materialize_from_selection", _boom)
        result = handlers.plan_from_candidates(
            TimelinePlanFromCandidatesInput(selections=[selection]),
            _context(
                case_state=case_state,
                connection=connection,
                metadata={"candidate_pack": pack},
            ),
        )
    assert result.status == "failed"
    assert result.error is not None
    assert result.error.error_code == "timeline_materialization_failed"


# ---------------------------------------------------------------------------
# candidate pack 三级回退加载与解析辅助
# ---------------------------------------------------------------------------


def test_candidate_pack_loaded_from_metadata_packs_mapping(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        base_state = _case_state()
        _seed_clip(connection, "asset_1", "clip_1")
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
    loaded = handlers._candidate_pack_from_metadata(
        {"candidate_packs": {pack.candidate_pack_id: pack.model_dump(mode="json")}},
        pack.candidate_pack_id,
    )
    assert loaded is not None
    assert loaded.candidate_pack_id == pack.candidate_pack_id
    assert handlers._candidate_pack_from_metadata({}, "pack_x") is None
    assert handlers._parse_candidate_pack("junk") is None


def test_candidate_pack_loaded_from_event_log_fallback(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        base_state = _case_state()
        _seed_clip(connection, "asset_1", "clip_1")
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
        # 混入格式不合的历史事件，覆盖跳过分支
        for junk in ("[]", dump_json({"payload": 3}), dump_json({"payload": {}})):
            connection.execute(
                schema.event_log.insert().values(
                    event_type="CandidatePackCreated",
                    actor="system",
                    case_id="case_1",
                    payload_json=junk,
                    created_at=NOW,
                )
            )
        connection.execute(
            schema.event_log.insert().values(
                event_type="CandidatePackCreated",
                actor="system",
                case_id="case_1",
                payload_json=dump_json(
                    {"payload": {"candidate_pack": pack.model_dump(mode="json")}}
                ),
                created_at=NOW,
            )
        )
        loaded = handlers._load_candidate_pack(
            _context(case_state=base_state, connection=connection),
            pack.candidate_pack_id,
        )
    assert loaded is not None
    assert loaded.candidate_pack_id == pack.candidate_pack_id


def test_candidate_pack_loaded_from_row_fallback(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        base_state = _case_state()
        _seed_clip(connection, "asset_1", "clip_1")
        pack = build_candidate_pack(connection, base_state, base_state.cut_plan, {})
        connection.execute(
            schema.candidate_packs.insert().values(
                candidate_pack_id=pack.candidate_pack_id,
                case_id=pack.case_id,
                slots=dump_json([slot.model_dump(mode="json") for slot in pack.slots]),
                created_at=pack.snapshot.generated_at,
            )
        )
        loaded = handlers._load_candidate_pack(
            _context(case_state=base_state, connection=connection),
            pack.candidate_pack_id,
        )
        assert loaded is not None
        assert loaded.candidate_pack_id == pack.candidate_pack_id
        assert [slot.slot_id for slot in loaded.slots] == [slot.slot_id for slot in pack.slots]

        # 行里 slots 不是 JSON 数组时放弃解析
        connection.execute(
            schema.candidate_packs.insert().values(
                candidate_pack_id="pack_bad",
                case_id="case_1",
                slots=dump_json({"not": "a list"}),
                created_at=NOW,
            )
        )
        assert (
            handlers._load_candidate_pack(
                _context(case_state=base_state, connection=connection), "pack_bad"
            )
            is None
        )


# ---------------------------------------------------------------------------
# _project_aspect_ratio 的各回退分支
# ---------------------------------------------------------------------------


def test_project_aspect_ratio_fallbacks(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    project_state = ProjectState.model_validate(
        {
            "project_id": "project_1",
            "name": "Project",
            "defaults": {"aspect_ratio": "16:9"},
            "created_at": NOW,
            "updated_at": NOW,
        }
    )
    assert handlers._project_aspect_ratio(_context(project_state=project_state)) == "16:9"
    assert handlers._project_aspect_ratio(_context()) == "unknown"

    with engine.begin() as connection:
        connection.execute(
            schema.projects.insert().values(
                project_id="project_bad_defaults",
                name="Bad",
                status="active",
                defaults=dump_json("not a mapping"),
                created_at=NOW,
                updated_at=NOW,
            )
        )
        missing_row = handlers._project_aspect_ratio(
            _context(case_state=_case_state(project_id="project_ghost"), connection=connection)
        )
        bad_defaults = handlers._project_aspect_ratio(
            _context(
                case_state=_case_state(project_id="project_bad_defaults"),
                connection=connection,
            )
        )
        from_row = handlers._project_aspect_ratio(
            _context(case_state=_case_state(), connection=connection)
        )
    assert missing_row == "unknown"
    assert bad_defaults == "unknown"
    assert from_row == "9:16"
