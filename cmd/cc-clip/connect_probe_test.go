package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

	mentions := filepath.Join(dir, "xclip-wrapper")
	if err := os.WriteFile(mentions, []byte("#!/bin/sh\n# my wrapper, works alongside cc-clip\nexec /usr/bin/xclip \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bash, "-c", shimPresenceCheck(mentions)).Run(); err == nil {
		t.Fatal("probe accepted a user wrapper that only mentions cc-clip")
	}

	if err := exec.Command(bash, "-c", shimPresenceCheck(filepath.Join(dir, "absent"))).Run(); err == nil {
		t.Fatal("probe accepted a missing file")
	}
}

// TestReadPidArgsShellNeedsNoPs runs the argv probe against a live process
// with ps unreachable, the busybox situation: `ps -p` failed there, so the
// bridge was never recognized for stop or restore.
func TestReadPidArgsShellNeedsNoPs(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc; the remote side is always Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// The trailing `; :` keeps bash from exec-ing sleep, so the bash process
	// itself carries the bridge-shaped argv.
	proc := exec.Command(bash, "-c", "sleep 30; :", "cc-clip", "x11-bridge", "--display", ":42", "--port", "29999")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.Process.Kill(); _ = proc.Wait() })

	// A PATH holding only tr: ps is unreachable, as `ps -p` is on busybox.
	trPath, err := exec.LookPath("tr")
	if err != nil {
		t.Skip("tr not available")
	}
	onlyTr := t.TempDir()
	if err := os.Symlink(trPath, filepath.Join(onlyTr, "tr")); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`PATH=%s; pid=%d; %s && printf '%%s' "$args"`, onlyTr, proc.Process.Pid, readPidArgsShell)
	out, err := exec.Command(bash, "-c", script).Output()
	if err != nil {
		t.Fatalf("probe failed without ps: %v", err)
	}
	display, port, ok := bridgeDisplayPort(string(out))
	if !strings.Contains(string(out), "cc-clip x11-bridge") || !ok || display != "42" || port != 29999 {
		t.Fatalf("probe read %q; want the bridge argv with display 42 and port 29999", out)
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
