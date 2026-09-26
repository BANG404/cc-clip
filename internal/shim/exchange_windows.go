//go:build windows

package shim

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows has no atomic exchange, so restoring an adopted program is refused.
func exchangePrograms(_, _ string) error {
	return syscall.ENOSYS
}

// renameProgramNoReplace moves an entry only if the destination is vacant:
// MoveFileEx without MOVEFILE_REPLACE_EXISTING fails on an existing target.
func renameProgramNoReplace(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, 0)
}
