from pathlib import Path

from sqlalchemy import func, select, update

from agent_harness.reducer import apply
from storage import schema
from storage.db import begin_immediate, create_workspace_engine
from storage.repositories import DraftsRepository
from storage.repositories._json import dump_json
from timeline import build_timeline_invariant_hook

NOW = "2026-07-04T00:00:00+00:00"


def _timeline_doc(draft_id: str = "draft_1", version: int = 1) -> dict[str, object]:
    return {
        "timeline_id": f"{draft_id}:v{version}",
        "draft_id": draft_id,
        "version": version,
        "fps": 30,
        "duration_frames": 30,
        "tracks": [
            {"track_id": "visual_base", "track_type": "primary_visual", "clips": []},
            {"track_id": "visual_overlay", "track_type": "visual_overlay", "clips": []},
            {"track_id": "original_audio", "track_type": "audio", "clips": []},
            {"track_id": "voiceover", "track_type": "audio", "clips": []},
            {"track_id": "bgm", "track_type": "audio", "clips": []},
            {"track_id": "subtitles", "track_type": "text", "clips": []},
        ],
    }


def _prepare_workspace(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with engine.begin() as connection:
        schema.create_all(connection)


def _insert_draft(tmp_path: Path, draft_id: str = "draft_1") -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        DraftsRepository(connection).insert(
            {
                "draft_id": draft_id,
                "name": "Draft",
                "state_version": 0,
                "status": "active",
                "defaults": {"aspect_ratio": "9:16", "fps": 30},
                "pending_decision_id": None,
                "running_jobs": [],
                "last_error": None,
                "brief": {"goal": "test", "confirmed_facts": []},
                "content_plan": None,
                "audio_plan": None,
                "cut_plan": None,
                "timeline_current_version": None,
                "timeline_validated": False,
                "preview_current_id": None,
                "last_viewed_preview_id": None,
                "rough_cut_approved": False,
                "rough_cut_approved_version": None,
                "postprocess_plan": None,
                "export_current_id": None,
                "scratch_memory": {},
                "messages_tail_ref": None,
                "created_at": NOW,
                "updated_at": NOW,
            }
        )


def _apply_brief_update(tmp_path: Path) -> object:
    engine = create_workspace_engine(tmp_path)
    return apply(
        [{"event": "BriefUpdated", "draft_id": "draft_1", "payload": {"brief": {"goal": "new"}}}],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
    )


def _assert_rolled_back(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        draft = DraftsRepository(connection).get("draft_1")
        event_count = connection.execute(
            select(func.count()).select_from(schema.event_log)
        ).scalar_one()
    assert draft is not None
    assert draft["state_version"] == 0
    assert draft["brief"]["goal"] == "test"
    assert event_count == 0


def test_validator_rejects_draft_reference_to_preview_from_another_draft(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    _insert_draft(tmp_path, draft_id="draft_2")
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.objects.insert().values(
                hash="hash_preview",
                rel_path="objects/hash_preview",
                size=0,
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_other",
                draft_id="draft_2",
                timeline_version=1,
                object_hash="hash_preview",
                quality=dump_json({}),
                created_at=NOW,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(preview_current_id="preview_other")
        )

    result = _apply_brief_update(tmp_path)

    assert result.status == "validation_failed"
    _assert_rolled_back(tmp_path)


def test_validator_rejects_pending_decision_that_is_not_pending(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.decisions.insert().values(
                decision_id="dec_answered",
                scope_type="draft",
                draft_id="draft_1",
                type="generic",
                question="?",
                options=dump_json([]),
                status="answered",
                answer=dump_json({"option_id": "yes", "answered_via": "button", "payload": {}}),
                pending_tool_call=None,
                pending_tool_call_status=None,
                consumed_at=None,
                replayed_tool_call_id=None,
                blocking=True,
                created_by_tool_call_id=None,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(pending_decision_id="dec_answered")
        )

    result = _apply_brief_update(tmp_path)

    assert result.status == "validation_failed"
    _assert_rolled_back(tmp_path)


def test_validator_rejects_invalid_timeline_document(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id="tl_bad",
                draft_id="draft_1",
                version=1,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(
                    {"timeline_id": "tl_bad", "draft_id": "draft_1", "version": 2}
                ),
                validation_report=None,
                created_at=NOW,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(timeline_current_version=1)
        )

    result = _apply_brief_update(tmp_path)

    assert result.status == "validation_failed"
    _assert_rolled_back(tmp_path)


def test_validator_rejects_missing_or_cross_draft_references(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    _insert_draft(tmp_path, draft_id="draft_2")
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.decisions.insert().values(
                decision_id="dec_other",
                scope_type="draft",
                draft_id="draft_2",
                type="generic",
                question="?",
                options=dump_json([]),
                status="pending",
                answer=None,
                pending_tool_call=None,
                pending_tool_call_status=None,
                consumed_at=None,
                replayed_tool_call_id=None,
                blocking=True,
                created_by_tool_call_id=None,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(
                timeline_current_version=99,
                pending_decision_id="dec_other",
            )
        )

    result = _apply_brief_update(tmp_path)

    assert result.status == "validation_failed"
    assert result.validation_failed is not None
    assert {violation.code for violation in result.validation_failed.violations} >= {
        "missing_timeline_current_version",
        "invalid_pending_decision_id",
    }
    _assert_rolled_back(tmp_path)


def test_validator_rejects_timeline_identity_mismatch(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id="tl_mismatch",
                draft_id="draft_1",
                version=1,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(_timeline_doc(version=2)),
                validation_report=None,
                created_at=NOW,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(timeline_current_version=1)
        )

    result = _apply_brief_update(tmp_path)

    assert result.status == "validation_failed"
    assert result.validation_failed is not None
    assert result.validation_failed.violations[0].code == "timeline_identity_mismatch"
    _assert_rolled_back(tmp_path)


def test_validator_rejects_timeline_invariant_hook_failure(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id="draft_1:v1",
                draft_id="draft_1",
                version=1,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(_timeline_doc()),
                validation_report=None,
                created_at=NOW,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(timeline_current_version=1)
        )

    result = apply(
        [{"event": "BriefUpdated", "draft_id": "draft_1", "payload": {"brief": {"goal": "new"}}}],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
        timeline_invariant_hook=lambda _connection, _draft_state, _timeline: [
            "primary visual has a gap"
        ],
    )

    assert result.status == "validation_failed"
    assert result.validation_failed is not None
    assert result.validation_failed.violations[0].code == "timeline_frame_invariant_failed"
    _assert_rolled_back(tmp_path)


def test_validator_can_use_timeline_validator_hook(tmp_path: Path) -> None:
    _prepare_workspace(tmp_path)
    _insert_draft(tmp_path)
    engine = create_workspace_engine(tmp_path)
    with begin_immediate(engine) as connection:
        connection.execute(
            schema.timeline_versions.insert().values(
                timeline_id="draft_1:v1",
                draft_id="draft_1",
                version=1,
                parent_version=None,
                created_by_patch_id=None,
                document_json=dump_json(_timeline_doc()),
                validation_report=None,
                created_at=NOW,
            )
        )
        connection.execute(
            update(schema.drafts)
            .where(schema.drafts.c.draft_id == "draft_1")
            .values(timeline_current_version=1)
        )

    result = apply(
        [{"event": "BriefUpdated", "draft_id": "draft_1", "payload": {"brief": {"goal": "new"}}}],
        engine=engine,
        base_version=0,
        actor="agent",
        created_at=NOW,
        timeline_invariant_hook=build_timeline_invariant_hook(),
    )

    assert result.status == "validation_failed"
    assert result.validation_failed is not None
    assert result.validation_failed.violations[0].code == "timeline_frame_invariant_failed"
    assert "timeline.primary_visual.gap" in result.validation_failed.violations[0].message
    _assert_rolled_back(tmp_path)
