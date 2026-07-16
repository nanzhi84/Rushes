//go:build darwin || linux

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func recoverObjectGCQuarantines(objectsRoot string) error {
	return filepath.WalkDir(objectsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || !validObjectGCQuarantinePath(objectsRoot, path) {
			return nil
		}
		if err := recoverObjectGCQuarantine(objectsRoot, path); err != nil {
			return err
		}
		return filepath.SkipDir
	})
}

func validObjectGCQuarantinePath(objectsRoot, path string) bool {
	relative, err := filepath.Rel(objectsRoot, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !strings.HasPrefix(parts[2], objectGCQuarantinePrefix) {
		return false
	}
	suffix := strings.TrimPrefix(parts[2], objectGCQuarantinePrefix)
	if len(suffix) != 24 {
		return false
	}
	return lowercaseHex(parts[0] + parts[1] + suffix)
}

func recoverObjectGCQuarantine(objectsRoot, path string) error {
	relative, err := filepath.Rel(objectsRoot, path)
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	rootFD, err := unix.Open(objectsRoot, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootFD) }()
	firstFD, err := openObjectDirectoryAt(rootFD, parts[0])
	if err != nil {
		return ignoreObjectPathRace(err)
	}
	defer func() { _ = unix.Close(firstFD) }()
	secondFD, err := openObjectDirectoryAt(firstFD, parts[1])
	if err != nil {
		return ignoreObjectPathRace(err)
	}
	defer func() { _ = unix.Close(secondFD) }()
	quarantineFD, err := openObjectDirectoryAt(secondFD, parts[2])
	if err != nil {
		return ignoreObjectPathRace(err)
	}
	quarantine := os.NewFile(uintptr(quarantineFD), parts[2])
	if quarantine == nil {
		_ = unix.Close(quarantineFD)
		return errors.New("创建对象 GC 隔离目录句柄失败")
	}
	if err := cleanupObjectGCProbe(quarantineFD); err != nil {
		_ = quarantine.Close()
		return err
	}
	entries, err := quarantine.ReadDir(-1)
	if err != nil {
		_ = quarantine.Close()
		return err
	}
	probed := false
	for _, entry := range entries {
		canonical := filepath.Join(objectsRoot, parts[0], parts[1], entry.Name())
		if !validObjectFilePath(objectsRoot, canonical) {
			continue
		}
		if !probed {
			if err := probeObjectNoReplace(quarantineFD); err != nil {
				_ = quarantine.Close()
				return err
			}
			probed = true
		}
		if err := renameObjectNoReplace(quarantineFD, entry.Name(), secondFD, entry.Name()); err != nil && !errors.Is(err, unix.EEXIST) && !errors.Is(err, unix.ENOENT) {
			_ = quarantine.Close()
			return err
		}
	}
	if err := quarantine.Close(); err != nil {
		return err
	}
	if err := unix.Unlinkat(secondFD, parts[2], unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTEMPTY) {
		return err
	}
	return nil
}
