package shim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// Only wl-paste is available as a clipboard command. The other PATH entries
// are an explicit allowlist of real utilities required by the shim/pipeline.
func TestClaudeCheckImagePipelineWithOnlyWlPasteShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX executable and symlink semantics")
	}
	dir := t.TempDir()
	for _, name := range []string{"bash", "grep", "cat", "awk", "curl", "head", "cut"} {
		binary, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("required utility %s: %v", name, err)
		}
		if err := os.Symlink(binary, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	port, token := startMockDaemon(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "wl-paste"), []byte(WlPasteShim(port, "/missing/wl-paste")), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const pipeline = `xclip -selection clipboard -t TARGETS -o 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)" || wl-paste -l 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)"`
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "bash"), "--noprofile", "--norc", "-c", pipeline)
	cmd.Env = []string{"PATH=" + dir, "HOME=" + dir, "CC_CLIP_PORT=" + strconv.Itoa(port), "CC_CLIP_TOKEN_FILE=" + token}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "image/png\n" {
		t.Fatalf("checkImage pipeline: err=%v stdout=%q; want exit 0 and image/png", err, out)
	}
}
