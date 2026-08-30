//go:build !windows

package tunnelmgr

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// TestBackendStopSignalsOnlyOwnedChild pins the process-ownership boundary:
// stopping the managed tunnel terminates the exact child this backend
// spawned and nothing else — no name matching, no PID hunting, no control
// socket it did not create.
func TestBackendStopSignalsOnlyOwnedChild(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "master")

	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	if err := b.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !b.Running() {
		t.Fatal("child should be running after Start")
	}

	// A long-running process that belongs to nobody in this package.
	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Skipf("cannot start unrelated probe process: %v", err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()

	b.Stop(2 * time.Second)
	if b.Running() {
		t.Fatal("owned child still running after Stop")
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process must survive Stop, got %v", err)
	}
	if _, err := os.Stat(b.ControlPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control socket must be removed on stop, stat err = %v", err)
	}
}

func TestMasterArgsHaveNoForwardAfterOpenSSHParsing(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("system ssh not available")
	}
	b := &Backend{Spec: Spec{Host: "example-host", Port: 18399, ExpectedInstanceID: "instance-123"}}
	args := append([]string{"-G"}, b.MasterArgs(filepath.Join(t.TempDir(), "control.sock"))...)
	out, err := exec.Command(ssh, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh -G failed: %v: %s", err, out)
	}
	if strings.Contains(string(out), "remoteforward ") {
		t.Fatalf("master unexpectedly contains a reverse forward:\n%s", out)
	}
}

// TestBackendStopWithoutStart verifies Stop is a no-op when nothing was ever
// started — there is no path from it to any process at all.
func TestBackendStopWithoutStart(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "master")

	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	b.Stop(time.Second) // must not panic, spawn, or signal anything
	if b.Running() || b.ControlPath() != "" {
		t.Fatalf("unexpected state after no-op stop: running=%v controlPath=%q", b.Running(), b.ControlPath())
	}
}

func TestBackendProbeRemoteClassifiesMarker(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "master")
	t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:ok\n")

	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	if err := b.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := b.WaitReady(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("wait ready: %v", err)
	}
	if err := b.Forward(context.Background()); err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer b.Stop(2 * time.Second)

	state, err := b.ProbeRemote(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if state != tunnel.RemoteTunnelOK {
		t.Fatalf("probe state = %q, want ok", state)
	}
}

// TestBackendProbeRemoteFailsClosed verifies that a probe which does not
// complete (transport error, unrecognized output) is never read as healthy.
func TestBackendProbeRemoteFailsClosed(t *testing.T) {
	t.Run("transport error stays unknown", func(t *testing.T) {
		fake := writeFakeSSH(t)
		t.Setenv("FAKE_SSH_MODE", "master")
		t.Setenv("FAKE_SSH_PROBE_OUT", "")
		t.Setenv("FAKE_SSH_PROBE_EXIT", "255")

		b := &Backend{
			Spec:       Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"},
			SSHBinary:  fake,
			ControlDir: t.TempDir(),
		}
		if err := b.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		defer b.Stop(2 * time.Second)

		state, err := b.ProbeRemote(context.Background(), 5*time.Second)
		if err == nil {
			t.Fatal("probe must report the transport failure")
		}
		if state != tunnel.RemoteTunnelUnknown {
			t.Fatalf("probe state = %q, want unknown (fail closed)", state)
		}
	})

	t.Run("no master started is an error", func(t *testing.T) {
		b := &Backend{Spec: Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"}, ControlDir: t.TempDir()}
		state, err := b.ProbeRemote(context.Background(), 2*time.Second)
		if err == nil {
			t.Fatal("probing without a master must fail")
		}
		if state.Healthy() {
			t.Fatal("probe state must never be healthy when no master exists")
		}
	})
}

func TestBackendLastExitCapturesStderr(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "authfail")

	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399, ExpectedInstanceID: "instance-123"},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	if err := b.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.Running() {
		t.Fatal("auth-failed child should exit on its own")
	}
	exit := b.LastExit()
	if exit == nil {
		t.Fatal("LastExit must be recorded after the child exits")
	}
	if exit.Code != 255 {
		t.Fatalf("exit code = %d, want 255", exit.Code)
	}
	if !strings.Contains(exit.StderrTail, "Permission denied") {
		t.Fatalf("stderr tail = %q, want the ssh auth diagnostic", exit.StderrTail)
	}
}
