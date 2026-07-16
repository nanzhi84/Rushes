//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package storage

import (
	"context"
	"errors"
)

type workspaceObjectLock struct{}

func acquireWorkspaceObjectGCLock(context.Context, Paths) (*workspaceObjectLock, bool, error) {
	return nil, false, errors.New("当前平台不支持 workspace 对象存储锁")
}

func transitionWorkspaceObjectGCLockToShared(context.Context, Paths, *workspaceObjectLock) (*workspaceObjectLock, error) {
	return nil, errors.New("当前平台不支持 workspace 对象存储锁")
}

func closeWorkspaceObjectGCLock(*workspaceObjectLock) error { return nil }
