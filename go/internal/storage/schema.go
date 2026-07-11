package storage

const schemaVersion = 1

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
