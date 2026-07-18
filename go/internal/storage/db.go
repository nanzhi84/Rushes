package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

type Paths struct {
	Root      string
	DB        string
	Objects   string
	Cache     string
	Segments  string
	Temporary string
	Logs      string
}

func NewPaths(root string) (Paths, error) {
	resolved, err := filepath.Abs(root)
	if err != nil {
		return Paths{}, err
	}
	cache := filepath.Join(resolved, "cache")
	return Paths{
		Root:      resolved,
		DB:        filepath.Join(resolved, "rushes.db"),
		Objects:   filepath.Join(resolved, "objects"),
		Cache:     cache,
		Segments:  filepath.Join(cache, "segments"),
		Temporary: filepath.Join(resolved, "tmp"),
		Logs:      filepath.Join(resolved, "logs"),
	}, nil
}

func (paths Paths) Initialize() error {
	for _, directory := range []string{
		paths.Root, paths.Objects, paths.Cache, paths.Segments, paths.Temporary, paths.Logs,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func (paths Paths) ObjectPath(hash string) (string, error) {
	if len(hash) != 64 {
		return "", errors.New("object hash 必须是 64 位 SHA-256")
	}
	return filepath.Join(paths.Objects, hash[:2], hash[2:4], hash), nil
}

type DB struct {
	write         *sql.DB
	read          *sql.DB
	workspaceLock *workspaceObjectLock
	Paths         Paths
}

func Open(ctx context.Context, workspace string) (*DB, error) {
	paths, err := NewPaths(workspace)
	if err != nil {
		return nil, err
	}
	if err := paths.Initialize(); err != nil {
		return nil, err
	}
	workspaceLock, runObjectGC, lockErr := acquireWorkspaceObjectGCLock(ctx, paths)
	if lockErr != nil {
		slog.Warn("无法安全锁定对象存储，跳过孤儿文件清理", "error", lockErr)
		workspaceLock = nil
		runObjectGC = false
	}
	u := &url.URL{Scheme: "file", Path: paths.DB}
	dsn := u.String() +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	write, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = closeWorkspaceObjectGCLock(workspaceLock)
		return nil, err
	}
	write.SetMaxOpenConns(1)
	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		_ = closeWorkspaceObjectGCLock(workspaceLock)
		return nil, err
	}
	read.SetMaxOpenConns(max(runtime.NumCPU(), 2))
	database := &DB{write: write, read: read, workspaceLock: workspaceLock, Paths: paths}
	if err := database.Migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := database.read.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("打开 SQLite 读池: %w", err)
	}
	if runObjectGC {
		database.cleanupOrphanObjectsBestEffort(ctx)
		sharedLock, err := transitionWorkspaceObjectGCLockToShared(ctx, paths, workspaceLock)
		database.workspaceLock = sharedLock
		if err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("建立 workspace 对象存储共享锁: %w", err)
		}
	}
	return database, nil
}

func (database *DB) Write() *sql.DB { return database.write }

func (database *DB) Read() *sql.DB { return database.read }

func (database *DB) Close() error {
	readErr := database.read.Close()
	writeErr := database.write.Close()
	lockErr := closeWorkspaceObjectGCLock(database.workspaceLock)
	return errors.Join(readErr, writeErr, lockErr)
}

func (database *DB) Migrate(ctx context.Context) error {
	var version int
	if err := database.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("数据库版本 %d 高于程序支持版本 %d", version, schemaVersion)
	}
	if version == 0 {
		if _, err := database.write.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("应用 schema v1: %w", err)
		}
		if _, err := database.write.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return err
		}
		version = 1
	}
	if version < 2 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV2); err != nil {
			return fmt.Errorf("应用 schema v2: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 2
	}
	if version < 3 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("应用 schema v3: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 3
	}
	if version < 4 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV4); err != nil {
			return fmt.Errorf("应用 schema v4: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 4
	}
	if version < 5 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV5); err != nil {
			return fmt.Errorf("应用 schema v5: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 5"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 5
	}
	if version < 6 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV6); err != nil {
			return fmt.Errorf("应用 schema v6: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 6
	}
	if version < 7 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV7); err != nil {
			return fmt.Errorf("应用 schema v7: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 7"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 7
	}
	if version < 8 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV8); err != nil {
			return fmt.Errorf("应用 schema v8: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 8"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 8
	}
	if version < 9 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV9); err != nil {
			return fmt.Errorf("应用 schema v9: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 9"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 9
	}
	if version < 10 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV10); err != nil {
			return fmt.Errorf("应用 schema v10: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 10"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 10
	}
	if version < 11 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "timeline_versions", "parent_version"); err != nil {
			return fmt.Errorf("应用 schema v11 parent_version: %w", err)
		}
		if err := addColumnIfMissing(ctx, tx, "messages", "rewound_at"); err != nil {
			return fmt.Errorf("应用 schema v11 rewound_at: %w", err)
		}
		if err := addColumnIfMissing(ctx, tx, "messages", "rewind_checkpoint_id"); err != nil {
			return fmt.Errorf("应用 schema v11 rewind_checkpoint_id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV11); err != nil {
			return fmt.Errorf("应用 schema v11: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 11"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 11
	}
	if version < 12 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV12); err != nil {
			return fmt.Errorf("应用 schema v12: %w", err)
		}
		// Existing checkpoints predate branch snapshots. Their active prefix is
		// the only visibility information available during migration.
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rewind_checkpoint_messages(checkpoint_id,message_id)
			SELECT checkpoint.checkpoint_id,message.message_id
			FROM rewind_checkpoints AS checkpoint
			JOIN messages AS anchor ON anchor.message_id=checkpoint.anchor_message_id
			JOIN messages AS message ON message.draft_id=checkpoint.draft_id AND message.rowid<=anchor.rowid
			WHERE message.rewound_at IS NULL`); err != nil {
			return fmt.Errorf("迁移 rewind checkpoint messages: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 12"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 12
	}
	if version < 13 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, schemaV13); err != nil {
			return fmt.Errorf("应用 schema v13: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 13"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 13
	}
	if version < 14 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := dropColumnIfExists(ctx, tx, "drafts", "scratch_memory_json", schemaV14); err != nil {
			return fmt.Errorf("应用 schema v14: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 14"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 14
	}
	if version < 15 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "rewind_restore_requests", "new_message_id"); err != nil {
			return fmt.Errorf("应用 schema v15 new_message_id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaV15); err != nil {
			return fmt.Errorf("应用 schema v15: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 15"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 15
	}
	if version < 16 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "user_memories", "last_used_at"); err != nil {
			return fmt.Errorf("应用 schema v16: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 16"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 16
	}
	if version < 17 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "user_memories", "manually_revised_at"); err != nil {
			return fmt.Errorf("应用 schema v17: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 17"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 17
	}
	if version < 18 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "assets", "peaks_object_hash"); err != nil {
			return fmt.Errorf("应用 schema v18: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 18"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		version = 18
	}
	if version < 19 {
		tx, err := database.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := addColumnIfMissing(ctx, tx, "rewind_restore_requests", "affected_memories_json"); err != nil {
			return fmt.Errorf("应用 schema v19: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 19"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	column string,
) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, table, column,
	).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	allowed := map[string]string{
		"timeline_versions.parent_version":               "ALTER TABLE timeline_versions ADD COLUMN parent_version INTEGER",
		"messages.rewound_at":                            "ALTER TABLE messages ADD COLUMN rewound_at TEXT",
		"messages.rewind_checkpoint_id":                  "ALTER TABLE messages ADD COLUMN rewind_checkpoint_id TEXT",
		"rewind_restore_requests.new_message_id":         "ALTER TABLE rewind_restore_requests ADD COLUMN new_message_id TEXT",
		"user_memories.last_used_at":                     schemaV16,
		"user_memories.manually_revised_at":              schemaV17,
		"assets.peaks_object_hash":                       schemaV18,
		"rewind_restore_requests.affected_memories_json": schemaV19,
	}
	statement, ok := allowed[table+"."+column]
	if !ok {
		return fmt.Errorf("不允许的迁移列 %s.%s", table, column)
	}
	_, err := tx.ExecContext(ctx, statement)
	return err
}

func dropColumnIfExists(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	column string,
	statement string,
) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, table, column,
	).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	allowed := table == "drafts" && column == "scratch_memory_json" && statement == schemaV14
	if !allowed {
		return fmt.Errorf("不允许删除迁移列 %s.%s", table, column)
	}
	_, err := tx.ExecContext(ctx, statement)
	return err
}
