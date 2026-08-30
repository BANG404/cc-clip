//go:build !windows

package tunnelmgr

import (
	"os"
	"syscall"
)

// terminateProcess asks the managed ssh child to shut down gracefully. ssh
// handles SIGTERM by closing the connection, which releases the remote
// forward and removes its control socket.
func terminateProcess(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
