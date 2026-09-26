//go:build !linux && !darwin

package shim

import "syscall"

// Refuse restoration on platforms without safe directory-entry primitives.
func exchangePrograms(_, _ string) error {
	return syscall.ENOSYS
}

func renameProgramNoReplace(_, _ string) error {
	return syscall.ENOSYS
}
