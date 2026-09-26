//go:build !linux && !darwin

package shim

import "syscall"

// Request uninstallShim's os.Rename fallback on platforms without an exchange
// primitive. That fallback retains the documented concurrent-replacement race.
func exchangePrograms(_, _ string) error {
	return syscall.ENOSYS
}
