"""timeline 工具 handler 的守卫分支。"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from sqlalchemy.engine import Connection

from contracts.draft import DraftState
from contracts.patch import AddBgmOp, DeleteRangeOp, TimelinePatchRequest
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from timeline import AnchorConflict, PatchApplyError, PatchOutcome
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


def _draft_state(
    *,
    aspect_ratio: str = "9:16",
    timeline_current_version: int | None = None,
) -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "defaults": {"aspect_ratio": aspect_ratio, "fps": 30},
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
        }
    )


def _context(
    *,
    draft_state: DraftState | None = None,
    connection: Connection | None = None,
    metadata: dict[str, Any] | None = None,
) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=draft_state,
        readonly_connection=connection,
        created_at=NOW,
        metadata=metadata or {},
    )


def _patch_request() -> TimelinePatchRequest:
    return TimelinePatchRequest(
        draft_id="draft_1",
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
        draft_id="draft_1",
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
# 守卫分支：missing_draft / missing_connection / timeline_missing / not_found
# ---------------------------------------------------------------------------


def test_all_handlers_fail_without_draft_or_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    calls = [
        (handlers.apply_patch, _patch_request()),
        (handlers.validate, TimelineValidateInput()),
        (handlers.inspect, TimelineInspectInput()),
        (handlers.restore_version, TimelineRestoreVersionInput(source_version=1)),
    ]
    with engine.connect() as connection:
        for handler, input_model in calls:
            no_draft = handler(input_model, _context(connection=connection))
            assert no_draft.status == "failed"
            assert no_draft.error is not None
            assert no_draft.error.error_code == "missing_draft"

            no_conn = handler(input_model, _context(draft_state=_draft_state()))
            assert no_conn.status == "failed"
            assert no_conn.error is not None
            assert no_conn.error.error_code == "missing_connection"


def test_compose_initial_fails_without_draft_or_connection(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    input_model = ComposeInitialInput(
        clips=[
            {"asset_id": "asset_1", "source_start_s": 0.0, "source_end_s": 1.0, "role": "a_roll"}
        ]
    )
    with engine.connect() as connection:
        no_draft = handlers.compose_initial(input_model, _context(connection=connection))
        assert no_draft.status == "failed"
        assert no_draft.error is not None
        assert no_draft.error.error_code == "missing_draft"

        no_conn = handlers.compose_initial(input_model, _context(draft_state=_draft_state()))
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
            result = handler(
                input_model, _context(draft_state=_draft_state(), connection=connection)
            )
            assert result.status == "failed"
            assert result.error is not None
            assert result.error.error_code == "timeline_missing"


def test_validate_and_inspect_fail_when_version_record_missing(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    draft_state = _draft_state(timeline_current_version=5)
    with engine.connect() as connection:
        context = _context(draft_state=draft_state, connection=connection)
        validated = handlers.validate(TimelineValidateInput(), context)
        inspected = handlers.inspect(TimelineInspectInput(version=5), context)
    assert validated.error is not None
    assert validated.error.error_code == "timeline_not_found"
    assert inspected.error is not None
    assert inspected.error.error_code == "timeline_not_found"


def test_restore_version_fails_when_source_version_missing(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    draft_state = _draft_state(timeline_current_version=1)
    with engine.begin() as connection:
        result = handlers.restore_version(
            TimelineRestoreVersionInput(source_version=99),
            _context(draft_state=draft_state, connection=connection),
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
    draft_state = _draft_state(timeline_current_version=1)

    def _fail(*args: Any, **kwargs: Any) -> PatchOutcome:
        return PatchOutcome(
            status="failed",
            error=PatchApplyError("patch_boom", "boom", details={"why": "test"}),
        )

    def _empty(*args: Any, **kwargs: Any) -> PatchOutcome:
        return PatchOutcome(status="succeeded")

    with engine.connect() as connection:
        context = _context(draft_state=draft_state, connection=connection)
        monkeypatch.setattr(handlers, "apply_timeline_patch", _fail)
        failed = handlers.apply_patch(_patch_request(), context)
        monkeypatch.setattr(handlers, "apply_timeline_patch", _empty)
        empty = handlers.apply_patch(_patch_request(), context)

    assert failed.error is not None
    assert failed.error.error_code == "patch_boom"
    assert failed.error.details == {"why": "test"}
    assert empty.error is not None
    assert empty.error.error_code == "timeline_patch_failed"


def test_apply_patch_conflict_returns_decision(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """锚点冲突时 apply_patch 转人工确认：发 DecisionCreated 并附带时间线摘要。"""
    engine = _engine(tmp_path)
    draft_state = _draft_state(timeline_current_version=1)

    def _conflict(*args: Any, **kwargs: Any) -> PatchOutcome:
        return PatchOutcome(
            status="conflict",
            conflict=AnchorConflict("anchor_conflict", "anchor changed", details={"clip": "tc_1"}),
        )

    with engine.connect() as connection:
        monkeypatch.setattr(handlers, "apply_timeline_patch", _conflict)
        result = handlers.apply_patch(
            _patch_request(),
            _context(draft_state=draft_state, connection=connection),
        )

    assert result.status == "requires_user"
    assert result.events[0]["event"] == "DecisionCreated"
    assert result.events[0]["scope_type"] == "draft"
    assert result.events[0]["draft_id"] == "draft_1"
    assert "timeline_summary" in result.data
    assert result.data["anchor_conflict"]["code"] == "anchor_conflict"


# ---------------------------------------------------------------------------
# apply_patch 的 add_bgm：素材必须是草稿内存量音频资产
# ---------------------------------------------------------------------------


class _ReachedApply(Exception):
    """标记 apply_timeline_patch 被真正调用，用于确认校验放行。"""


def test_apply_patch_add_bgm_rejects_missing_or_non_audio_asset(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    draft_state = _draft_state(timeline_current_version=1)
    with engine.begin() as connection:
        # 素材不存在
        missing = handlers.apply_patch(
            _add_bgm_request("asset_ghost"),
            _context(draft_state=draft_state, connection=connection),
        )
        # 素材存在但不是音频
        _seed_asset(connection, "asset_video", kind="video")
        wrong_kind = handlers.apply_patch(
            _add_bgm_request("asset_video"),
            _context(draft_state=draft_state, connection=connection),
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
    draft_state = _draft_state(timeline_current_version=1)
    captured: dict[str, str] = {}

    def _capture(
        connection: Connection,
        ds: DraftState,
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
                _context(draft_state=draft_state, connection=connection),
            )
    # 校验放行后原样把 input_model 交给 apply_timeline_patch，未再改写 asset_id
    assert captured["asset_id"] == "asset_bgm"


# ---------------------------------------------------------------------------
# _draft_aspect_ratio 的回退分支
# ---------------------------------------------------------------------------


def test_draft_aspect_ratio_reads_defaults_or_unknown() -> None:
    assert handlers._draft_aspect_ratio(
        _context(draft_state=_draft_state(aspect_ratio="16:9"))
    ) == ("16:9")
    assert handlers._draft_aspect_ratio(_context()) == "unknown"
