//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStopHotkeyProcessWritesStopSentinelAndKills(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "hotkey.pid")
	stopFile := filepath.Join(tmpDir, "hotkey.stop")

	hotkeyPIDPathOverride = pidFile
	hotkeyStopFilePathOverride = stopFile
	originalCmdFunc := localProcessCommandFunc
	t.Cleanup(func() {
		hotkeyPIDPathOverride = ""
		hotkeyStopFilePathOverride = ""
		localProcessCommandFunc = originalCmdFunc
	})

	// Mock localProcessCommand so it always reports "hotkey" in the
	// command line — prevents stopHotkeyProcess from refusing to kill.
	localProcessCommandFunc = func(pid int) (string, error) {
		return "cc-clip.exe hotkey --run-loop", nil
	}

	// Start a real child process that stopHotkeyProcess can kill.
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}

	stopHotkeyProcess()

	// Stop sentinel must exist — this is what prevents the VBS loop from respawning.
	if _, err := os.Stat(stopFile); os.IsNotExist(err) {
		t.Fatal("expected stop sentinel file to be created, but it does not exist")
	}
	// PID file must be cleaned up.
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatal("expected PID file to be removed after stop")
	}
}

func TestStopHotkeyProcessWritesSentinelEvenWhenNotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "hotkey.pid")
	stopFile := filepath.Join(tmpDir, "hotkey.stop")

	hotkeyPIDPathOverride = pidFile
	hotkeyStopFilePathOverride = stopFile
	originalCmdFunc := localProcessCommandFunc
	t.Cleanup(func() {
		hotkeyPIDPathOverride = ""
		hotkeyStopFilePathOverride = ""
		localProcessCommandFunc = originalCmdFunc
	})

	localProcessCommandFunc = func(pid int) (string, error) {
		return "cc-clip.exe hotkey --run-loop", nil
	}

	// No PID file exists — hotkey process may have crashed but the VBS
	// autostart loop could still be running. The sentinel must be written
	// unconditionally so the VBS loop exits on its next iteration.
	stopHotkeyProcess()

	if _, err := os.Stat(stopFile); os.IsNotExist(err) {
		t.Fatal("expected stop sentinel file even when hotkey process is not running")
	}
}

func TestParseHotkeyAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"default", "alt+shift+v", "alt+shift+v"},
		{"case insensitive", "ALT+SHIFT+V", "alt+shift+v"},
		{"ctrl alt other key", "ctrl+alt+p", "ctrl+alt+p"},
		{"win modifier", "win+shift+v", "shift+win+v"},
		{"function key", "ctrl+f12", "ctrl+f12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHotkey(tc.in)
			if err != nil {
				t.Fatalf("parseHotkey(%q) unexpected error: %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Fatalf("parseHotkey(%q).String() = %q, want %q", tc.in, got.String(), tc.want)
			}
		})
	}
}

func TestParseHotkeyRejectsPasteConflicts(t *testing.T) {
	// These combinations are rejected because they would prevent pastes
	// from reaching the terminal:
	//   - ctrl+v is the system paste shortcut; registering it as a global
	//     hotkey would hijack every paste.
	//   - ctrl+shift+v is what windowsSendCtrlShiftV synthesizes; an
	//     identical binding would be re-caught by our own RegisterHotKey
	//     loop (hotkeyRunning guard) and the simulated keystroke would be
	//     silently swallowed.
	cases := []struct {
		name     string
		in       string
		wantFrag string
	}{
		{"ctrl+v", "ctrl+v", "system paste shortcut"},
		{"ctrl+shift+v", "ctrl+shift+v", "simulated paste keystroke"},
		{"shift+ctrl+v normalized", "shift+ctrl+v", "simulated paste keystroke"},
		{"uppercase ctrl+V", "Ctrl+V", "system paste shortcut"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHotkey(tc.in)
			if err == nil {
				t.Fatalf("parseHotkey(%q) returned no error, want rejection", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantFrag) {
				t.Fatalf("parseHotkey(%q) error = %q, want to contain %q", tc.in, err.Error(), tc.wantFrag)
			}
		})
	}
}

func TestParseHotkeyNonVKeyNotRejected(t *testing.T) {
	// Sanity check: the conflict rule only targets the V key. Ctrl+Shift+B
	// looks structurally similar to ctrl+shift+v but must remain valid.
	if _, err := parseHotkey("ctrl+shift+b"); err != nil {
		t.Fatalf("parseHotkey(ctrl+shift+b) unexpected error: %v", err)
	}
}

func TestSaveHotkeyConfigRejectsPasteConflicts(t *testing.T) {
	tmpDir := t.TempDir()
	hotkeyConfigPathOverride = filepath.Join(tmpDir, "hotkey.json")
	t.Cleanup(func() { hotkeyConfigPathOverride = "" })

	cfg := hotkeyConfig{Host: "example", Hotkey: "ctrl+shift+v"}
	if err := saveHotkeyConfig(cfg); err == nil {
		t.Fatal("saveHotkeyConfig accepted ctrl+shift+v, want rejection")
	} else if !strings.Contains(err.Error(), "simulated paste keystroke") {
		t.Fatalf("saveHotkeyConfig error = %q, want to mention conflict reason", err.Error())
	}

	cfg.Hotkey = "ctrl+v"
	if err := saveHotkeyConfig(cfg); err == nil {
		t.Fatal("saveHotkeyConfig accepted ctrl+v, want rejection")
	}
}

// TestSameHotkeySettingsIgnoresTrayOwnedFields pins what counts as a
// configuration change. A running loop cannot reload, so a change the CLI
// manages must be refused rather than written to disk where --status would
// report a host the next screenshot never reaches.
func TestSameHotkeySettingsIgnoresTrayOwnedFields(t *testing.T) {
	base := hotkeyConfig{Host: "venus", RemoteDir: "~/uploads", DelayMS: 150, Hotkey: "alt+shift+v"}

	enabled, disabled := true, false
	withNotify := base
	withNotify.Notifications = &enabled
	other := base
	other.Notifications = &disabled
	if !sameHotkeySettings(withNotify, other) {
		t.Error("the tray's notification toggle must not count as a CLI config change")
	}

	changedHost := base
	changedHost.Host = "mars"
	if sameHotkeySettings(base, changedHost) {
		t.Error("a different host must count as a change")
	}

	changedRestore := base
	changedRestore.NoRestore = true
	if sameHotkeySettings(base, changedRestore) {
		t.Error("a different --no-restore must count as a change")
	}
}

// TestHotkeyChangeBlockedTreatsUnknownAsRunning pins the tri-state.
//
// The first version of this gate fired only on hotkeyProcessRunning, so an
// unverifiable PID — what a non-elevated shell gets back for an elevated loop —
// fell through, the new host was written to disk, and startHotkeyBackground
// then refused to start anything. That reproduced the original defect exactly:
// --status reporting one host while a live loop kept uploading to another.
func TestHotkeyChangeBlockedTreatsUnknownAsRunning(t *testing.T) {
	stored := hotkeyConfig{Host: "venus", RemoteDir: "~/uploads", DelayMS: 150, Hotkey: "alt+shift+v"}
	changed := stored
	changed.Host = "mars"

	cases := []struct {
		name      string
		state     hotkeyProcessState
		hasStored bool
		cfg       hotkeyConfig
		want      bool
	}{
		{"running with a changed host is refused", hotkeyProcessRunning, true, changed, true},
		{"unverifiable pid with a changed host is refused", hotkeyProcessUnknown, true, changed, true},
		{"running with identical settings is allowed", hotkeyProcessRunning, true, stored, false},
		{"unverifiable pid with identical settings is allowed", hotkeyProcessUnknown, true, stored, false},
		{"no live loop is always allowed", hotkeyProcessGone, true, changed, false},
		{"a foreign pid is always allowed", hotkeyProcessOther, true, changed, false},
		{"a live loop with no stored config is refused", hotkeyProcessRunning, false, changed, true},
		{"an unverifiable pid with no stored config is refused", hotkeyProcessUnknown, false, changed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hotkeyChangeBlocked(tc.state, tc.hasStored, tc.cfg, stored); got != tc.want {
				t.Errorf("hotkeyChangeBlocked = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDescribeBlockedHotkeyChangeDoesNotOverclaim keeps the refusal message
// honest about which of the two states it is reporting.
func TestDescribeBlockedHotkeyChangeDoesNotOverclaim(t *testing.T) {
	stored := hotkeyConfig{Host: "venus"}

	unknown := describeBlockedHotkeyChange(4321, hotkeyProcessUnknown, "access is denied.", true, stored)
	if !strings.Contains(unknown, "may still be running") {
		t.Errorf("an unverifiable pid must not be reported as certainly running: %q", unknown)
	}
	if !strings.Contains(unknown, "access is denied.") {
		t.Errorf("the refusal must say why the pid could not be verified: %q", unknown)
	}

	running := describeBlockedHotkeyChange(4321, hotkeyProcessRunning, "", true, stored)
	if !strings.Contains(running, "already running") || !strings.Contains(running, "venus") {
		t.Errorf("a verified loop must be named with its host: %q", running)
	}

	noStored := describeBlockedHotkeyChange(4321, hotkeyProcessRunning, "", false, hotkeyConfig{})
	if !strings.Contains(noStored, "unknown host") {
		t.Errorf("with no stored config the host must not be invented: %q", noStored)
	}
}
