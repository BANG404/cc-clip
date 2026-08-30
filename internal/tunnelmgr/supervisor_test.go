//go:build !windows

package tunnelmgr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// newTestSupervisor wires a supervisor to a fake ssh binary and a temp state
// store, with fast intervals so the whole lifecycle fits in milliseconds.
func newTestSupervisor(t *testing.T, port int) (*Supervisor, *Store) {
	t.Helper()
	fake := writeFakeSSH(t)
	storePath := filepath.Join(t.TempDir(), "tunnel-state.json")
	store := NewStoreAt(storePath)
	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: port},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	sup := NewSupervisor(Spec{Host: "fake-host", Port: port}, store, b)
	sup.ProbeInterval = 30 * time.Millisecond
	sup.ProbeTimeout = 5 * time.Second
	sup.StartBackoffBase = 5 * time.Millisecond
	sup.StartBackoffMax = 20 * time.Millisecond
	sup.StableWindow = 200 * time.Millisecond
	sup.StopGrace = 2 * time.Second
	return sup, store
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func runSupervisor(t *testing.T, sup *Supervisor) (cancel context.CancelFunc, done chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	return cancel, done
}

func TestSupervisorHealthyPathAndCleanStop(t *testing.T) {
	t.Setenv("FAKE_SSH_MODE", "master")
	t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:ok\n")

	sup, store := newTestSupervisor(t, 18399)
	cancel, done := runSupervisor(t, sup)

	waitUntil(t, 5*time.Second, func() bool { return sup.State() == StateHealthy }, "healthy state")
	waitUntil(t, 5*time.Second, func() bool {
		rec, err := store.Load()
		return err == nil && rec.Runtime.State == StateHealthy && rec.Runtime.RemoteState == tunnel.RemoteTunnelOK
	}, "persisted healthy state")
	rec, err := store.Load()
	if err != nil {
		t.Fatalf("load state during run: %v", err)
	}
	if rec.SchemaVersion != schemaVersion || rec.Spec.Host != "fake-host" || rec.Spec.Port != 18399 {
		t.Fatalf("persisted record = %+v", rec)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if sup.State() != StateStopped {
		t.Fatalf("state after cancel = %q, want stopped", sup.State())
	}
	if sup.Backend.Running() {
		t.Fatal("child must be terminated after shutdown")
	}
	final, err := store.Load()
	if err != nil {
		t.Fatalf("load final state: %v", err)
	}
	if final.Runtime.State != StateStopped {
		t.Fatalf("final persisted state = %q, want stopped", final.Runtime.State)
	}
}

// TestSupervisorRemoteNotOKNeverHealthy covers the core fail-closed rule:
// only a remote probe of `ok` may produce StateHealthy. Every other probe
// outcome — including unparseable output — must surface as degraded with the
// raw classification preserved.
func TestSupervisorRemoteNotOKNeverHealthy(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want tunnel.RemoteTunnelState
	}{
		{"stale", "cc-clip-probe:stale\n", tunnel.RemoteTunnelStale},
		{"down", "cc-clip-probe:down\n", tunnel.RemoteTunnelDown},
		{"unverified", "cc-clip-probe:unverified\n", tunnel.RemoteTunnelUnverified},
		{"unrecognized output", "some motd noise, no marker\n", tunnel.RemoteTunnelUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_SSH_MODE", "master")
			t.Setenv("FAKE_SSH_PROBE_OUT", tc.out)

			// Nothing listens on the port locally, so the stale annotation
			// path runs against a dead daemon.
			sup, store := newTestSupervisor(t, 18399)
			cancel, done := runSupervisor(t, sup)

			waitUntil(t, 5*time.Second, func() bool { return sup.State() == StateDegraded }, "degraded state")
			if sup.State().Healthy() {
				t.Fatal("degraded must never be healthy")
			}
			if got := sup.RemoteState(); got != tc.want {
				t.Fatalf("remote state = %q, want %q", got, tc.want)
			}
			waitUntil(t, 5*time.Second, func() bool {
				rec, err := store.Load()
				return err == nil && rec.Runtime.State == StateDegraded && rec.Runtime.RemoteState == tc.want
			}, "persisted degraded state")
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after cancel")
			}
		})
	}
}

// TestSupervisorStaleWithLocalDaemonDown checks the one diagnostic that a
// second probe adds real information to: a stale port plus a locally silent
// daemon is reported as the daemon being down, via the v0.9.2
// tunnel.ErrDaemonNotAnswering distinction.
func TestSupervisorStaleWithLocalDaemonDown(t *testing.T) {
	t.Run("daemon answering but not cc-clip", func(t *testing.T) {
		t.Setenv("FAKE_SSH_MODE", "master")
		t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:stale\n")

		// A non-cc-clip HTTP service occupies the local port, so ProbeHealth
		// fails with ErrDaemonNotAnswering (TCP up, identity wrong).
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"service":"something-else","status":"ok"}`)
		}))
		defer srv.Close()
		addr := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
		port := 0
		_, _ = fmt.Sscanf(addr, "%d", &port)

		sup, _ := newTestSupervisor(t, port)
		cancel, done := runSupervisor(t, sup)
		defer func() {
			cancel()
			<-done
		}()

		waitUntil(t, 5*time.Second, func() bool {
			return sup.State() == StateDegraded && strings.Contains(sup.LastError(), "NOT answering")
		}, "stale + ErrDaemonNotAnswering annotation")
	})

	t.Run("nothing listening locally", func(t *testing.T) {
		t.Setenv("FAKE_SSH_MODE", "master")
		t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:stale\n")

		sup, _ := newTestSupervisor(t, 18399)
		cancel, done := runSupervisor(t, sup)
		defer func() {
			cancel()
			<-done
		}()

		waitUntil(t, 5*time.Second, func() bool {
			return sup.State() == StateDegraded && strings.Contains(sup.LastError(), "unreachable")
		}, "stale + unreachable annotation")
	})
}

// TestSupervisorCrashLoopStopsRestarting verifies the circuit breaker: an
// ssh child that can never authenticate leads through auth-required to
// crash-loop after exactly CrashLoopThreshold starts, and the supervisor
// then stops spawning processes entirely.
func TestSupervisorCrashLoopStopsRestarting(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "authfail")

	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("FAKE_SSH_COUNT_FILE", countFile)

	storePath := filepath.Join(t.TempDir(), "tunnel-state.json")
	store := NewStoreAt(storePath)
	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	sup := NewSupervisor(Spec{Host: "fake-host", Port: 18399}, store, b)
	sup.ProbeInterval = 20 * time.Millisecond
	sup.ProbeTimeout = 5 * time.Second
	sup.StartBackoffBase = 5 * time.Millisecond
	sup.StartBackoffMax = 15 * time.Millisecond
	sup.CrashLoopThreshold = 3

	cancel, done := runSupervisor(t, sup)
	waitUntil(t, 5*time.Second, func() bool { return sup.State() == StateCrashLoop }, "crash-loop")

	masterStarts := func() int {
		data, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "x")
	}
	if got := masterStarts(); got != 3 {
		t.Fatalf("master starts = %d, want exactly CrashLoopThreshold(3)", got)
	}
	if !strings.Contains(sup.LastError(), "Permission denied") {
		t.Fatalf("last error = %q, want the ssh auth diagnostic", sup.LastError())
	}
	if sup.State().Healthy() {
		t.Fatal("crash-loop must never be healthy")
	}

	time.Sleep(50 * time.Millisecond)
	if got := masterStarts(); got != 3 {
		t.Fatalf("supervisor kept restarting in crash-loop: %d starts", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	final, err := store.Load()
	if err != nil {
		t.Fatalf("load final state: %v", err)
	}
	if final.Runtime.ConsecutiveStartFailures != 3 {
		t.Fatalf("persisted failures = %d, want 3", final.Runtime.ConsecutiveStartFailures)
	}
}

// TestSupervisorStableExitResetsBackoff verifies the stability gate: a child
// that ran past StableWindow gets a fresh backoff sequence, so a connection
// that works and occasionally drops never accumulates into a crash-loop.
func TestSupervisorStableExitResetsBackoff(t *testing.T) {
	fake := writeFakeSSH(t)
	t.Setenv("FAKE_SSH_MODE", "die")
	t.Setenv("FAKE_SSH_UPTIME", "0.25")
	t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:ok\n")

	countFile := filepath.Join(t.TempDir(), "count")
	t.Setenv("FAKE_SSH_COUNT_FILE", countFile)

	storePath := filepath.Join(t.TempDir(), "tunnel-state.json")
	store := NewStoreAt(storePath)
	b := &Backend{
		Spec:       Spec{Host: "fake-host", Port: 18399},
		SSHBinary:  fake,
		ControlDir: t.TempDir(),
	}
	sup := NewSupervisor(Spec{Host: "fake-host", Port: 18399}, store, b)
	sup.ProbeInterval = 20 * time.Millisecond
	sup.ProbeTimeout = 5 * time.Second
	sup.StartBackoffBase = 5 * time.Millisecond
	sup.StartBackoffMax = 15 * time.Millisecond
	sup.StableWindow = 200 * time.Millisecond
	// The fake child lives ~250ms, past the 200ms stable window, then exits
	// 255. With the reset working, the supervisor must survive more master
	// starts than the crash-loop threshold without ever opening the breaker.
	sup.CrashLoopThreshold = 3

	masterStarts := func() int {
		data, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "x")
	}

	cancel, done := runSupervisor(t, sup)
	defer func() {
		cancel()
		<-done
	}()

	waitUntil(t, 8*time.Second, func() bool { return masterStarts() >= 5 }, "5 master starts")
	if sup.State() == StateCrashLoop {
		t.Fatal("hit crash-loop despite the stable window resetting backoff")
	}
	// And the tunnel comes back healthy after each drop.
	waitUntil(t, 5*time.Second, func() bool { return sup.State() == StateHealthy }, "healthy after drops")
}

// TestSupervisorFailsClosedOnCorruptStore verifies the store contract at the
// supervisor boundary: an unparseable or foreign-schema state file is a hard
// error before anything is spawned.
func TestSupervisorFailsClosedOnCorruptStore(t *testing.T) {
	for _, content := range []string{"{not json", `{"schema_version":2}`} {
		t.Run(content, func(t *testing.T) {
			fake := writeFakeSSH(t)
			t.Setenv("FAKE_SSH_MODE", "master")
			t.Setenv("FAKE_SSH_PROBE_OUT", "cc-clip-probe:ok\n")

			storePath := filepath.Join(t.TempDir(), "tunnel-state.json")
			if err := os.WriteFile(storePath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			store := NewStoreAt(storePath)
			b := &Backend{
				Spec:       Spec{Host: "fake-host", Port: 18399},
				SSHBinary:  fake,
				ControlDir: t.TempDir(),
			}
			sup := NewSupervisor(Spec{Host: "fake-host", Port: 18399}, store, b)

			if err := sup.Run(context.Background()); err == nil {
				t.Fatal("Run must refuse to start on an unvalidatable state file")
			}
			if b.ControlPath() != "" {
				t.Fatal("no ssh child may be started when the state file fails validation")
			}
		})
	}
}
