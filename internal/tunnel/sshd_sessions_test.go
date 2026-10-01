package tunnel

import (
	"strings"
	"testing"
)

// TestParseSSHDSessions pins the #173 listing: marker lines are parsed, rc
// noise is ignored, the session the listing ran under is flagged, and a
// BusyBox row without a start time survives.
func TestParseSSHDSessions(t *testing.T) {
	out := strings.Join([]string{
		"bash: no job control in this shell",
		sshdSelfMarker + "1886713",
		sshdSessionMarker + "1881174\tWed Sep 30 21:24:10 2026\tsshd: alice@notty",
		sshdSessionMarker + "1886713\tWed Sep 30 21:26:18 2026\tsshd-session: alice@notty\r",
		sshdSessionMarker + "412\t?\tsshd: alice@pts/0 ",
		sshdSessionMarker + "not-a-pid\tx\ty",
		"motd line",
	}, "\n")

	got := ParseSSHDSessions(out)
	want := []SSHDSession{
		{PID: 1881174, Started: "Wed Sep 30 21:24:10 2026", Title: "sshd: alice@notty"},
		{PID: 1886713, Started: "Wed Sep 30 21:26:18 2026", Title: "sshd-session: alice@notty", Current: true},
		{PID: 412, Started: "?", Title: "sshd: alice@pts/0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("session %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestStaleForwardGuidanceWithoutSessions(t *testing.T) {
	got := strings.Join(StaleForwardGuidance("venus", 18339, nil, true, ""), "\n")
	for _, want := range []string{"could not be listed", "ssh -o ClearAllForwardings=yes venus 'kill <PID>'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("guidance lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "start time unknown") {
		t.Fatalf("no session rows expected:\n%s", got)
	}
}

// TestStaleForwardGuidanceSelfLabel pins the review of #176: the session the
// listing ran under is called "not the holder" only when the caller vouches
// that its connection could not have reused a master that owns the forward.
func TestStaleForwardGuidanceSelfLabel(t *testing.T) {
	sessions := []SSHDSession{
		{PID: 11, Started: "Wed Sep 30 21:24:10 2026", Title: "sshd: alice@notty"},
		{PID: 22, Started: "Wed Sep 30 21:26:18 2026", Title: "sshd: alice@notty", Current: true},
	}
	independent := strings.Join(StaleForwardGuidance("venus", 18339, sessions, true, ""), "\n")
	if !strings.Contains(independent, "PID 22") || !strings.Contains(independent, "(this check; not the holder)") {
		t.Fatalf("independent connection must rule itself out:\n%s", independent)
	}
	reused := strings.Join(StaleForwardGuidance("venus", 18339, sessions, false, ""), "\n")
	if strings.Contains(reused, "not the holder") || !strings.Contains(reused, "(this check)") {
		t.Fatalf("a possibly reused connection must not rule itself out:\n%s", reused)
	}
}

// TestStaleForwardGuidanceSparesOtherComputers pins the review of #176: a
// session opened from another workstation has no client on this machine, so
// the guidance must not treat "no local client" alone as stale.
func TestStaleForwardGuidanceSparesOtherComputers(t *testing.T) {
	got := strings.Join(StaleForwardGuidance("venus", 18339, nil, true, ""), "\n")
	if !strings.Contains(got, "from another computer") {
		t.Fatalf("guidance must warn about sessions from other computers:\n%s", got)
	}
}

func TestRemoteSSHDSessionsCommandFallsBackWithoutProcps(t *testing.T) {
	cmd := RemoteSSHDSessionsCommand()
	for _, want := range []string{"ps -o pid=,lstart=,args= -u", "/proc/[0-9]*", "/proc/$p/comm", sshdSelfMarker, sshdSessionMarker} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command lacks %q", want)
		}
	}
}
