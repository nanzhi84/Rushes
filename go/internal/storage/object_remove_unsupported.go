//go:build !darwin && !linux

package storage

import "time"

func removeOrphanObjectFileSafely(string, string, time.Time) (bool, error) { return false, nil }

func recoverObjectGCQuarantines(string) error { return nil }
