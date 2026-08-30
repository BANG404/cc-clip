//go:build windows

package tunnelmgr

import "os"

// terminateProcess stops the managed ssh child. Windows has no SIGTERM
// equivalent, so the only option is a hard kill; the managed tunnel path is
// not a supported Windows workflow in Phase 1A (Windows OpenSSH also lacks
// ControlMaster), but the supervisor still builds and terminates cleanly.
func terminateProcess(p *os.Process) error {
	return p.Kill()
}
