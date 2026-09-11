package tunnel

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ensurePrivateOutDir creates dir 0700 and refuses to reuse anything it does
// not own.
//
// The fallback output directory lives under a shared /tmp on Linux when
// XDG_RUNTIME_DIR is unset, and MkdirAll happily adopts a directory (or a
// symlink to one) another local user created first. The saved image is 0600, so
// this was never a read of the user's clipboard — but the directory's owner
// could replace the file behind the path this function returns, so the agent
// would be handed a different image than the one that was copied.
func ensurePrivateOutDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return fmt.Errorf("failed to create output dir: %w", err)
	}
	err := os.Mkdir(dir, 0700)
	if err == nil {
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("failed to create output dir: %w", err)
	}

	// Lstat, not Stat: a symlink pointing at an attacker-owned directory must
	// not pass by inspecting its target.
	info, statErr := os.Lstat(dir)
	if statErr != nil {
		return fmt.Errorf("failed to inspect output dir: %w", statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("output dir %s is not a directory", dir)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("output dir %s is owned by another user; refusing to write there", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("output dir %s is group/world accessible and could not be tightened: %w", dir, err)
		}
	}
	return nil
}

// DefaultOutDir returns the directory fetched images are written to.
func DefaultOutDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "claude-images")
	}
	// os.TempDir() is shared between users on Linux, so the per-user runtime
	// directory's isolation has to be reproduced in the name. Ownership is
	// still verified before use; this only stops honest collisions.
	return filepath.Join(os.TempDir(), "claude-images-"+strconv.Itoa(os.Getuid()))
}
