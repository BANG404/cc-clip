//go:build windows

package tunnelmgr

import (
	"fmt"
	"os"
	"path/filepath"
)

func newDefaultControlDir() (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve control socket directory: %w", err)
	}
	dir := filepath.Join(home, defaultControlDirName)
	if err := ensurePrivateDir(dir); err != nil {
		return "", false, err
	}
	return dir, false, nil
}

func controlDirOwnedByCurrentUser(os.FileInfo) bool {
	return true
}
