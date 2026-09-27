package instanceid

import (
	"os"
	"path/filepath"
	"sync"
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != fileName {
		t.Fatalf("unexpected files after instance-id creation: %v", entries)
	}
}

func TestLoadOrCreateConcurrentCallersShareIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	const callers = 32

	start := make(chan struct{})
	ids := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := LoadOrCreate(dir)
			ids <- id
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("LoadOrCreate failed: %v", err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent callers returned different IDs: got %q, want %q", id, want)
		}
	}
}

func TestLoadOrCreateRejectsIncompleteExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("LoadOrCreate accepted an incomplete instance-id file")
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
