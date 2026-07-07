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
    Column("usable", Boolean, nullable=False),
    Column("failure", Text, nullable=True),
    # Spec C：便宜本地索引与 agentic 理解的冗余展示列（纯加法）。
    Column("thumbnail_object_hash", Text, ForeignKey("objects.hash"), nullable=True),
    Column("index_json", Text, nullable=True),
    Column("understanding_status", Text, nullable=False, server_default="none"),
)

project_asset_links = Table(
    "project_asset_links",
    metadata,
    Column("project_id", Text, ForeignKey("projects.project_id"), primary_key=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), primary_key=True),
    Column("enabled", Boolean, nullable=False),
    Column("linked_at", Text, nullable=False),
    Column("note", Text, nullable=False),
    # 文件夹导入时相对所选根目录的子路径（含所选目录名），素材面板按它分组；直接导入的文件为 NULL。
    Column("rel_dir", Text, nullable=True),
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

material_summaries = Table(
    "material_summaries",
    metadata,
    Column("summary_id", Text, primary_key=True),
    Column("asset_id", Text, ForeignKey("assets.asset_id"), nullable=False),
    Column("version", Integer, nullable=False),
    Column("focus", Text, nullable=True),
    Column("status", Text, nullable=False),
    Column("summary_json", Text, nullable=False),
    Column("model", Text, nullable=True),
    Column("created_at", Text, nullable=False),
)
Index(
    "ux_material_summaries_asset_version",
    material_summaries.c.asset_id,
    material_summaries.c.version,
    unique=True,
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
    Column("kind", Text, nullable=False, server_default="reply"),
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
    "transcripts",
    "material_summaries",
    "decisions",
    "timeline_versions",
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


def create_all(connection: Connection) -> None:
    metadata.create_all(connection)


def drop_all(connection: Connection) -> None:
    metadata.drop_all(connection)


def expected_columns(table_name: str) -> Sequence[str]:
    table = metadata.tables[table_name]
    return tuple(column.name for column in table.columns)
