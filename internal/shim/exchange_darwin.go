//go:build darwin

package shim

import "golang.org/x/sys/unix"

// exchangePrograms atomically swaps two existing directory entries.
func exchangePrograms(sidecar, path string) error {
	return unix.RenamexNp(sidecar, path, unix.RENAME_SWAP)
}
