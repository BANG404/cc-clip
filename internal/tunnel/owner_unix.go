//go:build !windows

package tunnel

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether the current uid owns the inode.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}
