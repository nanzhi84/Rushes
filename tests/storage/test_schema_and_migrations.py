from pathlib import Path

from alembic import command
from alembic.config import Config

from storage import schema
from storage.db import create_workspace_engine

EXPECTED_COLUMNS: dict[str, tuple[str, ...]] = {
    "drafts": (
        "draft_id",
        "name",
        "state_version",
        "status",
        "defaults",
        "pending_decision_id",
        "running_jobs",
        "last_error",
        "brief",
        "content_plan",
        "audio_plan",
        "cut_plan",
        "timeline_current_version",
        "timeline_validated",
        "preview_current_id",
        "last_viewed_preview_id",
        "rough_cut_approved",
        "rough_cut_approved_version",
        "postprocess_plan",
        "export_current_id",
        "scratch_memory",
        "messages_tail_ref",
        "created_at",
        "updated_at",
    ),
    "assets": (
        "asset_id",
        "storage_mode",
        "object_hash",
        "reference_path",
        "kind",
        "source",
        "filename",
        "hash",
        "mtime",
        "size",
        "probe",
        "proxy_object_hash",
        "ingest_status",
        "usable",
        "failure",
        "thumbnail_object_hash",
        "index_json",
        "understanding_status",
    ),
    "draft_asset_links": ("draft_id", "asset_id", "linked_at", "note", "rel_dir"),
    "transcripts": (
        "transcript_id",
        "asset_id",
        "provider_id",
        "raw_preserved",
        "utterances",
        "vad_segments",
    ),
    "material_summaries": (
        "summary_id",
        "asset_id",
        "version",
        "focus",
        "status",
        "summary_json",
        "model",
        "fingerprint",
        "prompt_version",
        "created_at",
    ),
    "decisions": (
        "decision_id",
        "scope_type",
        "draft_id",
        "type",
        "question",
        "options",
        "status",
        "answer",
        "pending_tool_call",
        "pending_tool_call_status",
        "consumed_at",
        "replayed_tool_call_id",
        "blocking",
        "created_by_tool_call_id",
    ),
    "timeline_versions": (
        "timeline_id",
        "draft_id",
        "version",
        "parent_version",
        "created_by_patch_id",
        "document_json",
        "validation_report",
        "created_at",
    ),
    "previews": (
        "preview_id",
        "draft_id",
        "timeline_version",
        "object_hash",
        "quality",
        "created_at",
    ),
    "exports": (
        "export_id",
        "draft_id",
        "timeline_version",
        "object_hash",
        "quality",
        "created_at",
    ),
    "memory_candidates": (
        "candidate_id",
        "draft_id",
        "content",
        "suggested_scope",
        "status",
        "saved_memory_id",
        "created_at",
    ),
    "memories": (
        "memory_id",
        "scope",
        "content",
        "tags",
        "created_from_draft_id",
        "created_at",
    ),
    "messages": ("message_id", "draft_id", "role", "kind", "content", "created_at"),
    "jobs": (
        "job_id",
        "kind",
        "status",
        "draft_id",
        "requested_by_draft_id",
        "asset_id",
        "idempotency_key",
        "payload_json",
        "result_json",
        "error_json",
        "attempts",
        "max_retries",
        "next_run_at",
        "progress",
        "worker_id",
        "heartbeat_at",
        "created_at",
        "started_at",
        "finished_at",
    ),
    "event_log": (
        "event_id",
        "event_type",
        "actor",
        "draft_id",
        "payload_json",
        "state_version",
        "created_at",
    ),
    "provider_calls": (
        "call_id",
        "provider_id",
        "capability",
        "model",
        "draft_id",
        "job_id",
        "latency_ms",
        "usage_json",
        "cost_estimate",
        "status",
    ),
    "agent_traces": (
        "trace_id",
        "turn_id",
        "draft_id",
        "seq",
        "kind",
        "payload_json",
        "created_at",
    ),
    "objects": ("hash", "rel_path", "size", "created_at"),
}


def test_schema_columns_match_prd_er_graph() -> None:
    assert tuple(EXPECTED_COLUMNS) == schema.ALL_TABLE_NAMES
    for table_name, columns in EXPECTED_COLUMNS.items():
        assert tuple(schema.expected_columns(table_name)) == columns


def test_pragmas_apply_on_each_connection(tmp_path: Path) -> None:
    engine = create_workspace_engine(tmp_path)

    for _ in range(2):
        with engine.connect() as connection:
            assert connection.exec_driver_sql("PRAGMA journal_mode").scalar_one() == "wal"
            assert connection.exec_driver_sql("PRAGMA foreign_keys").scalar_one() == 1
            assert connection.exec_driver_sql("PRAGMA busy_timeout").scalar_one() == 5000
            assert connection.exec_driver_sql("PRAGMA synchronous").scalar_one() == 1


def test_alembic_upgrade_head_creates_all_tables(tmp_path: Path) -> None:
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    config = Config(str(Path("packages/storage/migrations/alembic.ini").resolve()))
    config.set_main_option("script_location", str(Path("packages/storage/migrations").resolve()))
    config.set_main_option("sqlalchemy.url", f"sqlite+pysqlite:///{workspace / 'rushes.db'}")

    command.upgrade(config, "head")

    engine = create_workspace_engine(workspace)
    with engine.connect() as connection:
        names = {
            str(row[0])
            for row in connection.exec_driver_sql(
                "SELECT name FROM sqlite_master WHERE type IN ('table', 'view')"
            ).all()
        }
        assert set(schema.ALL_TABLE_NAMES).issubset(names)
