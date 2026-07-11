package storage

import (
	"context"
	"os"
	"testing"
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
	count, err := CountTables(t.Context(), database.Read())
	if err != nil {
		t.Fatal(err)
	}
	if count != 13 {
		t.Fatalf("业务表数=%d want=13", count)
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
