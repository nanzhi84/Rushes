from __future__ import annotations

from pathlib import Path

from sqlalchemy.engine import Connection

from contracts.draft import DraftState
from storage import schema
from storage.db import create_workspace_engine
from storage.repositories._json import dump_json
from tools import ToolExecutionContext
from tools.render_tools import preview, status
from tools.specs import RenderPreviewInput, RenderStatusInput

NOW = "2026-07-05T00:00:00+00:00"


def test_render_preview_handler_enqueues_draft_job(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.connect() as connection:
        result = preview(RenderPreviewInput(), _context(connection, _draft_state()))

    assert result.status == "running"
    assert result.events[0]["event"] == "JobEnqueued"
    assert result.events[0]["payload"]["kind"] == "render_preview"
    assert result.events[0]["draft_id"] == "draft_1"
    assert result.data["draft_id"] == "draft_1"


def test_render_status_reports_artifacts_and_running_jobs(tmp_path: Path) -> None:
    engine = _engine(tmp_path)
    with engine.begin() as connection:
        connection.execute(
            schema.objects.insert().values(
                hash="a" * 64,
                rel_path=f"{'a' * 2}/{'a' * 2}/{'a' * 64}",
                size=10,
                created_at=NOW,
            )
        )
        connection.execute(
            schema.previews.insert().values(
                preview_id="preview_1",
                draft_id="draft_1",
                timeline_version=1,
                object_hash="a" * 64,
                quality=dump_json({"name": "preview"}),
                created_at=NOW,
            )
        )
        connection.execute(
            schema.jobs.insert().values(
                job_id="job_render",
                kind="render_preview",
                status="running",
                draft_id="draft_1",
                requested_by_draft_id="draft_1",
                asset_id=None,
                idempotency_key="render",
                payload_json=dump_json({"arguments": {}}),
                result_json=None,
                error_json=None,
                attempts=0,
                max_retries=0,
                next_run_at=NOW,
                progress=0.5,
                worker_id=None,
                heartbeat_at=None,
                created_at=NOW,
                started_at=NOW,
                finished_at=None,
            )
        )
    with engine.connect() as connection:
        draft_state = _draft_state(preview_current_id="preview_1")
        result = status(RenderStatusInput(), _context(connection, draft_state))

    assert result.status == "succeeded"
    assert result.data["preview_current_id"] == "preview_1"
    assert result.data["previews"][0]["current"] is True
    assert result.data["running_jobs"][0]["progress"] == 0.5


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
                running_jobs="[]",
                brief=dump_json({"goal": "test", "confirmed_facts": []}),
                timeline_current_version=1,
                timeline_validated=True,
                rough_cut_approved=False,
                scratch_memory="{}",
                created_at=NOW,
                updated_at=NOW,
            )
        )
    return engine


def _context(connection: Connection, draft_state: DraftState) -> ToolExecutionContext:
    return ToolExecutionContext(
        tool_call_id="tc_1",
        turn_id="turn_1",
        draft_state=draft_state,
        readonly_connection=connection,
        created_at=NOW,
    )


def _draft_state(*, preview_current_id: str | None = None) -> DraftState:
    return DraftState.model_validate(
        {
            "draft_id": "draft_1",
            "name": "Draft",
            "brief": {"goal": "test", "confirmed_facts": []},
            "timeline_current_version": 1,
            "timeline_validated": True,
            "preview_current_id": preview_current_id,
        }
    )
