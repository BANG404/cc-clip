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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return read(path)
		}
		return "", fmt.Errorf("create instance-id file: %w", err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write instance-id file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("sync instance-id file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close instance-id file: %w", err)
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
