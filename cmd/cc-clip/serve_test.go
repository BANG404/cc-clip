package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServeRotateTokenLeavesTokenUntouchedWhenPortTaken(t *testing.T) {
	// cmdServe uses log.Fatal on bind failure, so run it in a child process.
	if os.Getenv("CC_CLIP_TEST_SERVE_CHILD") == "1" {
		os.Args = []string{"cc-clip", "serve", "--rotate-token"}
		cmdServe()
		return
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CC_CLIP_TOKEN_DIR", home)
	t.Setenv("CC_CLIP_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	t.Setenv("CC_CLIP_TEST_SERVE_CHILD", "1")
	path := filepath.Join(home, "session.token")
	before := []byte("running-daemon-token\n2099-01-01T00:00:00Z\n")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	// An old timestamp also catches an otherwise byte-identical rewrite.
	stamp := time.Unix(946684800, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServeRotateTokenLeavesTokenUntouchedWhenPortTaken$")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("serve child timed out: %s", out)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("serve child error = %v, want exit 1; output: %s", err, out)
	}
	if !strings.Contains(string(out), "failed to listen on "+listener.Addr().String()) {
		t.Fatalf("expected bind failure, got: %s", out)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("serve --rotate-token changed the shared token file despite losing the port race")
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, current) || !current.ModTime().Equal(original.ModTime()) || current.Mode() != original.Mode() {
		t.Fatal("serve --rotate-token replaced or modified the shared token file despite losing the port race")
	}
}
