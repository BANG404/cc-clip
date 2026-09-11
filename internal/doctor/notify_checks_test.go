package doctor

import (
	"strings"
	"testing"
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
