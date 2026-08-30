//go:build !windows

package tunnelmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeSSH writes a /bin/sh script that emulates the OpenSSH surface the
// backend and supervisor exercise: a long-running master (invoked with -N)
// and one-shot probe clients multiplexed over the ControlPath. Behavior is
// selected through environment variables so each test can script failure
// modes (auth refused, host key rejected, master dies after N seconds).
//
// This file is excluded on Windows by its build tag (no /bin/sh there); the
// CI Windows job still compiles the whole package, and the argv/store logic
// these tests cover is platform-independent.
func writeFakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
has_n=0
cp=""
for a in "$@"; do
  [ "$a" = "-N" ] && has_n=1
  case "$a" in ControlPath=*) cp="${a#ControlPath=}" ;; esac
done
if [ "$has_n" = "1" ]; then
  [ -n "$FAKE_SSH_COUNT_FILE" ] && echo x >> "$FAKE_SSH_COUNT_FILE"
  case "$FAKE_SSH_MODE" in
    authfail) echo "ssh: Permission denied (publickey,password)." >&2; exit 255 ;;
    hostkey)  echo "Host key verification failed." >&2; exit 255 ;;
    die)      [ -n "$cp" ] && : > "$cp"; sleep "${FAKE_SSH_UPTIME:-0}"; echo "Connection closed by remote host." >&2; exit 255 ;;
    *)        [ -n "$cp" ] && : > "$cp"; trap 'exit 0' TERM; while :; do sleep 1; done ;;
  esac
fi
printf '%s' "$FAKE_SSH_PROBE_OUT"
exit "${FAKE_SSH_PROBE_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	return path
}
