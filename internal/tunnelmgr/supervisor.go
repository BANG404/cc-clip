package tunnelmgr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/shunmei/cc-clip/internal/tunnel"
)

// Supervisor defaults. They are fields on the struct so tests can shrink
// them; the values only have to behave sensibly for a foreground manual run.
const (
	defaultProbeInterval      = 30 * time.Second
	defaultProbeTimeout       = 15 * time.Second
	defaultStartBackoffBase   = 2 * time.Second
	defaultStartBackoffMax    = 30 * time.Second
	defaultStableWindow       = 30 * time.Second
	defaultStopGrace          = 5 * time.Second
	defaultCrashLoopThreshold = 5
)

// Supervisor manages one managed tunnel: it keeps the backend's ssh child
// alive with bounded backoff, verifies health with the canonical remote
// probe, persists state transitions to the store, and shuts the child down
// when its context is cancelled. It is deliberately single-tunnel and
// synchronous — no scheduling framework, no plugin points.
type Supervisor struct {
	Spec    Spec
	Store   *Store
	Backend *Backend

	// Logf receives one line per state transition and probe outcome change.
	// Nil means silent.
	Logf func(format string, args ...any)

	ProbeInterval      time.Duration
	ProbeTimeout       time.Duration
	StartBackoffBase   time.Duration
	StartBackoffMax    time.Duration
	StableWindow       time.Duration
	StopGrace          time.Duration
	CrashLoopThreshold int

	mu       sync.Mutex
	state    State
	remote   tunnel.RemoteTunnelState
	lastErr  string
	failures int
	lastKey  string
}

// NewSupervisor returns a supervisor with production defaults for the given
// spec, store, and backend.
func NewSupervisor(spec Spec, store *Store, backend *Backend) *Supervisor {
	return &Supervisor{
		Spec:               spec,
		Store:              store,
		Backend:            backend,
		ProbeInterval:      defaultProbeInterval,
		ProbeTimeout:       defaultProbeTimeout,
		StartBackoffBase:   defaultStartBackoffBase,
		StartBackoffMax:    defaultStartBackoffMax,
		StableWindow:       defaultStableWindow,
		StopGrace:          defaultStopGrace,
		CrashLoopThreshold: defaultCrashLoopThreshold,
	}
}

// State returns the current aggregate supervisor state.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// RemoteState returns the raw remote probe outcome of the last completed
// probe, or "" if none has run.
func (s *Supervisor) RemoteState() tunnel.RemoteTunnelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

// LastError returns the reason for the most recent non-healthy state.
func (s *Supervisor) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Run drives the supervisor until ctx is cancelled, then stops the managed
// child and persists StateStopped.
//
// A corrupt or unvalidatable state file is a hard error before anything is
// started: the store contract is fail closed, and the supervisor will not
// run on top of state it cannot read.
func (s *Supervisor) Run(ctx context.Context) error {
	if _, err := s.Store.Load(); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("load tunnel state: %w", err)
		}
		// First run for this host: start from an explicit unknown state.
		fresh := &Record{
			SchemaVersion: schemaVersion,
			Spec:          s.Spec,
			Runtime:       Runtime{State: StateUnknown, UpdatedAt: time.Now().UTC()},
		}
		if err := s.Store.Save(fresh); err != nil {
			return fmt.Errorf("initialize tunnel state: %w", err)
		}
	}

	s.mu.Lock()
	s.state = StateReconnecting
	s.failures = 0
	s.mu.Unlock()
	s.persist()

	for {
		if ctx.Err() != nil {
			break
		}
		if s.State() == StateCrashLoop {
			// Circuit breaker open: no automatic restarts. The supervisor
			// stays alive so the persisted state and logs remain inspectable
			// until the operator stops it.
			<-ctx.Done()
			break
		}
		if !s.Backend.Running() {
			if exit := s.Backend.LastExit(); exit != nil {
				// A child that ran past the stable window gets a fresh
				// backoff sequence; rapid crash-looping only counts when
				// starts never stabilize.
				if exit.Uptime >= s.StableWindow {
					s.mu.Lock()
					s.failures = 0
					s.mu.Unlock()
				}
				s.recordStartFailure(classifyExit(exit), exit)
				if s.State() == StateCrashLoop {
					continue
				}
				if !sleepInterruptible(ctx, s.backoff()) {
					break
				}
			}
			if err := s.Backend.Start(); err != nil {
				s.recordStartFailure(StateReconnecting, &ExitInfo{
					Code:       -1,
					StderrTail: err.Error(),
				})
				if s.State() == StateCrashLoop {
					continue
				}
				if !sleepInterruptible(ctx, s.backoff()) {
					break
				}
			}
			continue
		}

		// Child is up: ask the daemon to identify itself through the forward.
		remote, probeErr := s.Backend.ProbeRemote(ctx, s.ProbeTimeout)
		if ctx.Err() != nil {
			break
		}
		if probeErr != nil && !s.Backend.Running() {
			// The master died while probing; the next iteration classifies
			// the exit and restarts.
			continue
		}
		if probeErr == nil && remote.Healthy() {
			s.transition(StateHealthy, remote, "")
		} else {
			s.transition(StateDegraded, remote, s.degradedReason(remote, probeErr))
		}
		if !sleepInterruptible(ctx, s.ProbeInterval) {
			break
		}
	}

	s.Backend.Stop(s.StopGrace)
	s.mu.Lock()
	s.state = StateStopped
	s.mu.Unlock()
	s.persist()
	return nil
}

// recordStartFailure bumps the consecutive failure counter, opens the
// circuit breaker at the threshold, and persists the resulting state.
func (s *Supervisor) recordStartFailure(cause State, exit *ExitInfo) {
	s.mu.Lock()
	s.failures++
	s.lastErr = fmt.Sprintf("ssh child exited with code %d after %s: %s",
		exit.Code, exit.Uptime.Round(time.Millisecond), exit.StderrTail)
	if s.failures >= s.CrashLoopThreshold {
		s.state = StateCrashLoop
	} else {
		s.state = cause
	}
	s.mu.Unlock()
	s.persist()
}

// degradedReason builds the operator-facing reason for a failed remote
// probe. A stale port is the one case where a second, local check adds real
// information: it separates "our forward is up but the local daemon is down"
// from "something remote still holds the port" — the exact distinction
// tunnel.ErrDaemonNotAnswering exists for.
func (s *Supervisor) degradedReason(remote tunnel.RemoteTunnelState, probeErr error) string {
	if probeErr != nil {
		return fmt.Sprintf("remote probe did not complete: %v", probeErr)
	}
	reason := remote.Summary(s.Spec.Port)
	if remote == tunnel.RemoteTunnelStale {
		addr := fmt.Sprintf("127.0.0.1:%d", s.Spec.Port)
		if err := tunnel.ProbeHealth(addr, 2*time.Second); err != nil {
			if errors.Is(err, tunnel.ErrDaemonNotAnswering) {
				reason += "; local daemon is NOT answering at " + addr
			} else {
				reason += "; local daemon unreachable at " + addr
			}
		} else {
			reason += "; local daemon is answering (port likely held on the remote by a stale session)"
		}
	}
	return reason
}

// transition updates the in-memory state and persists when anything
// observable changed.
func (s *Supervisor) transition(state State, remote tunnel.RemoteTunnelState, lastErr string) {
	s.mu.Lock()
	s.state = state
	s.remote = remote
	s.lastErr = lastErr
	s.mu.Unlock()
	s.persist()
}

// persist writes the current snapshot when it differs from the last written
// one. Frequency is bounded by real change, not by the probe interval.
func (s *Supervisor) persist() {
	s.mu.Lock()
	rec := &Record{
		SchemaVersion: schemaVersion,
		Spec:          s.Spec,
		Runtime: Runtime{
			State:                    s.state,
			RemoteState:              s.remote,
			LastError:                s.lastErr,
			ConsecutiveStartFailures: s.failures,
			UpdatedAt:                time.Now().UTC(),
		},
	}
	key := fmt.Sprintf("%s|%s|%s|%d", s.state, s.remote, s.lastErr, s.failures)
	if key == s.lastKey {
		s.mu.Unlock()
		return
	}
	s.lastKey = key
	s.mu.Unlock()
	if err := s.Store.Save(rec); err != nil && s.Logf != nil {
		s.Logf("persist tunnel state failed: %v", err)
	}
}

// backoff returns the delay before the next spawn attempt: exponential from
// StartBackoffBase, capped at StartBackoffMax.
func (s *Supervisor) backoff() time.Duration {
	d := s.StartBackoffBase
	for i := 1; i < s.failures; i++ {
		d *= 2
		if d >= s.StartBackoffMax {
			return s.StartBackoffMax
		}
	}
	if d > s.StartBackoffMax {
		return s.StartBackoffMax
	}
	return d
}

// classifyExit maps a child exit to the restart state. Only exit code 255
// with ssh's own diagnostic strings is classified further; anything else is
// a plain reconnect. Unknown output never becomes healthy — it becomes a
// restart with the stderr tail preserved as last_error.
func classifyExit(exit *ExitInfo) State {
	if exit == nil {
		return StateReconnecting
	}
	msg := strings.ToLower(exit.StderrTail)
	if exit.Code == 255 {
		if strings.Contains(msg, "permission denied") {
			return StateAuthRequired
		}
		if strings.Contains(msg, "host key verification failed") {
			return StateHostKeyError
		}
	}
	return StateReconnecting
}

// sleepInterruptible waits for d or ctx cancellation. It reports whether the
// full delay elapsed.
func sleepInterruptible(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
