package tunnelmgr

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMasterArgs pins the exact ssh invocation of the managed master. The
// option set is the safety boundary: BatchMode keeps a background supervisor
// off interactive prompts, ClearAllForwardings drops the user's own config
// forwards before exactly one managed reverse forward is added, and the
// ControlMaster socket is a path we generated — never a user master.
func TestMasterArgs(t *testing.T) {
	b := &Backend{Spec: Spec{Host: "example-host", Port: 18339}}
	args := b.MasterArgs("/ctl/dir/ctl-ab12.sock")

	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"-N", "-T",
		"BatchMode=yes",
		"ClearAllForwardings=yes",
		"ExitOnForwardFailure=yes",
		"ControlMaster=yes",
		"ControlPath=/ctl/dir/ctl-ab12.sock",
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
		"-R",
		"127.0.0.1:18339:127.0.0.1:18339",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("master argv missing %q; got %q", want, args)
		}
	}
	if args[len(args)-1] != "example-host" {
		t.Errorf("host must be the final argument, got %q", args[len(args)-1])
	}
	// Exactly one reverse forward may be requested.
	if got := strings.Count(joined, "-R\x00"); got != 1 {
		t.Errorf("master argv requests %d reverse forwards, want 1", got)
	}
}

func TestProbeArgs(t *testing.T) {
	b := &Backend{Spec: Spec{Host: "example-host", Port: 18339}}
	cmd := "remote probe script"
	args := b.probeArgs("/ctl/dir/ctl-ab12.sock", cmd)

	joined := strings.Join(args, "\x00")
	for _, want := range []string{"BatchMode=yes", "ClearAllForwardings=yes", "ControlPath=/ctl/dir/ctl-ab12.sock"} {
		if !strings.Contains(joined, want) {
			t.Errorf("probe argv missing %q; got %q", want, args)
		}
	}
	// The probe session must not open or close any forwarding of its own.
	if strings.Contains(joined, "-R") || strings.Contains(joined, "-N") {
		t.Errorf("probe argv must not carry forwarding flags: %q", args)
	}
	if args[len(args)-1] != cmd {
		t.Errorf("probe command must be the final argument, got %q", args[len(args)-1])
	}
	if args[len(args)-2] != "example-host" {
		t.Errorf("probe target host wrong: %q", args)
	}
}

func TestNewControlSocketPathUniqueAndInsideDir(t *testing.T) {
	dir := t.TempDir()
	first, err := newControlSocketPath(dir)
	if err != nil {
		t.Fatalf("first path: %v", err)
	}
	second, err := newControlSocketPath(dir)
	if err != nil {
		t.Fatalf("second path: %v", err)
	}
	if first == second {
		t.Fatalf("control socket paths must be unique per start, got %q twice", first)
	}
	if filepath.Dir(first) != dir {
		t.Fatalf("socket path %q escapes control dir %q", first, dir)
	}
	// Reserve leaves no file behind: ssh creates the socket itself.
	if _, err := os.Stat(first); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reserved path must not exist, stat err = %v", err)
	}
}
