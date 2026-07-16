package storage

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	orphanObjectMinimumAge = 24 * time.Hour
	objectGCLockFilename   = ".rushes-object-gc.lock"
)

func (database *DB) cleanupOrphanObjectsBestEffort(ctx context.Context) {
	deleted, err := database.cleanupOrphanObjects(ctx, time.Now().UTC())
	if err != nil {
		slog.Warn("清理孤儿对象文件失败", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("已清理孤儿对象文件", "deleted", deleted)
	}
}

func (database *DB) cleanupOrphanObjects(ctx context.Context, now time.Time) (int, error) {
	if err := recoverObjectGCQuarantines(database.Paths.Objects); err != nil {
		return 0, err
	}
	deleted := 0
	cutoff := now.Add(-orphanObjectMinimumAge)
	err := filepath.WalkDir(database.Paths.Objects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !validObjectFilePath(database.Paths.Objects, path) {
			return nil
		}
		hash := entry.Name()
		var found int
		err := database.read.QueryRowContext(ctx, "SELECT 1 FROM objects WHERE hash=? LIMIT 1", hash).Scan(&found)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		removed, err := removeOrphanObjectFileSafely(database.Paths.Objects, path, cutoff)
		if err != nil {
			return err
		}
		if removed {
			deleted++
		}
		return nil
	})
	return deleted, err
}

func validObjectFilePath(objectsRoot, path string) bool {
	relative, err := filepath.Rel(objectsRoot, path)
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 {
		return false
	}
	hash := parts[2]
	if len(hash) != 64 || parts[0] != hash[:2] || parts[1] != hash[2:4] {
		return false
	}
	return lowercaseHex(hash)
}

func lowercaseHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
