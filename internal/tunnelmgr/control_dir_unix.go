//go:build !windows

package tunnelmgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const maxDefaultControlSocketPathLen = 90

func newDefaultControlDir() (string, bool, error) {
	var errs []error
	seen := make(map[string]struct{})
	for _, root := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir(), "/tmp"} {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		if !filepath.IsAbs(root) {
			errs = append(errs, fmt.Errorf("runtime root %q is not absolute", root))
			continue
		}
		dir, err := os.MkdirTemp(root, ".ccs-"+strconv.Itoa(os.Getuid())+"-")
		if err != nil {
			errs = append(errs, fmt.Errorf("create private control dir in %s: %w", root, err))
			continue
		}
		if err := ensurePrivateDir(dir); err != nil {
			_ = os.Remove(dir)
			errs = append(errs, err)
			continue
		}
		if !defaultControlPathFits(dir) {
			_ = os.Remove(dir)
			errs = append(errs, fmt.Errorf("control socket path under %s would exceed the safe length", root))
			continue
		}
		return dir, true, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		errs = append(errs, fmt.Errorf("resolve home control socket directory: %w", err))
	} else {
		dir := filepath.Join(home, defaultControlDirName)
		if !defaultControlPathFits(dir) {
			errs = append(errs, fmt.Errorf("home control socket path would exceed the safe length"))
		} else if err := ensurePrivateDir(dir); err != nil {
			errs = append(errs, err)
		} else {
			return dir, false, nil
		}
	}

	return "", false, fmt.Errorf("create a safe short control socket directory: %w", errors.Join(errs...))
}

func defaultControlPathFits(dir string) bool {
	const socketName = "ctl-0123456789abcdef.sock"
	return len(filepath.Join(dir, socketName)) <= maxDefaultControlSocketPathLen
}

func controlDirOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
