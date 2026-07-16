//go:build darwin || linux

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOpenCleansOnlyOldUntrackedObjectFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	paths := database.Paths

	oldOrphanHash := strings.Repeat("a", 64)
	trackedHash := strings.Repeat("b", 64)
	freshOrphanHash := strings.Repeat("c", 64)
	validDirectoryHash := strings.Repeat("d", 64)
	oldOrphan := writeObjectFixture(t, paths, oldOrphanHash, false)
	tracked := writeObjectFixture(t, paths, trackedHash, false)
	freshOrphan := writeObjectFixture(t, paths, freshOrphanHash, false)
	validDirectory := writeObjectFixture(t, paths, validDirectoryHash, true)
	invalidPath := filepath.Join(paths.Objects, "wrong", "layout", strings.Repeat("e", 64))
	if err := os.MkdirAll(filepath.Dir(invalidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalidPath, []byte("invalid layout"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-25 * time.Hour)
	for _, path := range []string{oldOrphan, tracked, validDirectory, invalidPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Write().ExecContext(t.Context(), `
		INSERT INTO objects(hash,rel_path,size,created_at) VALUES(?,?,?,?)`,
		trackedHash, filepath.ToSlash(filepath.Join(trackedHash[:2], trackedHash[2:4], trackedHash)),
		len("fixture"), old.Format(time.RFC3339Nano),
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
	if _, err := os.Stat(oldOrphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan still exists: %v", err)
	}
	for _, path := range []string{tracked, freshOrphan, validDirectory, invalidPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path removed %s: %v", path, err)
		}
	}
}

func TestOpenSkipsObjectGCWhileAnotherDatabaseUsesWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	oldOrphan := writeObjectFixture(t, first.Paths, strings.Repeat("f", 64), false)
	old := time.Now().UTC().Add(-25 * time.Hour)
	if err := os.Chtimes(oldOrphan, old, old); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := os.Stat(oldOrphan); err != nil {
		t.Fatalf("concurrent workspace open ran object GC: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	if _, err := os.Stat(oldOrphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exclusive startup did not remove old orphan: %v", err)
	}
}

func TestOpenContinuesWhenObjectGCLockIsUnavailable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths, err := NewPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	orphan := writeObjectFixture(t, paths, strings.Repeat("9", 64), false)
	old := time.Now().UTC().Add(-25 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, objectGCLockFilename), 0o700); err != nil {
		t.Fatal(err)
	}

	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatalf("object GC lock failure must not block startup: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("GC must be skipped without a safety lock: %v", err)
	}
}

func TestSafeObjectRemovalDoesNotFollowReplacedShardDirectory(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("8", 64)
	original := writeObjectFixture(t, paths, hash, false)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, hash[2:4], hash)
	if err := os.MkdirAll(filepath.Dir(outsideFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(paths.Objects, hash[:2])); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(paths.Objects, hash[:2])); err != nil {
		t.Fatal(err)
	}

	removed, err := removeOrphanObjectFileSafely(paths.Objects, original, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("symlinked shard must not be removed")
	}
	if content, err := os.ReadFile(outsideFile); err != nil || string(content) != "outside" {
		t.Fatalf("outside object changed: content=%q err=%v", content, err)
	}
}

func TestSafeObjectRemovalRejectsInvalidAndSymlinkTargets(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeOrphanObjectFileSafely(paths.Objects, filepath.Join(paths.Objects, "invalid"), time.Now().UTC()); err != nil || removed {
		t.Fatalf("invalid object path: removed=%v err=%v", removed, err)
	}
	hash := strings.Repeat("7", 64)
	symlink, err := paths.ObjectPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(symlink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), symlink); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeOrphanObjectFileSafely(paths.Objects, symlink, time.Now().UTC()); err != nil || removed {
		t.Fatalf("symlink target: removed=%v err=%v", removed, err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink target changed: info=%v err=%v", info, err)
	}
	sentinel := errors.New("permission denied")
	if err := ignoreObjectPathRace(sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("non-race error must remain visible: %v", err)
	}
}

func TestWorkspaceObjectGCLockWaitHonorsContext(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	first, exclusive, err := acquireWorkspaceObjectGCLock(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeWorkspaceObjectGCLock(first) }()
	if !exclusive {
		t.Fatal("first lock must be exclusive")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	second, _, err := acquireWorkspaceObjectGCLock(ctx, paths)
	if second != nil {
		_ = closeWorkspaceObjectGCLock(second)
		t.Fatal("cancelled waiter acquired lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v want context.Canceled", err)
	}
	shared, err := transitionWorkspaceObjectGCLockToShared(t.Context(), paths, first)
	if err != nil {
		t.Fatalf("transition to shared lock: %v", err)
	}
	first = shared
	third, thirdExclusive, err := acquireWorkspaceObjectGCLock(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeWorkspaceObjectGCLock(third) }()
	if thirdExclusive {
		t.Fatal("shared lifetime lock allowed a concurrent exclusive lock")
	}
}

func TestWorkspaceObjectSharedTransitionHonorsContext(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	blocker, exclusive, err := acquireWorkspaceObjectGCLock(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeWorkspaceObjectGCLock(blocker) }()
	if !exclusive {
		t.Fatal("first lock must be exclusive")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	shared, err := transitionWorkspaceObjectGCLockToShared(ctx, paths, nil)
	if shared != nil {
		_ = closeWorkspaceObjectGCLock(shared)
		t.Fatal("cancelled transition acquired a shared lock")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("transition error=%v want context.Canceled", err)
	}
}

func TestObjectGCSafetyErrorPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	database.cleanupOrphanObjectsBestEffort(cancelled)

	uppercaseHash := strings.Repeat("A", 64)
	uppercasePath := filepath.Join(database.Paths.Objects, uppercaseHash[:2], uppercaseHash[2:4], uppercaseHash)
	if validObjectFilePath(database.Paths.Objects, uppercasePath) {
		t.Fatal("uppercase object hash must be rejected")
	}
	missingRoot := filepath.Join(t.TempDir(), "missing")
	lowercaseHash := strings.Repeat("6", 64)
	missingPath := filepath.Join(missingRoot, lowercaseHash[:2], lowercaseHash[2:4], lowercaseHash)
	if _, err := removeOrphanObjectFileSafely(missingRoot, missingPath, time.Now().UTC()); err == nil {
		t.Fatal("missing objects root must be visible")
	}
	firstShard := filepath.Join(database.Paths.Objects, lowercaseHash[:2])
	if err := os.MkdirAll(firstShard, 0o755); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(firstShard, lowercaseHash[2:4], lowercaseHash)
	if removed, err := removeOrphanObjectFileSafely(database.Paths.Objects, validPath, time.Now().UTC()); err != nil || removed {
		t.Fatalf("missing second shard: removed=%v err=%v", removed, err)
	}
	if err := os.MkdirAll(filepath.Dir(validPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeOrphanObjectFileSafely(database.Paths.Objects, validPath, time.Now().UTC()); err != nil || removed {
		t.Fatalf("missing leaf: removed=%v err=%v", removed, err)
	}
}

func TestQuarantineRestoreNeverOverwritesCurrentPath(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("5", 64)
	current := writeObjectFixture(t, paths, hash, false)
	rootFD, err := unix.Open(paths.Objects, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(rootFD) }()
	firstFD, err := openObjectDirectoryAt(rootFD, hash[:2])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(firstFD) }()
	secondFD, err := openObjectDirectoryAt(firstFD, hash[2:4])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(secondFD) }()
	quarantineName, quarantineFD, err := createObjectGCQuarantine(secondFD)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Close(quarantineFD)
		_ = os.RemoveAll(filepath.Join(filepath.Dir(current), quarantineName))
	}()
	quarantined := filepath.Join(filepath.Dir(current), quarantineName, hash)
	if err := os.WriteFile(quarantined, []byte("quarantined"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameObjectNoReplace(quarantineFD, hash, secondFD, hash); !errors.Is(err, unix.EEXIST) {
		t.Fatal("restore must not overwrite an existing object path")
	}
	if content, err := os.ReadFile(current); err != nil || string(content) != "fixture" {
		t.Fatalf("current object changed: content=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(quarantined); err != nil || string(content) != "quarantined" {
		t.Fatalf("quarantined object lost: content=%q err=%v", content, err)
	}
}

func TestOpenRecoversInterruptedObjectGC(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	paths := database.Paths
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("4", 64)
	quarantine := filepath.Join(paths.Objects, hash[:2], hash[2:4], objectGCQuarantinePrefix+strings.Repeat("a", 24))
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	interrupted := filepath.Join(quarantine, hash)
	if err := os.WriteFile(interrupted, []byte("fresh after crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{objectGCProbeSource, objectGCProbeDestination} {
		if err := os.WriteFile(filepath.Join(quarantine, name), []byte("stale probe"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	database, err = Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	canonical, err := paths.ObjectPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(canonical); err != nil || string(content) != "fresh after crash" {
		t.Fatalf("interrupted object was not restored: content=%q err=%v", content, err)
	}
	if _, err := os.Stat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty quarantine was not removed: %v", err)
	}
}

func TestObjectGCRecoveryErrorPaths(t *testing.T) {
	t.Parallel()
	if err := recoverObjectGCQuarantines(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing objects root must be visible")
	}
	root := t.TempDir()
	if validObjectGCQuarantinePath(root, filepath.Join(root, "aa", "aa", objectGCQuarantinePrefix+"short")) {
		t.Fatal("short quarantine suffix must be rejected")
	}
	symlinkRoot := filepath.Join(t.TempDir(), "objects-link")
	if err := os.Symlink(t.TempDir(), symlinkRoot); err != nil {
		t.Fatal(err)
	}
	quarantineName := objectGCQuarantinePrefix + strings.Repeat("a", 24)
	if err := recoverObjectGCQuarantine(symlinkRoot, filepath.Join(symlinkRoot, "aa", "aa", quarantineName)); err == nil {
		t.Fatal("symlinked objects root must be rejected")
	}

	paths, err := NewPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	quarantinePath := filepath.Join(paths.Objects, "aa", "aa", quarantineName)
	if err := recoverObjectGCQuarantine(paths.Objects, quarantinePath); err != nil {
		t.Fatalf("missing first shard is a harmless race: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Objects, "aa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recoverObjectGCQuarantine(paths.Objects, quarantinePath); err != nil {
		t.Fatalf("missing second shard is a harmless race: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Objects, "aa", "aa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recoverObjectGCQuarantine(paths.Objects, quarantinePath); err != nil {
		t.Fatalf("missing quarantine is a harmless race: %v", err)
	}
}

func TestObjectGCRecoveryPreservesConflictsAndInvalidEntries(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("3", 64)
	current := writeObjectFixture(t, paths, hash, false)
	quarantine := filepath.Join(filepath.Dir(current), objectGCQuarantinePrefix+strings.Repeat("b", 24))
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(quarantine, hash)
	if err := os.WriteFile(candidate, []byte("quarantined"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(quarantine, "not-an-object-hash")
	if err := os.WriteFile(invalid, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverObjectGCQuarantines(paths.Objects); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(current); err != nil || string(content) != "fixture" {
		t.Fatalf("current object changed: content=%q err=%v", content, err)
	}
	for _, path := range []string{candidate, invalid} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery discarded conflicting data %s: %v", path, err)
		}
	}
}

func TestObjectGCRecoveryReportsPermissionFailures(t *testing.T) {
	t.Parallel()
	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("2", 64)
	shard := filepath.Join(paths.Objects, hash[:2], hash[2:4])
	quarantine := filepath.Join(shard, objectGCQuarantinePrefix+strings.Repeat("c", 24))
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quarantine, hash), []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shard, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := recoverObjectGCQuarantine(paths.Objects, quarantine); err == nil {
		t.Fatal("read-only shard must surface restore failure")
	}
	if err := os.Chmod(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(quarantine, hash)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shard, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := recoverObjectGCQuarantine(paths.Objects, quarantine); err == nil {
		t.Fatal("read-only shard must surface quarantine cleanup failure")
	}
	if err := os.Chmod(shard, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceObjectSharedTransitionClosesOnSetupErrors(t *testing.T) {
	t.Parallel()
	t.Run("replacement open", func(t *testing.T) {
		paths, err := NewPaths(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := paths.Initialize(); err != nil {
			t.Fatal(err)
		}
		lock, _, err := acquireWorkspaceObjectGCLock(t.Context(), paths)
		if err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(paths.Root, objectGCLockFilename)
		if err := os.Remove(lockPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(lockPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if shared, err := transitionWorkspaceObjectGCLockToShared(t.Context(), paths, lock); err == nil || shared != nil {
			t.Fatalf("transition unexpectedly succeeded: shared=%v err=%v", shared, err)
		}
	})
	t.Run("old close", func(t *testing.T) {
		paths, err := NewPaths(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := paths.Initialize(); err != nil {
			t.Fatal(err)
		}
		lock, _, err := acquireWorkspaceObjectGCLock(t.Context(), paths)
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.file.Close(); err != nil {
			t.Fatal(err)
		}
		if shared, err := transitionWorkspaceObjectGCLockToShared(t.Context(), paths, lock); err == nil || shared != nil {
			t.Fatalf("transition unexpectedly succeeded: shared=%v err=%v", shared, err)
		}
	})
}

func TestCleanupReportsQuarantineRecoveryFailure(t *testing.T) {
	t.Parallel()
	database, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := os.RemoveAll(database.Paths.Objects); err != nil {
		t.Fatal(err)
	}
	if _, err := database.cleanupOrphanObjects(t.Context(), time.Now().UTC()); err == nil {
		t.Fatal("missing objects root must fail recovery before deletion")
	}
}

func TestObjectGCPrimitiveFailuresLeaveCanonicalDataUntouched(t *testing.T) {
	t.Parallel()
	if file, _, _, err := openObjectCandidate(-1, "missing"); err == nil || file != nil {
		t.Fatalf("invalid candidate FD: file=%v err=%v", file, err)
	}
	if err := probeObjectNoReplace(-1); err == nil {
		t.Fatal("invalid probe FD must fail")
	}
	if name, fd, err := createObjectGCQuarantine(-1); err == nil || name != "" || fd != -1 {
		t.Fatalf("invalid quarantine FD: name=%q fd=%d err=%v", name, fd, err)
	}

	paths, err := NewPaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Initialize(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("1", 64)
	canonical := writeObjectFixture(t, paths, hash, false)
	old := time.Now().UTC().Add(-25 * time.Hour)
	if err := os.Chtimes(canonical, old, old); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Dir(canonical)
	if err := os.Chmod(shard, 0o500); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeOrphanObjectFileSafely(paths.Objects, canonical, time.Now().UTC().Add(-24*time.Hour)); err == nil || removed {
		t.Fatalf("read-only shard: removed=%v err=%v", removed, err)
	}
	if err := os.Chmod(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical object changed after setup failure: %v", err)
	}

	probeQuarantine := filepath.Join(shard, objectGCQuarantinePrefix+strings.Repeat("e", 24))
	for _, name := range []string{objectGCProbeSource, objectGCProbeDestination} {
		if err := os.MkdirAll(filepath.Join(probeQuarantine, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverObjectGCQuarantine(paths.Objects, probeQuarantine); err == nil {
		t.Fatal("non-file probe residue must be visible")
	}
}

func writeObjectFixture(t *testing.T, paths Paths, hash string, directory bool) string {
	t.Helper()
	path, err := paths.ObjectPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	if directory {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
