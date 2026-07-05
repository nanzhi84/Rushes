"""SQLAlchemy Core schema for the PRD §3.2 database."""

from __future__ import annotations

from collections.abc import Sequence

from sqlalchemy import (
    Boolean,
    Column,
    Float,
    ForeignKey,
    Index,
    Integer,
    LargeBinary,
    MetaData,
    Table,
    Text,
)
from sqlalchemy.engine import Connection

metadata = MetaData()

projects = Table(
    "projects",
    metadata,
    Column("project_id", Text, primary_key=True),
    Column("name", Text, nullable=False),
    Column("status", Text, nullable=False),
    Column("defaults", Text, nullable=False),
    Column("created_at", Text, nullable=False),
    Column("updated_at", Text, nullable=False),
)

cases = Table(
    "cases",
    metadata,
    Column("case_id", Text, primary_key=True),
    Column("project_id", Text, ForeignKey("projects.project_id"), nullable=False),
    Column("name", Text, nullable=False),
    Column("state_version", Integer, nullable=False, default=0),
    Column("status", Text, nullable=False),
    Column("pending_decision_id", Text, ForeignKey("decisions.decision_id"), nullable=True),
    Column("running_jobs", Text, nullable=False),
    Column("last_error", Text, nullable=True),
    Column("brief", Text, nullable=False),
    Column("content_plan", Text, nullable=True),
    Column("audio_plan", Text, nullable=True),
    Column("cut_plan", Text, nullable=True),
    Column(
        "candidate_pack_id",
        Text,
        ForeignKey("candidate_packs.candidate_pack_id"),
        nullable=True,
    ),
    Column("timeline_current_version", Integer, nullable=True),
    Column("timeline_validated", Boolean, nullable=False, default=False),
    Column("preview_current_id", Text, ForeignKey("previews.preview_id"), nullable=True),
    Column("last_viewed_preview_id", Text, nullable=True),
    Column("rough_cut_approved", Boolean, nullable=False, default=False),
    Column("rough_cut_approved_version", Integer, nullable=True),
    Column("postprocess_plan", Text, nullable=True),
    Column("export_current_id", Text, ForeignKey("exports.export_id"), nullable=True),
    Column("selected_asset_ids", Text, nullable=False),
    Column("disabled_asset_ids", Text, nullable=False),
    Column("scratch_memory", Text, nullable=False),
)

assets = Table(
    "assets",
    metadata,
    Column("asset_id", Text, primary_key=True),
    Column("storage_mode", Text, nullable=False),
    Column("object_hash", Text, ForeignKey("objects.hash"), nullable=True),
    Column("reference_path", Text, nullable=True),
    Column("kind", Text, nullable=False),
    Column("source", Text, nullable=False),
    Column("filename", Text, nullable=False, default="", server_default=""),
    Column("hash", Text, nullable=False),
    Column("mtime", Integer, nullable=True),
    Column("size", Integer, nullable=False),
    Column("probe", Text, nullable=True),
    Column("proxy_object_hash", Text, ForeignKey("objects.hash"), nullable=True),
    Column("ingest_status", Text, nullable=False),
    Column("annotation_status", Text, nullable=False),
    Column("annotation_pass", Text, nullable=False),
    Column("index_status", Text, nullable=False),
    Column("usable", Boolean, nullable=False),
    Column("failure", Text, nullable=True),
)

project_asset_links = Table(
    "project_asset_links",
    metadata,
    Column("project_id", Text, ForeignKey("projects.project_id"), primary_key=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), primary_key=True),
    Column("enabled", Boolean, nullable=False),
    Column("linked_at", Text, nullable=False),
    Column("note", Text, nullable=False),
)

annotations_table = Table(
    "annotations",
    metadata,
    Column("annotation_id", Text, primary_key=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), nullable=False),
    Column("schema", Text, nullable=False),
    Column("status", Text, nullable=False),
    Column("document_json", Text, nullable=False),
    Column("created_at", Text, nullable=False),
    Column("updated_at", Text, nullable=False),
)

annotation_clip_projection = Table(
    "annotation_clip_projection",
    metadata,
    Column("clip_id", Text, primary_key=True),
    Column("annotation_id", Text, ForeignKey("annotations.annotation_id"), nullable=False),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), nullable=False),
    Column("start_frame", Integer, nullable=False),
    Column("end_frame", Integer, nullable=False),
    Column("role", Text, nullable=False),
    Column("summary", Text, nullable=False),
    Column("keywords_json", Text, nullable=False),
    Column("quality_score", Float, nullable=True),
    Column("usable", Boolean, nullable=False),
    Column("embedding", LargeBinary, nullable=True),
)

annotation_signal_projection = Table(
    "annotation_signal_projection",
    metadata,
    Column("signal_id", Text, primary_key=True),
    Column("clip_id", Text, ForeignKey("annotation_clip_projection.clip_id"), nullable=False),
    Column("namespace", Text, nullable=False),
    Column("field", Text, nullable=False),
    Column("value_text", Text, nullable=True),
    Column("value_number", Float, nullable=True),
    Column("confidence", Float, nullable=True),
)

transcripts = Table(
    "transcripts",
    metadata,
    Column("transcript_id", Text, primary_key=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), nullable=False),
    Column("provider_id", Text, nullable=False),
    Column("raw_preserved", Boolean, nullable=False),
    Column("utterances", Text, nullable=False),
    Column("vad_segments", Text, nullable=False),
)

decisions = Table(
    "decisions",
    metadata,
    Column("decision_id", Text, primary_key=True),
    Column("scope_type", Text, nullable=False),
    Column("project_id", Text, ForeignKey("projects.project_id"), nullable=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("type", Text, nullable=False),
    Column("question", Text, nullable=False),
    Column("options", Text, nullable=False),
    Column("status", Text, nullable=False),
    Column("answer", Text, nullable=True),
    Column("pending_tool_call", Text, nullable=True),
    Column("pending_tool_call_status", Text, nullable=True),
    Column("consumed_at", Text, nullable=True),
    Column("replayed_tool_call_id", Text, nullable=True),
    Column("blocking", Boolean, nullable=False),
    Column("created_by_tool_call_id", Text, nullable=True),
)

timeline_versions = Table(
    "timeline_versions",
    metadata,
    Column("timeline_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("version", Integer, nullable=False),
    Column("parent_version", Integer, nullable=True),
    Column("created_by_patch_id", Text, nullable=True),
    Column("document_json", Text, nullable=False),
    Column("validation_report", Text, nullable=True),
    Column("created_at", Text, nullable=False),
)
Index(
    "ix_timeline_versions_case_version",
    timeline_versions.c.case_id,
    timeline_versions.c.version,
    unique=True,
)

candidate_packs = Table(
    "candidate_packs",
    metadata,
    Column("candidate_pack_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("slots", Text, nullable=False),
    Column("created_at", Text, nullable=False),
)

previews = Table(
    "previews",
    metadata,
    Column("preview_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("timeline_version", Integer, nullable=False),
    Column("object_hash", Text, ForeignKey("objects.hash"), nullable=False),
    Column("quality", Text, nullable=False),
    Column("created_at", Text, nullable=False),
)

exports = Table(
    "exports",
    metadata,
    Column("export_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("timeline_version", Integer, nullable=False),
    Column("object_hash", Text, ForeignKey("objects.hash"), nullable=False),
    Column("quality", Text, nullable=False),
    Column("created_at", Text, nullable=False),
)

memory_candidates = Table(
    "memory_candidates",
    metadata,
    Column("candidate_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("content", Text, nullable=False),
    Column("suggested_scope", Text, nullable=False),
    Column("status", Text, nullable=False),
    Column("saved_memory_id", Text, ForeignKey("memories.memory_id"), nullable=True),
    Column("created_at", Text, nullable=False),
)

memories = Table(
    "memories",
    metadata,
    Column("memory_id", Text, primary_key=True),
    Column("scope", Text, nullable=False),
    Column("project_id", Text, ForeignKey("projects.project_id"), nullable=True),
    Column("content", Text, nullable=False),
    Column("tags", Text, nullable=False),
    Column("created_from_case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("created_at", Text, nullable=False),
)

messages = Table(
    "messages",
    metadata,
    Column("message_id", Text, primary_key=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("role", Text, nullable=False),
    Column("content", Text, nullable=False),
    Column("created_at", Text, nullable=False),
)

jobs = Table(
    "jobs",
    metadata,
    Column("job_id", Text, primary_key=True),
    Column("kind", Text, nullable=False),
    Column("status", Text, nullable=False),
    Column("project_id", Text, ForeignKey("projects.project_id"), nullable=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("requested_by_case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), nullable=True),
    Column("idempotency_key", Text, nullable=False),
    Column("payload_json", Text, nullable=False),
    Column("result_json", Text, nullable=True),
    Column("error_json", Text, nullable=True),
    Column("attempts", Integer, nullable=False),
    Column("max_retries", Integer, nullable=False),
    Column("next_run_at", Text, nullable=False),
    Column("progress", Float, nullable=True),
    Column("worker_id", Text, nullable=True),
    Column("heartbeat_at", Text, nullable=True),
    Column("created_at", Text, nullable=False),
    Column("started_at", Text, nullable=True),
    Column("finished_at", Text, nullable=True),
)
Index("ux_jobs_kind_idempotency_key", jobs.c.kind, jobs.c.idempotency_key, unique=True)
Index("ix_jobs_claim", jobs.c.status, jobs.c.next_run_at, jobs.c.created_at)

event_log = Table(
    "event_log",
    metadata,
    Column("event_id", Integer, primary_key=True, autoincrement=True),
    Column("event_type", Text, nullable=False),
    Column("actor", Text, nullable=False),
    Column("project_id", Text, ForeignKey("projects.project_id"), nullable=True),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("payload_json", Text, nullable=False),
    Column("state_version", Integer, nullable=True),
    Column("created_at", Text, nullable=False),
)
Index("ix_event_log_cursor", event_log.c.event_id)
Index("ix_event_log_case_cursor", event_log.c.case_id, event_log.c.event_id)
Index("ix_event_log_project_cursor", event_log.c.project_id, event_log.c.event_id)

provider_calls = Table(
    "provider_calls",
    metadata,
    Column("call_id", Text, primary_key=True),
    Column("provider_id", Text, nullable=False),
    Column("capability", Text, nullable=False),
    Column("model", Text, nullable=False),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=True),
    Column("job_id", Text, ForeignKey("jobs.job_id"), nullable=True),
    Column("latency_ms", Integer, nullable=False),
    Column("usage_json", Text, nullable=False),
    Column("cost_estimate", Float, nullable=True),
    Column("status", Text, nullable=False),
)

agent_traces = Table(
    "agent_traces",
    metadata,
    Column("trace_id", Text, primary_key=True),
    Column("turn_id", Text, nullable=False),
    Column("case_id", Text, ForeignKey("cases.case_id"), nullable=False),
    Column("seq", Integer, nullable=False),
    Column("kind", Text, nullable=False),
    Column("payload_json", Text, nullable=False),
    Column("created_at", Text, nullable=False),
)
Index(
    "ix_agent_traces_case_turn_seq",
    agent_traces.c.case_id,
    agent_traces.c.turn_id,
    agent_traces.c.seq,
)

objects = Table(
    "objects",
    metadata,
    Column("hash", Text, primary_key=True),
    Column("rel_path", Text, nullable=False),
    Column("size", Integer, nullable=False),
    Column("created_at", Text, nullable=False),
)

ALL_TABLE_NAMES: tuple[str, ...] = (
    "projects",
    "cases",
    "assets",
    "project_asset_links",
    "annotations",
    "annotation_clip_projection",
    "annotation_signal_projection",
    "transcripts",
    "decisions",
    "timeline_versions",
    "candidate_packs",
    "previews",
    "exports",
    "memory_candidates",
    "memories",
    "messages",
    "jobs",
    "event_log",
    "provider_calls",
    "agent_traces",
    "objects",
)

FTS_TABLE_NAME = "clip_fts"
CREATE_CLIP_FTS_SQL = (
    "CREATE VIRTUAL TABLE IF NOT EXISTS clip_fts "
    "USING fts5(clip_id, summary, keywords, retrieval_sentence, ocr_text)"
)
DROP_CLIP_FTS_SQL = "DROP TABLE IF EXISTS clip_fts"


def create_all(connection: Connection) -> None:
    metadata.create_all(connection)
    create_fts(connection)


def create_fts(connection: Connection) -> None:
    connection.exec_driver_sql(CREATE_CLIP_FTS_SQL)


def drop_all(connection: Connection) -> None:
    connection.exec_driver_sql(DROP_CLIP_FTS_SQL)
    metadata.drop_all(connection)


def expected_columns(table_name: str) -> Sequence[str]:
    table = metadata.tables[table_name]
    return tuple(column.name for column in table.columns)
