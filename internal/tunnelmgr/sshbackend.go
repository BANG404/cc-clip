package tunnelmgr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// defaultControlDirName is the directory, under the user's home, that holds
// the managed tunnel's private ControlMaster sockets. The parent directory
// is created with mode 0700, so only the current user can reach the socket.
const defaultControlDirName = ".cache/cc-clip/tunnels"

// ExitInfo describes how the managed ssh child ended.
type ExitInfo struct {
	// Code is the child's exit status, or -1 if it could not be determined
	// (e.g. killed by a signal).
	Code int

	// StderrTail is the last chunk of the child's stderr, the raw material
	// for exit classification. It may name the SSH target.
	StderrTail string

	// Uptime is how long the child ran before exiting. The supervisor uses
	// it to reset the restart backoff once a connection proved stable.
	Uptime time.Duration
}

// Backend owns the managed tunnel's ssh child: it starts a private
// non-interactive ControlMaster with exactly one reverse forward, probes
// remote daemon health through that master's own control socket, and stops
// the child it started.
//
// Process-ownership boundary: the backend only ever signals the os.Process
// it spawned itself. It never looks up, matches, or kills ssh processes by
// name, PID file, or broad pattern, and it only ever speaks to the control
// socket path it generated — a user's own SSH master is never touched.
type Backend struct {
	// Spec is the tunnel to run.
	Spec Spec

	// SSHBinary is the ssh executable. Empty means "ssh" from PATH.
	SSHBinary string

	// ControlDir overrides the directory for the control socket. Empty means
	// ~/.cache/cc-clip/tunnels. Tests point it at a temp directory.
	ControlDir string

	mu          sync.Mutex
	cmd         *exec.Cmd
	controlPath string
	running     bool
	startedAt   time.Time
	exit        *ExitInfo
	done        chan struct{}
}

// Start spawns the managed ssh master. The child stays in the foreground
// (ssh -N) until it exits or is stopped; completion is reported through
// Running/LastExit.
//
// The child is intentionally NOT bound to a context: context cancellation
// kills via SIGKILL, which would strand the remote forward (the stale-sshd
// problem this supervisor exists to avoid). Shutdown goes through Stop,
// which terminates gracefully first.
func (b *Backend) Start() error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("managed ssh child already running")
	}
	dir := b.controlDir()
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("create control socket dir: %w", err)
	}
	controlPath, err := newControlSocketPath(dir)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	args := b.MasterArgs(controlPath)
	cmd := exec.Command(b.sshBinary(), args...)
	buf := &tailBuffer{max: 4096}
	cmd.Stdout = buf
	cmd.Stderr = buf
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("start ssh master: %w", err)
	}
	b.cmd = cmd
	b.controlPath = controlPath
	b.running = true
	b.startedAt = startedAt
	b.exit = nil
	done := make(chan struct{})
	b.done = done
	b.mu.Unlock()

	go b.reap(cmd, startedAt, controlPath, buf, done)
	return nil
}

// reap waits for the child and records how it ended. Exactly one goroutine
// per Start generation runs this.
func (b *Backend) reap(cmd *exec.Cmd, startedAt time.Time, controlPath string, buf *tailBuffer, done chan struct{}) {
	err := cmd.Wait()
	code := -1
	if err == nil {
		code = 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	b.mu.Lock()
	if b.cmd == cmd {
		b.running = false
		b.exit = &ExitInfo{
			Code:       code,
			StderrTail: strings.TrimSpace(buf.Tail()),
			Uptime:     time.Since(startedAt),
		}
	}
	b.mu.Unlock()
	// Best-effort socket cleanup; ssh normally removes it itself.
	_ = os.Remove(controlPath)
	close(done)
}

// Running reports whether the managed ssh child is currently alive.
func (b *Backend) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// LastExit returns how the child ended, or nil while it is running or before
// the first start.
func (b *Backend) LastExit() *ExitInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exit
}

// ControlPath returns the control socket path of the current or most recent
// child, or "" if none was ever started.
func (b *Backend) ControlPath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.controlPath
}

// Stop terminates the managed child: SIGTERM first (ssh then removes its own
// control socket and the remote forward closes with the connection), and
// SIGKILL after the grace period if it is still alive. Only the exact child
// process this backend spawned is ever signalled.
func (b *Backend) Stop(grace time.Duration) {
	b.mu.Lock()
	cmd := b.cmd
	done := b.done
	running := b.running
	controlPath := b.controlPath
	if cmd != nil {
		// Detach this generation: reap still finishes and closes done, but
		// can no longer touch the backend fields a later Start would reset.
		b.cmd = nil
		b.running = false
		b.done = nil
	}
	b.mu.Unlock()

	if cmd != nil && running && cmd.Process != nil {
		_ = terminateProcess(cmd.Process)
		select {
		case <-done:
		case <-time.After(grace):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	if controlPath != "" {
		_ = os.Remove(controlPath)
	}
}

// ProbeRemote runs the canonical remote health probe (the tunnel package's
// RemoteHealthProbeCommand) over the managed master's own control socket and
// classifies the output with ClassifyRemoteProbeOutput. A transport failure
// (dead or missing master) returns a non-nil error alongside the fallback
// classification of whatever output was produced — always fail-closed, never
// a guessed ok.
func (b *Backend) ProbeRemote(ctx context.Context, timeout time.Duration) (tunnel.RemoteTunnelState, error) {
	b.mu.Lock()
	controlPath := b.controlPath
	b.mu.Unlock()
	if controlPath == "" {
		return tunnel.RemoteTunnelUnknown, fmt.Errorf("managed ssh master not started")
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, b.sshBinary(), b.probeArgs(controlPath, tunnel.RemoteHealthProbeCommand(b.Spec.Port))...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	state := tunnel.ClassifyRemoteProbeOutput(out.String())
	if err != nil {
		return state, fmt.Errorf("remote health probe over control socket: %w", err)
	}
	return state, nil
}

// MasterArgs builds the argv for the managed ssh master. It is exported so
// the exact command line the supervisor will run stays testable.
//
// The option set is deliberately explicit rather than relying on the user's
// ~/.ssh/config, and it is the safety boundary of the whole backend:
//
//   - BatchMode=yes: never wait on an interactive password or keychain
//     prompt a background supervisor cannot see or answer; auth problems
//     surface as an immediate exit instead.
//   - ClearAllForwardings=yes: drop every forwarding inherited from the
//     user's config, then add exactly one managed reverse forward.
//   - ExitOnForwardFailure=yes: if the remote bind fails (port already
//     held), the child exits instead of running forward-less while looking
//     alive.
//   - ControlMaster=yes + private ControlPath: the child is a master on a
//     socket this backend generated; user config cannot redirect it because
//     command-line -o wins, and no existing user master is ever reused.
//   - ServerAliveInterval/CountMax: a dead peer is noticed in ~45s instead
//     of never.
func (b *Backend) MasterArgs(controlPath string) []string {
	args := []string{
		"-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ControlMaster=yes",
		"-o", "ControlPath=" + controlPath,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", b.Spec.Port, b.Spec.Port),
	}
	return append(args, b.Spec.Host)
}

// probeArgs builds the argv for a one-shot probe client multiplexed over the
// managed master's own socket. BatchMode and ClearAllForwardings keep the
// probe session from prompting or forwarding anything; ControlPath pins it
// to this backend's master so it can never attach to a user's socket.
func (b *Backend) probeArgs(controlPath, remoteCmd string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ControlPath=" + controlPath,
		b.Spec.Host,
		remoteCmd,
	}
}

func (b *Backend) sshBinary() string {
	if b.SSHBinary != "" {
		return b.SSHBinary
	}
	return "ssh"
}

// controlDir resolves the directory that holds the control socket.
func (b *Backend) controlDir() string {
	if b.ControlDir != "" {
		return b.ControlDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, defaultControlDirName)
}

// newControlSocketPath reserves a unique socket filename inside dir. The
// name is random per start, so two supervisors can never share or hijack one
// another's control socket, and ssh creates the socket itself on master
// startup.
func newControlSocketPath(dir string) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("generate control socket name: %w", err)
	}
	path := filepath.Join(dir, "ctl-"+hex.EncodeToString(rnd[:])+".sock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, stateFileMode)
	if err != nil {
		return "", fmt.Errorf("reserve control socket path: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("reserve control socket path: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("reserve control socket path: %w", err)
	}
	return path, nil
}

// tailBuffer is a mutex-guarded, size-bounded writer that keeps only the
// last max bytes. The ssh child writes into it for its whole lifetime; the
// tail is enough to classify an exit without holding unbounded output.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

// Tail returns the retained bytes; the caller must treat them as
// diagnostics only.
func (t *tailBuffer) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
