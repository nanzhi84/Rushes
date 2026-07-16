//go:build !darwin && !linux

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenSkipsObjectGCOnUnsupportedPlatforms(t *testing.T) {
	root := t.TempDir()
	paths, err := NewPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	path, err := paths.ObjectPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-25 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unsupported platform must skip object GC: %v", err)
	}
}
