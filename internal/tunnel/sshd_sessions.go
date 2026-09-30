package tunnel

import (
	"fmt"
	"strconv"
	"strings"
)

// A port that is held but silent is owned by one of the user's own sshd
// sessions whose client is gone (#173). Nothing short of root can map the
// listener to a PID: ss -p and lsof need privileges for another process's
// sockets, and sshd marks itself non-dumpable, so even the user's own
// /proc/<pid>/fd is unreadable. The start times of the sshd sessions are what
// an unprivileged user can compare against the ssh clients on the local
// machine, so cc-clip lists them instead of pointing at lsof or sudo.

const (
	sshdSessionMarker = "cc-clip-sshd:"
	sshdSelfMarker    = "cc-clip-sshd-self:"
	// startUnknown stands in for a start time the remote ps cannot report
	// (BusyBox has neither -u nor lstart).
	startUnknown = "?"
)

// SSHDSession is one of the calling user's sshd session processes.
type SSHDSession struct {
	PID     int
	Started string // as printed by ps lstart, or "?" when unavailable
	Title   string // process title, e.g. "sshd: alice@notty"
	Current bool   // the session the listing itself ran under
}

// RemoteSSHDSessionsCommand builds a POSIX sh command that lists the calling
// user's sshd session processes with their start times and marks the session
// it runs under. It prefers procps ps and falls back to /proc on BusyBox.
func RemoteSSHDSessionsCommand() string {
	return `uid=$(id -u)
p=$$
while [ -n "$p" ] && [ "$p" -gt 1 ] 2>/dev/null; do
  c=$(cat "/proc/$p/comm" 2>/dev/null)
  case "$c" in sshd*) echo "` + sshdSelfMarker + `$p"; break ;; esac
  p=$(awk '/^PPid:/ {print $2}' "/proc/$p/status" 2>/dev/null)
done
if ps -o pid=,lstart=,args= -u "$uid" >/dev/null 2>&1; then
  ps -o pid=,lstart=,args= -u "$uid" | while read -r pid d1 d2 d3 d4 d5 title; do
    case "$title" in sshd*) printf '` + sshdSessionMarker + `%s\t%s %s %s %s %s\t%s\n' "$pid" "$d1" "$d2" "$d3" "$d4" "$d5" "$title" ;; esac
  done
else
  for d in /proc/[0-9]*; do
    [ "$(stat -c %u "$d" 2>/dev/null)" = "$uid" ] || continue
    case "$(cat "$d/comm" 2>/dev/null)" in sshd*) ;; *) continue ;; esac
    printf '` + sshdSessionMarker + `%s\t` + startUnknown + `\t%s\n' "${d#/proc/}" "$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)"
  done
fi`
}

// ParseSSHDSessions reads the output of RemoteSSHDSessionsCommand. Lines
// without a marker (rc noise, motd) are ignored.
func ParseSSHDSessions(out string) []SSHDSession {
	self := 0
	var sessions []SSHDSession
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, sshdSelfMarker); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				self = pid
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, sshdSessionMarker)
		if !ok {
			continue
		}
		fields := strings.SplitN(rest, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		sessions = append(sessions, SSHDSession{
			PID:     pid,
			Started: strings.TrimSpace(fields[1]),
			Title:   strings.TrimSpace(fields[2]),
		})
	}
	for i := range sessions {
		sessions[i].Current = sessions[i].PID == self
	}
	return sessions
}

// StaleForwardGuidance renders the steps for a port that is held but silent:
// the user's remote sshd sessions (one of them holds the forward) and how to
// end the one whose client is gone without needing root. Every line starts
// with indent.
func StaleForwardGuidance(host string, port int, sessions []SSHDSession, indent string) []string {
	lines := []string{indent + fmt.Sprintf("One of your SSH sessions on %s holds port %d but no longer reaches this machine.", host, port)}
	if len(sessions) == 0 {
		lines = append(lines, indent+"Your sshd sessions on the remote could not be listed.")
	} else {
		lines = append(lines, indent+"Your sshd sessions on the remote (one of them holds the port):")
		for _, s := range sessions {
			note := ""
			if s.Current {
				note = "  (this check; not the holder)"
			}
			started := s.Started
			if started == startUnknown {
				started = "start time unknown"
			}
			lines = append(lines, indent+fmt.Sprintf("  PID %-8d %-26s %s%s", s.PID, started, s.Title, note))
		}
	}
	lines = append(lines,
		indent+"Compare with the ssh clients still running on this machine:",
		indent+"  ps -o pid,lstart,command -ax | grep '[s]sh'",
		indent+"End the remote session that has no live client, then reconnect:",
		indent+fmt.Sprintf("  ssh -o ClearAllForwardings=yes %s 'kill <PID>'", host),
		indent+"(ClearAllForwardings keeps that ssh from taking the port itself.)",
	)
	return lines
}
