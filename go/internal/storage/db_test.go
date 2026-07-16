package storage

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenMigratesSchemaAndCreatesWorkspace(t *testing.T) {
	t.Parallel()

	database, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	for _, path := range []string{
		database.Paths.Objects, database.Paths.Cache, database.Paths.Segments,
		database.Paths.Temporary, database.Paths.Logs,
	} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("workspace 目录缺失 %s: %v", path, err)
		}
	}
	var version int
	if err := database.Read().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version=%d", version)
	}
	var count int
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 18 {
		t.Fatalf("业务表数=%d want=18", count)
	}
	batches, err := ListTimelineEditBatches(t.Context(), database.Read(), "missing", 20)
	if err != nil || len(batches) != 0 {
		t.Fatalf("timeline_edit_batches migration missing: batches=%#v err=%v", batches, err)
	}
	if err := database.Migrate(t.Context()); err != nil {
		t.Fatalf("迁移必须幂等: %v", err)
	}
}

func TestObjectPathValidation(t *testing.T) {
	t.Parallel()

	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paths.ObjectPath("short"); err == nil {
		t.Fatal("短 hash 应拒绝")
	}
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path, err := paths.ObjectPath(hash)
	if err != nil || path == "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

func TestOpenMigratesTimelineHistoryAndAllowsFutureSnapshots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(root, "rushes.db")}).String()
	legacy, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacy.Exec(`
		INSERT INTO drafts(draft_id,name,created_at,updated_at,timeline_current_version)
		VALUES('draft_migrate','draft',?,?,2)`, now, now); err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 2; version++ {
		if _, err := legacy.Exec(`
			INSERT INTO timeline_versions(timeline_id,draft_id,version,parent_version,document_json,created_at)
			VALUES(?,?,?,?,?,?)`, "timeline_"+string(rune('0'+version)), "draft_migrate", version, version-1, `{}`, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := legacy.Exec(`
		INSERT INTO event_log(event_type,actor,draft_id,payload_json,created_at)
		VALUES('TimelineVersionRestored','user','draft_migrate','{}',?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var count, version int
	if err := database.Read().QueryRow(`
		SELECT COUNT(*), MAX(version) FROM timeline_versions WHERE draft_id='draft_migrate'`).Scan(&count, &version); err != nil {
		t.Fatal(err)
	}
	if count != 1 || version != 2 {
		t.Fatalf("timeline rows=%d version=%d", count, version)
	}
	if err := database.Read().QueryRow(`
		SELECT COUNT(*) FROM event_log WHERE event_type='TimelineVersionRestored'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy restore events=%d err=%v", count, err)
	}
	if _, err := database.Write().Exec(`
		INSERT INTO timeline_versions(timeline_id,draft_id,version,document_json,created_at)
		VALUES('timeline_3','draft_migrate',3,'{}',?)`, now); err != nil {
		t.Fatalf("应允许保存后续不可变时间线快照: %v", err)
	}
	if err := database.Read().QueryRow(`
		SELECT COUNT(*), MAX(version) FROM timeline_versions WHERE draft_id='draft_migrate'`).Scan(&count, &version); err != nil || count != 2 || version != 3 {
		t.Fatalf("timeline rows=%d version=%d err=%v", count, version, err)
	}
	columns, err := database.Read().Query("PRAGMA table_info(timeline_versions)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = columns.Close() }()
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "parent_version" {
			t.Fatal("迁移后不应保留版本父链字段")
		}
	}
}

func TestV8MigrationStartsAgentBridgeAfterHistoricalEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO drafts(draft_id,name,created_at,updated_at)
		VALUES('draft_bridge_migrate','draft',?,?);
		INSERT INTO event_log(event_type,actor,draft_id,payload_json,created_at)
		VALUES('JobSucceeded','job','draft_bridge_migrate',?,?);
		DROP TABLE agent_job_observations;
		DROP TABLE agent_job_observation_suppressions;
		DROP TABLE agent_job_bridge_state;
		PRAGMA user_version = 7`, now, now,
		`{"event":"JobSucceeded","draft_id":"draft_bridge_migrate","payload":{"job_id":"historical_job","kind":"render_preview"}}`, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var cursor, maxEventID int64
	if err := database.Read().QueryRowContext(t.Context(), `
		SELECT last_event_id FROM agent_job_bridge_state WHERE consumer_id='agent'`,
	).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(event_id),0) FROM event_log`).Scan(&maxEventID); err != nil {
		t.Fatal(err)
	}
	if cursor != maxEventID || cursor == 0 {
		t.Fatalf("bridge cursor=%d max_event_id=%d", cursor, maxEventID)
	}
	var observations int
	if err := database.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM agent_job_observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("historical observations=%d err=%v", observations, err)
	}
}

func TestV10MigrationIndexesOnlyUndeliveredAgentObservations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		DROP INDEX ix_agent_job_observations_undelivered_event;
		PRAGMA user_version = 9`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var plan string
	if err := database.Read().QueryRowContext(t.Context(), `
		EXPLAIN QUERY PLAN
		SELECT event_id,job_id,draft_id,event_json,claim_token
		FROM agent_job_observations
		WHERE delivered_at IS NULL AND event_id>?
		ORDER BY event_id LIMIT ?`, 0, 100,
	).Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "ix_agent_job_observations_undelivered_event") {
		t.Fatalf("未交付扫描未使用 partial index: %s", plan)
	}
}
