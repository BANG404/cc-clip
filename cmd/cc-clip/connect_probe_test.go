package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shunmei/cc-clip/internal/shim"
)

// TestShimPresenceCheckRecognizesAnInstalledShim runs the actual probe command
// against an actual generated shim.
//
// The previous probe read only line 1 — the `#!/bin/bash` — while the ownership
// marker is on line 2, so it reported "shim missing despite cached state" on
// every connect and reinstalled a shim that was already correct.
func TestShimPresenceCheckRecognizesAnInstalledShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe is a remote POSIX shell command")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	dir := t.TempDir()
	installed := filepath.Join(dir, "xclip")
	if err := os.WriteFile(installed, []byte(shim.XclipShim(18339, "/usr/bin/xclip")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bash, "-c", shimPresenceCheck(installed)).Run(); err != nil {
		t.Fatalf("probe did not recognize an installed shim: %v", err)
	}

	foreign := filepath.Join(dir, "wl-paste")
	if err := os.WriteFile(foreign, []byte("#!/bin/sh\nexec /usr/bin/wl-paste \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bash, "-c", shimPresenceCheck(foreign)).Run(); err == nil {
		t.Fatal("probe accepted a file cc-clip did not write")
	}

	if err := exec.Command(bash, "-c", shimPresenceCheck(filepath.Join(dir, "absent"))).Run(); err == nil {
		t.Fatal("probe accepted a missing file")
	}
}

// TestBridgeDisplayPortReadsRunningArgs pins bridge identity to what it is
// actually serving. A PID-liveness-only check reused a bridge still bound to
// the previous --port, so `connect --codex --port <new>` silently kept talking
// to the old daemon.
func TestBridgeDisplayPortReadsRunningArgs(t *testing.T) {
	display, port, ok := bridgeDisplayPort("/home/u/.local/bin/cc-clip x11-bridge --display :42 --port 29999")
	if !ok {
		t.Fatal("expected the display and port to be readable from argv")
	}
	if display != "42" || port != 29999 {
		t.Fatalf("got display=%q port=%d, want 42/29999", display, port)
	}

	if _, _, ok := bridgeDisplayPort("/home/u/.local/bin/cc-clip x11-bridge --display :42"); ok {
		t.Fatal("argv without a port must not be treated as identifiable")
	}
}
