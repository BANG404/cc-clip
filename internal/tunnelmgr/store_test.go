package tunnelmgr

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

func validRecord() *Record {
	return &Record{
		SchemaVersion: schemaVersion,
		Spec:          Spec{Host: "example-host", Port: 18339},
		Runtime:       Runtime{State: StateHealthy, RemoteState: tunnel.RemoteTunnelOK, UpdatedAt: time.Now().UTC()},
	}
}

func TestStoreRoundTripAndFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnels", "example-host.json")
	store := NewStoreAt(path)

	if _, err := store.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: want fs.ErrNotExist, got %v", err)
	}

	rec := validRecord()
	rec.Runtime.LastError = "stale forward on remote"
	if err := store.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != stateFileMode {
		t.Fatalf("state file mode = %o, want %o (may contain SSH target names)", got, stateFileMode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != stateDirMode {
		t.Fatalf("state dir mode = %o, want %o", got, stateDirMode)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Spec != rec.Spec {
		t.Fatalf("spec = %+v, want %+v", got.Spec, rec.Spec)
	}
	if got.Runtime.State != StateHealthy || got.Runtime.RemoteState != tunnel.RemoteTunnelOK {
		t.Fatalf("runtime = %+v, want healthy/ok", got.Runtime)
	}
	if got.Runtime.LastError != rec.Runtime.LastError {
		t.Fatalf("last_error = %q, want %q", got.Runtime.LastError, rec.Runtime.LastError)
	}
}

func TestStoreLoadRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStoreAt(path).Load(); err == nil || !strings.Contains(err.Error(), "parse tunnel state") {
		t.Fatalf("corrupt JSON: want parse error, got %v", err)
	}
}

func TestStoreLoadRejectsForeignSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data, _ := json.Marshal(func() any {
		r := validRecord()
		r.SchemaVersion = 99
		return r
	}())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewStoreAt(path).Load()
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("foreign schema: want error, got %v", err)
	}
}

func TestStoreLoadRejectsUnknownStates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Record)
	}{
		{"unknown state", func(r *Record) { r.Runtime.State = State("running") }},
		{"empty state", func(r *Record) { r.Runtime.State = State("") }},
		{"unknown remote state", func(r *Record) { r.Runtime.RemoteState = tunnel.RemoteTunnelState("maybe-ok") }},
		{"empty host", func(r *Record) { r.Spec.Host = "" }},
		{"port zero", func(r *Record) { r.Spec.Port = 0 }},
		{"port out of range", func(r *Record) { r.Spec.Port = 70000 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			data, _ := json.Marshal(func() any {
				r := validRecord()
				tc.mutate(r)
				return r
			}())
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStoreAt(path).Load(); err == nil {
				t.Fatalf("want fail-closed error for %s, got nil", tc.name)
			}
		})
	}
}

func TestStoreSaveRejectsInvalidRecord(t *testing.T) {
	store := NewStoreAt(filepath.Join(t.TempDir(), "state.json"))
	rec := validRecord()
	rec.Runtime.State = State("made-up")
	if err := store.Save(rec); err == nil {
		t.Fatal("save must refuse a record with an unknown state")
	}
}

func TestStateHealthyFailsClosed(t *testing.T) {
	for s, want := range map[State]bool{
		StateUnknown:      false,
		StateHealthy:      true,
		StateReconnecting: false,
		StateAuthRequired: false,
		StateHostKeyError: false,
		StateDegraded:     false,
		StateCrashLoop:    false,
		StateStopped:      false,
	} {
		if got := s.Healthy(); got != want {
			t.Fatalf("State(%q).Healthy() = %v, want %v", s, got, want)
		}
	}
	if _, err := ParseState("restarting-ish"); err == nil {
		t.Fatal("ParseState must reject unknown states")
	}
}

func TestEncodeHostKey(t *testing.T) {
	cases := map[string]string{
		"myserver":         "myserver",
		"user@venus":       "user%40venus",
		"root@192.168.1.5": "root%40192.168.1.5",
		"user@a:1":         "user%40a%3A1",
		"host/../etc":      "host%2F..%2Fetc",
	}
	for in, want := range cases {
		if got := encodeHostKey(in); got != want {
			t.Fatalf("encodeHostKey(%q) = %q, want %q", in, got, want)
		}
	}
	if encodeHostKey("user@a:1") == encodeHostKey("user@a_1") {
		t.Fatal("distinct hosts must not collide in the state file name")
	}
}

func TestDefaultStorePath(t *testing.T) {
	path, err := DefaultStorePath("user@venus")
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join(".cache/cc-clip/tunnels", "user%40venus.json")) {
		t.Fatalf("unexpected store path %q", path)
	}
}
