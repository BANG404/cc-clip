package xvfb

import (
	"fmt"
	"strings"
)

// AuthFileName is the per-deployment X authority file kept next to the rest of
// the Codex state.
const AuthFileName = "Xauthority"

// AuthFilePath returns the X authority file for a Codex state directory.
func AuthFilePath(stateDir string) string {
	return stateDir + "/" + AuthFileName
}

// AuthMarkerPath returns the file recording that the Xvfb instance described by
// xvfb.pid was started under X authorization. Written at start, removed by
// CleanStale, and checked before an instance is reused.
func AuthMarkerPath(stateDir string) string {
	return stateDir + "/xvfb.auth"
}

// EnsureRemoteCookie creates a private MIT-MAGIC-COOKIE-1 authority file on the
// remote if one is not already there, and returns its path.
//
// Why this exists: Xvfb is started with -listen tcp so Codex CLI can reach it
// from inside its sandbox, and it used to be started with no -auth at all. An X
// server with no authorization records falls back to host-based access control,
// which permits the server's own machine — so ANY other account on that remote
// could connect to the display and ask the bridge for the clipboard image,
// holding none of cc-clip's HTTP token. The 0600 token file was never the only
// door.
//
// The cookie is generated ON the remote from /dev/urandom and assembled with
// printf, so it never appears in an argument vector: `ssh host '<cmd>'` puts
// <cmd> in the remote process list, where the other accounts this is defending
// against can read it.
//
// The record uses FamilyWild with an empty address and empty display number.
// The X server loads every MIT-MAGIC-COOKIE-1 record in the file regardless of
// its family, and all three clients involved — jezek/xgb (the bridge), x11rb
// (arboard, and therefore Codex) and libXau — treat a wild family with an empty
// number as matching any display, so one record covers both the Unix socket and
// the TCP loopback path without knowing the display number in advance.
func EnsureRemoteCookie(session RemoteExecutor, stateDir string) (string, error) {
	auth := AuthFilePath(stateDir)
	script := fmt.Sprintf(`umask 077
mkdir -p %[1]s || exit 1
chmod 700 %[1]s 2>/dev/null
if [ -s %[2]s ]; then echo reused; exit 0; fi
tmp=$(mktemp %[1]s/.Xauthority.XXXXXX) || exit 1
# family(FFFF) addrlen(0) numlen(0) namelen(18) "MIT-MAGIC-COOKIE-1" datalen(16)
printf '\377\377\000\000\000\000\000\022MIT-MAGIC-COOKIE-1\000\020' > "$tmp" || exit 1
od -An -v -N16 -to1 /dev/urandom | tr ' ' '\n' | grep . | while read -r b; do printf "\\$b"; done >> "$tmp"
size=$(wc -c < "$tmp" | tr -d ' ')
if [ "$size" != "44" ]; then rm -f "$tmp"; echo "bad-size:$size"; exit 1; fi
chmod 600 "$tmp" || exit 1
mv "$tmp" %[2]s || exit 1
echo created`, stateDir, auth)

	out, err := session.Exec(script)
	if err != nil {
		return "", fmt.Errorf("failed to create X authority cookie on the remote: %s: %w", strings.TrimSpace(out), err)
	}
	switch got := lastNonEmptyLine(out); got {
	case "created", "reused":
		return auth, nil
	default:
		return "", fmt.Errorf("failed to create X authority cookie on the remote: %q", got)
	}
}

// lastNonEmptyLine returns the final meaningful line of remote output, skipping
// any rc-file chatter the login shell prints ahead of it.
func lastNonEmptyLine(out string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}
