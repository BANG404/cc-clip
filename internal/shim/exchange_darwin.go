//go:build darwin

package shim

import "golang.org/x/sys/unix"

// exchangePrograms atomically swaps two existing directory entries.
func exchangePrograms(sidecar, path string) error {
	return unix.RenamexNp(sidecar, path, unix.RENAME_SWAP)
}

// renameProgramNoReplace moves an entry only if the destination is vacant.
func renameProgramNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
