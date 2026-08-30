package instanceid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateIsStableAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("instance id not stable: first=%q second=%q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("instance-id mode = %o, want 600", got)
	}
}

func TestLoadOrCreatePreservesExistingSessionToken(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "session.token")
	wantToken := []byte("existing-token\n2030-01-01T00:00:00Z\n")
	if err := os.WriteFile(tokenPath, wantToken, 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("instance id changed across daemon-style restarts: first=%q second=%q", first, second)
	}

	gotToken, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotToken) != string(wantToken) {
		t.Fatalf("existing session token changed: got %q, want %q", gotToken, wantToken)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("session token mode = %o, want 600", got)
	}
}
