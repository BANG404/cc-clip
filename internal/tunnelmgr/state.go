// Package tunnelmgr implements the managed persistent tunnel core from
// issue #108 Phase 1A: a minimal state store, a managed non-interactive SSH
// backend, and a single-tunnel supervisor that can be driven manually.
//
// Phase 1A scope only: no LaunchAgent, no enable/disable commands, no SSH
// config migration, no rollback. The supervisor runs only when explicitly
// started (foreground `cc-clip tunnel run <host>` or a test harness); the
// legacy interactive `RemoteForward` workflow is untouched and remains the
// default behavior of every existing command.
//
// Health semantics are built on the v0.9.2 probe primitives in
// internal/tunnel (ProbeHealth, RemoteHealthProbeCommand,
// ClassifyRemoteProbeOutput). Nothing in this package reimplements them, and
// everything except a proven-healthy state fails closed: a supervisor state
// is healthy only when the managed ssh child is running AND the remote probe
// classified the forward as ok.
package tunnelmgr

import "fmt"

// State is the supervisor's aggregate lifecycle state, persisted in the
// tunnel state file. It deliberately collapses both health dimensions
// (ssh child process, remote probe outcome) into one exhaustive enum so that
// anything the supervisor cannot prove falls into its own non-healthy state
// instead of being folded into a boolean.
type State string

const (
	// StateUnknown means no validated state exists yet. It is the initial
	// state of a fresh record and the parse fallback for anything
	// unrecognized loaded from disk. Never healthy.
	StateUnknown State = "unknown"

	// StateHealthy means the managed ssh child is running and the remote
	// probe classified the forward as tunnel.RemoteTunnelOK. The only
	// healthy state.
	StateHealthy State = "healthy"

	// StateReconnecting means the managed ssh child is not running (spawn
	// failure or exit) and the supervisor is backing off before the next
	// spawn attempt.
	StateReconnecting State = "reconnecting"

	// StateAuthRequired means the ssh child exited because authentication
	// was refused. BatchMode forbids interactive prompts, so this needs a
	// human (ssh-agent, key, passphrase); the supervisor retries slowly and
	// stops after CrashLoopThreshold consecutive failures.
	StateAuthRequired State = "auth-required"

	// StateHostKeyError means the ssh child exited because the remote host
	// key could not be verified. The supervisor never bypasses host key
	// checking; a human must fix known_hosts.
	StateHostKeyError State = "host-key-error"

	// StateDegraded means the ssh child is running but the remote probe did
	// not classify the forward as ok (down, stale, unverified, or unknown).
	// The raw probe outcome is preserved in Runtime.RemoteState. The child
	// is left alone: restarting it cannot fix a remote-side conflict or a
	// local daemon that is down, and doing so on every failed probe would
	// churn the connection.
	StateDegraded State = "degraded"

	// StateCrashLoop means the supervisor hit CrashLoopThreshold consecutive
	// failed starts without reaching the stable window. Automatic restarts
	// stop until the supervisor is restarted by hand.
	StateCrashLoop State = "crash-loop"

	// StateStopped means the supervisor shut down cleanly (context
	// cancelled, child terminated).
	StateStopped State = "stopped"
)

// Healthy reports whether the state proves the data path works end to end.
// Only StateHealthy qualifies; every other state, including StateUnknown,
// StateDegraded and StateCrashLoop, fails closed.
func (s State) Healthy() bool {
	return s == StateHealthy
}

// ParseState validates a state string loaded from disk. Anything outside the
// known set — including the empty string — is an error, so a corrupt or
// future-schema state file can never be read as healthy.
func ParseState(s string) (State, error) {
	switch State(s) {
	case StateUnknown, StateHealthy, StateReconnecting, StateAuthRequired,
		StateHostKeyError, StateDegraded, StateCrashLoop, StateStopped:
		return State(s), nil
	default:
		return StateUnknown, fmt.Errorf("unknown tunnel supervisor state %q", s)
	}
}
