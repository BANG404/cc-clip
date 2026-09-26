package plugin

import (
	"fmt"
	"io"
	"strings"
)

// Adapter names dispatched by Run. The Antigravity adapter's user-facing id is
// "agy-notify" (canonical CLI flag --agy, alias --antigravity); its Go identifier
// stays descriptive, mirroring DeployTargets.Antigravity.
const (
	AdapterClaudeNotify      = "claude-notify"
	AdapterCodexNotify       = "codex-notify"
	AdapterAntigravityNotify = "agy-notify"
	AdapterOpencodeNotify    = "opencode-notify" // MUST equal shim.AdapterOpencodeNotify
	AdapterCursorNotify      = "cursor-notify"   // MUST equal shim.AdapterCursorNotify
)

// Run dispatches to the named adapter handler with no adapter arguments.
// stdin/stdout are injected for testability. port comes from the caller (the
// cmd layer resolves it from getPort()).
func Run(name string, port int, stdin io.Reader, stdout io.Writer) error {
	return RunWithArgs(name, port, nil, stdin, stdout)
}

// RunWithArgs is Run plus the arguments the producer appended after the
// adapter name. Only Codex uses them: its notify contract is to run the
// configured program with the event JSON as a trailing argv element, not on
// stdin, so an argv-only invocation posted nothing at all.
func RunWithArgs(name string, port int, args []string, stdin io.Reader, stdout io.Writer) error {
	switch name {
	case AdapterClaudeNotify:
		return runClaudeNotify(port, stdin)
	case AdapterCodexNotify:
		return runCodexNotify(port, args, stdin)
	case AdapterAntigravityNotify:
		return runAntigravityNotify(port, stdin, stdout)
	case AdapterOpencodeNotify:
		return runOpencodeNotify(port, stdin)
	case AdapterCursorNotify:
		return runCursorNotify(port, stdin)
	default:
		return fmt.Errorf("unknown plugin adapter: %q", name)
	}
}

// runClaudeNotify reads raw Claude hook JSON from stdin, injects the host alias
// (reproducing the cc-clip-hook bash flow, hook_template.go:24-31), and forwards
// it to /notify using the hook content type so the daemon classifies it. It is
// fail-soft: neither a stdin read error nor a POST failure propagates, matching
// the cc-clip-hook always-exit-0 contract for hook contexts.
func runClaudeNotify(port int, stdin io.Reader) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return nil // fail-soft: never block the hook (matches cc-clip-hook exit 0)
	}
	_ = postHookPayload(port, injectHost(raw)) // POST failure must not propagate
	return nil
}

// runCodexNotify parses the Codex notify JSON into a generic message and posts
// it. It is fail-soft: a read error, parse error, or POST failure must NOT
// propagate, since codex hook contexts require exit 0 (mirrors antigravity's
// non-blocking posture but without the decision-JSON stdout that codex hooks
// do not expect).
//
// The payload arrives as a trailing argv element: Codex runs the configured
// `notify` program with the event JSON appended as one more argument. stdin
// stays supported because a hand-written argv-to-stdin wrapper is a documented
// way to drive this adapter, and because every other adapter here reads stdin.
func runCodexNotify(port int, args []string, stdin io.Reader) error {
	payload, ok := codexPayload(args, stdin)
	if !ok {
		return nil
	}
	parsed, perr := parseCodexNotifyPayload(payload)
	if perr != nil {
		return nil // fail-soft: invalid payload must not block the agent
	}
	_ = PostNotification(port, "codex", parsed)
	return nil
}

// codexPayload picks the notify JSON out of the trailing arguments, falling
// back to stdin when none of them carries one. Codex appends exactly one JSON
// argument, but the scan is positional-agnostic so an extra fixed argument in
// the user's own `notify` array does not hide the payload.
func codexPayload(args []string, stdin io.Reader) (string, bool) {
	for i := len(args) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(args[i]), "{") {
			return args[i], true
		}
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// runAntigravityNotify parses stdin as an Antigravity Stop payload
// (terminationReason/fullyIdle/error) and posts the notification, but ALWAYS
// writes {"decision":""} to stdout on every exit path (success or POST failure)
// and returns nil regardless of POST outcome, so the dispatcher never blocks
// 'agy' from stopping. {"decision":""} is the "other than continue" value that
// allows the Stop to proceed.
func runAntigravityNotify(port int, stdin io.Reader, stdout io.Writer) error {
	defer func() { _, _ = io.WriteString(stdout, "{\"decision\":\"\"}\n") }()
	b, err := io.ReadAll(stdin)
	if err != nil {
		return nil // stdout already guaranteed by defer
	}
	parsed, perr := parseAntigravityNotifyPayload(string(b))
	if perr == nil {
		_ = PostNotification(port, "agy", parsed)
	}
	return nil
}

// runOpencodeNotify reads the opencode event JSON from stdin, parses it into a
// generic message, and posts it. It mirrors runCodexNotify: a read error, parse
// error, or POST failure must NOT propagate, since the opencode plugin's `event`
// hook is fire-and-forget and a notify failure must never disrupt opencode.
func runOpencodeNotify(port int, stdin io.Reader) error {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return nil // fail-soft
	}
	parsed, perr := parseOpencodeNotifyPayload(string(b))
	if perr != nil {
		return nil // fail-soft: invalid payload must not block opencode
	}
	_ = PostNotification(port, "opencode", parsed)
	return nil
}

// runCursorNotify reads the Cursor `stop` hook payload from stdin, parses it
// into a generic message, and posts it. Fail-soft like every other adapter.
//
// It deliberately writes NOTHING to stdout. Cursor reads a stop hook's stdout
// as an optional {"followup_message": ...}, which restarts the agent — a
// notification adapter emitting that would turn every finished turn into a new
// one, bounded only by Cursor's loop_limit. Silence is the correct response.
func runCursorNotify(port int, stdin io.Reader) error {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return nil // fail-soft
	}
	parsed, perr := parseCursorNotifyPayload(string(b))
	if perr != nil {
		return nil // fail-soft: invalid payload must not block Cursor
	}
	_ = PostNotification(port, "cursor", parsed)
	return nil
}
