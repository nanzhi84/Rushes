//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type workspaceObjectLock struct {
	file *os.File
}

func acquireWorkspaceObjectGCLock(ctx context.Context, paths Paths) (*workspaceObjectLock, bool, error) {
	file, err := os.OpenFile(filepath.Join(paths.Root, objectGCLockFilename), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	lock := &workspaceObjectLock{file: file}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return lock, true, nil
	} else if !lockWouldBlock(err) {
		_ = file.Close()
		return nil, false, err
	}

	for {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
			return lock, false, nil
		} else if !lockWouldBlock(err) {
			_ = file.Close()
			return nil, false, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func lockWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func transitionWorkspaceObjectGCLockToShared(ctx context.Context, paths Paths, lock *workspaceObjectLock) (*workspaceObjectLock, error) {
	replacement, err := os.OpenFile(filepath.Join(paths.Root, objectGCLockFilename), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = closeWorkspaceObjectGCLock(lock)
		return nil, err
	}
	if err := closeWorkspaceObjectGCLock(lock); err != nil {
		_ = replacement.Close()
		return nil, err
	}
	for {
		if err := syscall.Flock(int(replacement.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
			return &workspaceObjectLock{file: replacement}, nil
		} else if !lockWouldBlock(err) {
			_ = replacement.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = replacement.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func closeWorkspaceObjectGCLock(lock *workspaceObjectLock) error {
	if lock == nil {
		return nil
	}
	return lock.file.Close()
}
