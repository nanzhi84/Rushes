package storage

const schemaVersion = 7

const schemaV1 = `
CREATE TABLE IF NOT EXISTS drafts (
    draft_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    state_version INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'active',
    defaults_json TEXT NOT NULL DEFAULT '{}',
    pending_decision_id TEXT,
    running_jobs_json TEXT NOT NULL DEFAULT '[]',
    last_error_json TEXT,
    brief_json TEXT NOT NULL DEFAULT '{"goal":""}',
    content_plan_json TEXT,
    timeline_current_version INTEGER,
    timeline_validated INTEGER NOT NULL DEFAULT 0,
    preview_current_id TEXT,
    last_viewed_preview_id TEXT,
    export_current_id TEXT,
    scratch_memory_json TEXT NOT NULL DEFAULT '{}',
    messages_tail_ref TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS objects (
    hash TEXT PRIMARY KEY,
    rel_path TEXT NOT NULL,
    size INTEGER NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assets (
    asset_id TEXT PRIMARY KEY,
    storage_mode TEXT NOT NULL,
    object_hash TEXT REFERENCES objects(hash),
    reference_path TEXT,
    kind TEXT NOT NULL,
    source TEXT NOT NULL,
    filename TEXT NOT NULL,
    hash TEXT NOT NULL,
    mtime INTEGER,
    size INTEGER NOT NULL,
    probe_json TEXT,
    proxy_object_hash TEXT REFERENCES objects(hash),
    thumbnail_object_hash TEXT REFERENCES objects(hash),
    ingest_status TEXT NOT NULL,
    understanding_status TEXT NOT NULL DEFAULT 'none',
    usable INTEGER NOT NULL DEFAULT 1,
    failure_json TEXT
);

CREATE TABLE IF NOT EXISTS draft_asset_links (
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE CASCADE,
    linked_at TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    rel_dir TEXT,
    PRIMARY KEY (draft_id, asset_id)
);

CREATE TABLE IF NOT EXISTS transcripts (
    transcript_id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    raw_preserved INTEGER NOT NULL,
    utterances_json TEXT NOT NULL,
    vad_segments_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS material_summaries (
    summary_id TEXT PRIMARY KEY,
    asset_id TEXT NOT NULL REFERENCES assets(asset_id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    focus TEXT,
    status TEXT NOT NULL,
    summary_json TEXT NOT NULL,
    model TEXT,
    fingerprint TEXT,
    prompt_version TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(asset_id, version)
);

CREATE TABLE IF NOT EXISTS decisions (
    decision_id TEXT PRIMARY KEY,
    scope_type TEXT NOT NULL,
    draft_id TEXT REFERENCES drafts(draft_id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    question TEXT NOT NULL,
    options_json TEXT NOT NULL,
    allow_free_text INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL,
    answer_json TEXT,
    pending_tool_call_json TEXT,
    pending_tool_call_status TEXT,
    consumed_at TEXT,
    replayed_tool_call_id TEXT,
    blocking INTEGER NOT NULL,
    created_by_tool_call_id TEXT
);

CREATE TABLE IF NOT EXISTS timeline_versions (
    timeline_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    parent_version INTEGER,
    created_by_patch_id TEXT,
    document_json TEXT NOT NULL,
    validation_report_json TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(draft_id, version)
);

CREATE TABLE IF NOT EXISTS previews (
    preview_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    timeline_version INTEGER NOT NULL,
    object_hash TEXT NOT NULL REFERENCES objects(hash),
    quality_json TEXT NOT NULL,
    render_width INTEGER,
    render_height INTEGER,
    render_fps REAL,
    expected_duration_sec REAL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS exports (
    export_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    timeline_version INTEGER NOT NULL,
    object_hash TEXT NOT NULL REFERENCES objects(hash),
    quality_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    message_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'reply',
    content TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
    job_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    draft_id TEXT REFERENCES drafts(draft_id) ON DELETE CASCADE,
    requested_by_draft_id TEXT REFERENCES drafts(draft_id) ON DELETE CASCADE,
    asset_id TEXT REFERENCES assets(asset_id) ON DELETE SET NULL,
    idempotency_key TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    result_json TEXT,
    error_json TEXT,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 0,
    next_run_at TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 100,
    progress REAL,
    worker_id TEXT,
    heartbeat_at TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    UNIQUE(kind, idempotency_key)
);

CREATE INDEX IF NOT EXISTS ix_jobs_claim
ON jobs(status, next_run_at, priority, created_at);

CREATE TABLE IF NOT EXISTS event_log (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,
    actor TEXT NOT NULL,
    draft_id TEXT REFERENCES drafts(draft_id) ON DELETE CASCADE,
    payload_json TEXT NOT NULL,
    merge_key TEXT,
    state_version INTEGER,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_event_log_cursor ON event_log(event_id);
CREATE INDEX IF NOT EXISTS ix_event_log_draft_cursor ON event_log(draft_id, event_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_event_log_merge
ON event_log(event_type, merge_key) WHERE merge_key IS NOT NULL;
`

// schemaV2 把时间线持久化收敛为“每个草稿仅保留当前版本”。版本号仍然
// 单调递增，用于并发校验和渲染产物关联，但不再作为可浏览、可恢复的历史。
const schemaV2 = `
CREATE TABLE timeline_versions_v2 (
    timeline_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL UNIQUE REFERENCES drafts(draft_id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    created_by_patch_id TEXT,
    document_json TEXT NOT NULL,
    validation_report_json TEXT,
    created_at TEXT NOT NULL
);

INSERT INTO timeline_versions_v2(
    timeline_id, draft_id, version, created_by_patch_id,
    document_json, validation_report_json, created_at
)
SELECT
    current.timeline_id, current.draft_id, current.version, current.created_by_patch_id,
    current.document_json, current.validation_report_json, current.created_at
FROM timeline_versions AS current
WHERE current.version = COALESCE(
    (SELECT drafts.timeline_current_version FROM drafts WHERE drafts.draft_id = current.draft_id),
    (SELECT MAX(latest.version) FROM timeline_versions AS latest WHERE latest.draft_id = current.draft_id)
);

DROP TABLE timeline_versions;
ALTER TABLE timeline_versions_v2 RENAME TO timeline_versions;
`

// schemaV3 清理已删除的版本恢复功能留下的领域事件，避免旧工作区在 SSE
// 重放时持续产生未知事件错误。
const schemaV3 = `
DELETE FROM event_log WHERE event_type = 'TimelineVersionRestored';
`

// schemaV4 只保存当前时间线之外的紧凑语义编辑批次，供下一次 Agent 回合理解
// 人工剪辑意图。它不是时间线快照，也不能用于版本恢复；每个草稿最多保留
// 最近 20 批，具体裁剪在 reducer 的同一事务里完成。
const schemaV4 = `
CREATE TABLE IF NOT EXISTS timeline_edit_batches (
    edit_batch_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    actor TEXT NOT NULL,
    origin TEXT NOT NULL,
    operations_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_timeline_edit_batches_draft
ON timeline_edit_batches(draft_id, created_at, edit_batch_id);
`

// schemaV5 accelerates the persistent understanding cache. The table already
// carried fingerprint from v1, so no data rewrite is required.
const schemaV5 = `
CREATE INDEX IF NOT EXISTS ix_material_summaries_fingerprint
ON material_summaries(asset_id, fingerprint, status);
`

// schemaV6 stores the model-facing context window independently from the
// visible conversation. A checkpoint is a replacement-history boundary: the
// current objective WorldState is stored as a reference snapshot, while older
// dialogue is represented by one compact handoff summary.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS agent_context_checkpoints (
    draft_id TEXT PRIMARY KEY REFERENCES drafts(draft_id) ON DELETE CASCADE,
    window_id TEXT NOT NULL,
    window_number INTEGER NOT NULL,
    history_version INTEGER NOT NULL,
    base_snapshot_json TEXT NOT NULL,
    base_snapshot_hash TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    compacted_through_message_id TEXT REFERENCES messages(message_id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`

// schemaV7 允许保留同一草稿的多个不可变时间线快照，使已入队渲染任务
// 能在当前时间线继续演进后仍读取其 payload 指定的版本。
const schemaV7 = `
CREATE TABLE timeline_versions_v7 (
    timeline_id TEXT PRIMARY KEY,
    draft_id TEXT NOT NULL REFERENCES drafts(draft_id) ON DELETE CASCADE,
    version INTEGER NOT NULL,
    created_by_patch_id TEXT,
    document_json TEXT NOT NULL,
    validation_report_json TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(draft_id, version)
);

INSERT INTO timeline_versions_v7(
    timeline_id, draft_id, version, created_by_patch_id,
    document_json, validation_report_json, created_at
)
SELECT
    timeline_id, draft_id, version, created_by_patch_id,
    document_json, validation_report_json, created_at
FROM timeline_versions;

DROP TABLE timeline_versions;
ALTER TABLE timeline_versions_v7 RENAME TO timeline_versions;
`
