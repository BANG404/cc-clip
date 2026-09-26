package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shunmei/cc-clip/internal/shim"
)

// TestClaudeHooksProbeFlagsWrongPort pins the diagnosis. A managed hook wired
// for another port is still ours and still present, but its notifications go
// nowhere; reporting it as "wired" made doctor agree with the bug.
func TestClaudeHooksProbeFlagsWrongPort(t *testing.T) {
	got := classifyClaudeHooksCheck("claude-hooks:managed-wrong-port\n", nil)
	if got.OK {
		t.Fatal("a hook wired for the wrong port must not pass")
	}
	if !strings.Contains(got.Message, "DIFFERENT port") {
		t.Errorf("the diagnosis must name the cause, got: %s", got.Message)
	}

	if ok := classifyClaudeHooksCheck("claude-hooks:managed\n", nil); !ok.OK {
		t.Error("a correctly wired managed hook must still pass")
	}

	// The probe must look for this deployment's exact command, not just the
	// ownership prefix.
	if !strings.Contains(claudeHooksProbeCommand(29999), "CC_CLIP_PORT=29999") {
		t.Error("probe must check the command for the deployed port")
	}
}

// TestClaudeHooksProbeSeparatesLegacyFromWrongPort runs the probe against real
// settings files. The legacy managed command shares the ownership prefix but
// has no port, and was reported as a wrong-port hook on a correct host.
func TestClaudeHooksProbeSeparatesLegacyFromWrongPort(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cases := []struct {
		name, command, want string
	}{
		{"legacy managed", shim.ClaudeLegacyManagedHookCommand, "claude-hooks:managed-legacy"},
		{"current, other port", shim.ClaudeManagedHookCommand(29999), "claude-hooks:managed-wrong-port"},
		{"current, this port", shim.ClaudeManagedHookCommand(18339), "claude-hooks:managed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			settings := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + tc.command + `"}]}]}}`
			if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, "-c", claudeHooksProbeCommand(18339))
			cmd.Env = append(os.Environ(), "HOME="+home)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Fatalf("probe printed %q, want %q", got, tc.want)
			}
			if tc.want == "claude-hooks:managed-legacy" && !classifyClaudeHooksCheck(string(out), nil).OK {
				t.Error("a legacy managed hook fires via the fallback script and must not fail the check")
			}
		})
	}
}
