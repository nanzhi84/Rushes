//go:build darwin || linux

package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	objectGCQuarantinePrefix = ".rushes-object-gc-"
	objectGCProbeSource      = ".no-replace-probe-source"
	objectGCProbeDestination = ".no-replace-probe-destination"
)

func removeOrphanObjectFileSafely(objectsRoot, path string, cutoff time.Time) (removed bool, resultErr error) {
	if !validObjectFilePath(objectsRoot, path) {
		return false, nil
	}
	relative, err := filepath.Rel(objectsRoot, path)
	if err != nil {
		return false, err
	}
	pathParts := strings.Split(filepath.ToSlash(relative), "/")

	rootFD, err := unix.Open(objectsRoot, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(rootFD) }()
	firstFD, err := openObjectDirectoryAt(rootFD, pathParts[0])
	if err != nil {
		return false, ignoreObjectPathRace(err)
	}
	defer func() { _ = unix.Close(firstFD) }()
	secondFD, err := openObjectDirectoryAt(firstFD, pathParts[1])
	if err != nil {
		return false, ignoreObjectPathRace(err)
	}
	defer func() { _ = unix.Close(secondFD) }()

	initialFile, initial, initialInfo, err := openObjectCandidate(secondFD, pathParts[2])
	if err != nil {
		return false, ignoreObjectPathRace(err)
	}
	defer func() { _ = initialFile.Close() }()
	if initial.Mode&unix.S_IFMT != unix.S_IFREG || initialInfo.ModTime().After(cutoff) {
		return false, nil
	}

	quarantineName, quarantineFD, err := createObjectGCQuarantine(secondFD)
	if err != nil {
		return false, err
	}
	if err := probeObjectNoReplace(quarantineFD); err != nil {
		_ = unix.Close(quarantineFD)
		_ = unix.Unlinkat(secondFD, quarantineName, unix.AT_REMOVEDIR)
		return false, err
	}
	quarantined := false
	defer func() {
		if quarantined {
			if restoreErr := renameObjectNoReplace(quarantineFD, pathParts[2], secondFD, pathParts[2]); restoreErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("恢复隔离对象 %s: %w", pathParts[2], restoreErr))
			}
		}
		_ = unix.Close(quarantineFD)
		_ = unix.Unlinkat(secondFD, quarantineName, unix.AT_REMOVEDIR)
	}()
	if err := unix.Renameat(secondFD, pathParts[2], quarantineFD, pathParts[2]); err != nil {
		return false, ignoreObjectPathRace(err)
	}
	quarantined = true

	candidateFile, candidate, candidateInfo, err := openObjectCandidate(quarantineFD, pathParts[2])
	if err != nil {
		return false, err
	}
	defer func() { _ = candidateFile.Close() }()
	if candidate.Mode&unix.S_IFMT != unix.S_IFREG || candidate.Dev != initial.Dev || candidate.Ino != initial.Ino || candidateInfo.ModTime().After(cutoff) {
		return false, nil
	}
	if err := unix.Unlinkat(quarantineFD, pathParts[2], 0); err != nil {
		return false, err
	}
	quarantined = false
	return true, nil
}

func openObjectCandidate(parentFD int, name string) (*os.File, unix.Stat_t, os.FileInfo, error) {
	fileFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, unix.Stat_t{}, nil, err
	}
	file := os.NewFile(uintptr(fileFD), name)
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, unix.Stat_t{}, nil, errors.New("创建对象文件句柄失败")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, nil, err
	}
	return file, stat, info, nil
}

func probeObjectNoReplace(directoryFD int) error {
	if err := cleanupObjectGCProbe(directoryFD); err != nil {
		return err
	}
	for _, name := range []string{objectGCProbeSource, objectGCProbeDestination} {
		fd, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL, 0o600)
		if err != nil {
			_ = cleanupObjectGCProbe(directoryFD)
			return err
		}
		_ = unix.Close(fd)
	}
	err := renameObjectNoReplace(directoryFD, objectGCProbeSource, directoryFD, objectGCProbeDestination)
	cleanupErr := cleanupObjectGCProbe(directoryFD)
	if !errors.Is(err, unix.EEXIST) {
		return errors.Join(fmt.Errorf("workspace 文件系统不支持原子 no-replace rename: %w", err), cleanupErr)
	}
	return cleanupErr
}

func cleanupObjectGCProbe(directoryFD int) error {
	var result error
	for _, name := range []string{objectGCProbeSource, objectGCProbeDestination} {
		if err := unix.Unlinkat(directoryFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func createObjectGCQuarantine(parentFD int) (string, int, error) {
	for range 4 {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return "", -1, err
		}
		name := objectGCQuarantinePrefix + hex.EncodeToString(random)
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", -1, err
		}
		fd, err := openObjectDirectoryAt(parentFD, name)
		if err != nil {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return "", -1, err
		}
		return name, fd, nil
	}
	return "", -1, errors.New("无法创建唯一的对象 GC 隔离目录")
}

func openObjectDirectoryAt(parentFD int, name string) (int, error) {
	return unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
}

func ignoreObjectPathRace(err error) error {
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) {
		return nil
	}
	return err
}
