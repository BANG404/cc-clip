// Package instanceid manages the installation identity exposed by the local
// daemon to managed-tunnel health checks.
package instanceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const fileName = "instance-id"

// LoadOrCreate returns the stable random identity stored in dir. The file is
// created once with mode 0600 and survives daemon restarts and upgrades.
func LoadOrCreate(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create instance-id directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure instance-id directory: %w", err)
	}

	path := filepath.Join(dir, fileName)
	if id, err := read(path); err == nil {
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary instance-id file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("secure temporary instance-id file: %w", err)
	}
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write temporary instance-id file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync temporary instance-id file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temporary instance-id file: %w", err)
	}

	// Publish only a fully written file. A hard link provides create-if-absent
	// semantics without exposing a partially written final path. If another
	// process wins the race, its linked file is already complete and safe to
	// read; unlike rename, this never replaces an established installation ID.
	if err := os.Link(tmpPath, path); err != nil {
		if os.IsExist(err) {
			return read(path)
		}
		return "", fmt.Errorf("publish instance-id file: %w", err)
	}
	return id, nil
}

func read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid instance-id file %s", path)
	}
	return id, nil
}
