//go:build linux

package storage

import "golang.org/x/sys/unix"

func renameObjectNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.Renameat2(fromFD, from, toFD, to, unix.RENAME_NOREPLACE)
}
