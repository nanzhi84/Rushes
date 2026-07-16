//go:build darwin

package storage

import "golang.org/x/sys/unix"

func renameObjectNoReplace(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}
