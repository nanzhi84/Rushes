package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	write *sql.DB
	read  *sql.DB
	Paths Paths
}

func Open(ctx context.Context, workspace string) (*DB, error) {
	paths, err := NewPaths(workspace)
	if err != nil {
		return nil, err
	}
	if err := paths.Initialize(); err != nil {
		return nil, err
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
		return nil, err
	}
	write.SetMaxOpenConns(1)
	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(max(runtime.NumCPU(), 2))
	database := &DB{write: write, read: read, Paths: paths}
	if err := database.Migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := database.read.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("打开 SQLite 读池: %w", err)
	}
	return database, nil
}

func (database *DB) Write() *sql.DB { return database.write }

func (database *DB) Read() *sql.DB { return database.read }

func (database *DB) Close() error {
	return errors.Join(database.read.Close(), database.write.Close())
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
	}
	return nil
}
