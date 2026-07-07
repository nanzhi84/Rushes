"""timeline 工具 handler 的守卫分支。"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.case import CaseState
from contracts.patch import AddBgmOp, DeleteRangeOp, TimelinePatchRequest
from contracts.project import ProjectState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import PatchApplyError, PatchOutcome
from tools import ToolExecutionContext
from tools.specs import (
    ComposeInitialInput,
    TimelineInspectInput,
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


def _add_bgm_request(asset_id: str) -> TimelinePatchRequest:
    return TimelinePatchRequest(
        case_id="case_1",
        op=AddBgmOp(kind="add_bgm", asset_id=asset_id, gain_db=-12.0, duck=True),
        reason="add bgm",
    )


def _seed_asset(connection: Connection, asset_id: str, *, kind: str) -> None:
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


# ---------------------------------------------------------------------------
# 守卫分支：missing_case / missing_connection / timeline_missing / not_found
# ---------------------------------------------------------------------------


def test_all_handlers_fail_without_case_or_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    calls = [
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


def test_compose_initial_fails_without_case_or_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    input_model = ComposeInitialInput(
        clips=[
            {"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 1.0, "role": "a_roll"}
        ]
    )
    with engine.connect() as connection:
        no_case = handlers.compose_initial(input_model, _context(connection=connection))
        assert no_case.status == "failed"
        assert no_case.error is not None
        assert no_case.error.error_code == "missing_case"

        no_conn = handlers.compose_initial(input_model, _context(case_state=_case_state()))
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


# ---------------------------------------------------------------------------
# apply_patch 的引擎级错误分支
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


# ---------------------------------------------------------------------------
# apply_patch 的 add_bgm：素材必须是项目内存量音频资产
# ---------------------------------------------------------------------------


class _ReachedApply(Exception):
    """标记 apply_timeline_patch 被真正调用，用于确认校验放行。"""


def test_apply_patch_add_bgm_rejects_missing_or_non_audio_asset(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(timeline_current_version=1)
    with engine.begin() as connection:
        # 素材不存在
        missing = handlers.apply_patch(
            _add_bgm_request("asset_ghost"),
            _context(case_state=case_state, connection=connection),
        )
        # 素材存在但不是音频
        _seed_asset(connection, "asset_video", kind="video")
        wrong_kind = handlers.apply_patch(
            _add_bgm_request("asset_video"),
            _context(case_state=case_state, connection=connection),
        )
    for result, asset_id in ((missing, "asset_ghost"), (wrong_kind, "asset_video")):
        assert result.status == "failed"
        assert result.error is not None
        assert result.error.error_code == "asset_not_found"
        assert result.error.details == {"asset_id": asset_id}


def test_apply_patch_add_bgm_real_audio_proceeds_into_patch(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = _engine(tmp_path)
    case_state = _case_state(timeline_current_version=1)
    captured: dict[str, str] = {}

    def _capture(
        connection: Connection,
        cs: CaseState,
        input_model: TimelinePatchRequest,
        *,
        created_at: str,
    ) -> Any:
        captured["asset_id"] = input_model.op.asset_id
        raise _ReachedApply

    with engine.begin() as connection:
        _seed_asset(connection, "asset_bgm", kind="audio")
        monkeypatch.setattr(handlers, "apply_timeline_patch", _capture)
        with pytest.raises(_ReachedApply):
            handlers.apply_patch(
                _add_bgm_request("asset_bgm"),
                _context(case_state=case_state, connection=connection),
            )
    # 校验放行后原样把 input_model 交给 apply_timeline_patch，未再改写 asset_id
    assert captured["asset_id"] == "asset_bgm"


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
