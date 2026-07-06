from pathlib import Path

from alembic import command
from alembic.config import Config

from storage import schema
from storage.db import create_workspace_engine

EXPECTED_COLUMNS: dict[str, tuple[str, ...]] = {
    "projects": ("project_id", "name", "status", "defaults", "created_at", "updated_at"),
    "cases": (
        "case_id",
        "project_id",
        "name",
        "state_version",
        "status",
        "pending_decision_id",
        "running_jobs",
        "last_error",
        "brief",
        "content_plan",
        "audio_plan",
        "cut_plan",
        "candidate_pack_id",
        "timeline_current_version",
        "timeline_validated",
        "preview_current_id",
        "last_viewed_preview_id",
        "rough_cut_approved",
        "rough_cut_approved_version",
        "postprocess_plan",
        "export_current_id",
        "selected_asset_ids",
        "disabled_asset_ids",
        "scratch_memory",
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
        "annotation_status",
        "annotation_pass",
        "index_status",
        "usable",
        "failure",
        "thumbnail_object_hash",
        "index_json",
        "understanding_status",
    ),
    "project_asset_links": ("project_id", "asset_id", "enabled", "linked_at", "note"),
    "annotations": (
        "annotation_id",
        "asset_id",
        "schema",
        "status",
        "document_json",
        "created_at",
        "updated_at",
    ),
    "annotation_clip_projection": (
        "clip_id",
        "annotation_id",
        "asset_id",
        "start_frame",
        "end_frame",
        "role",
        "summary",
        "keywords_json",
        "quality_score",
        "usable",
        "embedding",
    ),
    "annotation_signal_projection": (
        "signal_id",
        "clip_id",
        "namespace",
        "field",
        "value_text",
        "value_number",
        "confidence",
    ),
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
        "created_at",
    ),
    "decisions": (
        "decision_id",
        "scope_type",
        "project_id",
        "case_id",
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
        "case_id",
        "version",
        "parent_version",
        "created_by_patch_id",
        "document_json",
        "validation_report",
        "created_at",
    ),
    "candidate_packs": ("candidate_pack_id", "case_id", "slots", "created_at"),
    "previews": (
        "preview_id",
        "case_id",
        "timeline_version",
        "object_hash",
        "quality",
        "created_at",
    ),
    "exports": ("export_id", "case_id", "timeline_version", "object_hash", "quality", "created_at"),
    "memory_candidates": (
        "candidate_id",
        "case_id",
        "content",
        "suggested_scope",
        "status",
        "saved_memory_id",
        "created_at",
    ),
    "memories": (
        "memory_id",
        "scope",
        "project_id",
        "content",
        "tags",
        "created_from_case_id",
        "created_at",
    ),
    "messages": ("message_id", "case_id", "role", "kind", "content", "created_at"),
    "jobs": (
        "job_id",
        "kind",
        "status",
        "project_id",
        "case_id",
        "requested_by_case_id",
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
        "project_id",
        "case_id",
        "payload_json",
        "state_version",
        "created_at",
    ),
    "provider_calls": (
        "call_id",
        "provider_id",
        "capability",
        "model",
        "case_id",
        "job_id",
        "latency_ms",
        "usage_json",
        "cost_estimate",
        "status",
    ),
    "agent_traces": ("trace_id", "turn_id", "case_id", "seq", "kind", "payload_json", "created_at"),
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


def test_alembic_upgrade_head_creates_all_tables_and_fts(tmp_path: Path) -> None:
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
        assert "clip_fts" in names
        fts_columns = tuple(
            str(row[1]) for row in connection.exec_driver_sql("PRAGMA table_info(clip_fts)").all()
        )
        assert fts_columns == ("clip_id", "summary", "keywords", "retrieval_sentence", "ocr_text")
