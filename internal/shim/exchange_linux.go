//go:build linux

package shim

import "golang.org/x/sys/unix"

// exchangePrograms atomically swaps two existing directory entries.
func exchangePrograms(sidecar, path string) error {
	return unix.Renameat2(unix.AT_FDCWD, sidecar, unix.AT_FDCWD, path, unix.RENAME_EXCHANGE)
}

// renameProgramNoReplace moves an entry only if the destination is vacant.
func renameProgramNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
