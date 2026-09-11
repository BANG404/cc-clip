//go:build windows

package tunnel

import "os"

// ownedByCurrentUser is not enforced on Windows: the per-user temp directory
// is already isolated there, and Go exposes no owner SID through os.FileInfo.
func ownedByCurrentUser(os.FileInfo) bool {
	return true
}
