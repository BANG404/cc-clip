package shim

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallAndUninstallXclip(t *testing.T) {
	dir := t.TempDir()

	result, err := Install(TargetXclip, dir, 18339)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	if result.Target != TargetXclip {
		t.Fatalf("expected target xclip, got %s", result.Target)
	}
	if result.ShimPath != filepath.Join(dir, "xclip") {
		t.Fatalf("unexpected shim path: %s", result.ShimPath)
	}

	// Verify shim file exists and is executable
	info, err := os.Stat(result.ShimPath)
	if err != nil {
		t.Fatalf("shim file not found: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		t.Fatal("shim file is not executable")
	}

	// Verify shim content
	data, err := os.ReadFile(result.ShimPath)
	if err != nil {
		t.Fatalf("failed to read shim: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "cc-clip") {
		t.Fatal("shim does not contain cc-clip marker")
	}
	if !strings.Contains(content, "18339") {
		t.Fatal("shim does not contain expected port")
	}

	// Test duplicate install fails
	_, err = Install(TargetXclip, dir, 18339)
	if err == nil {
		t.Fatal("expected error on duplicate install")
	}

	// Uninstall
	if err := Uninstall(TargetXclip, dir); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}

	// Verify removed
	if _, err := os.Stat(result.ShimPath); !os.IsNotExist(err) {
		t.Fatal("shim file should be removed after uninstall")
	}
}

func TestUninstallNonShim(t *testing.T) {
	dir := t.TempDir()

	// Create a non-shim file
	path := filepath.Join(dir, "xclip")
	os.WriteFile(path, []byte("#!/bin/bash\necho real xclip"), 0755)

	err := Uninstall(TargetXclip, dir)
	if err == nil {
		t.Fatal("expected error when uninstalling non-shim file")
	}
}

func TestInstallWlPaste(t *testing.T) {
	dir := t.TempDir()

	result, err := Install(TargetWlPaste, dir, 18339)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	if result.Target != TargetWlPaste {
		t.Fatalf("expected target wl-paste, got %s", result.Target)
	}

	data, _ := os.ReadFile(result.ShimPath)
	if !strings.Contains(string(data), "wl-paste") {
		t.Fatal("wl-paste shim content incorrect")
	}

	Uninstall(TargetWlPaste, dir)
}

func TestIsOurShim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xclip")

	// Not our shim
	os.WriteFile(path, []byte("#!/bin/bash\necho real"), 0755)
	if isOurShim(path) {
		t.Fatal("should not detect non-shim as our shim")
	}

	// A foreign wrapper that merely MENTIONS cc-clip is not ours. Classifying
	// on a bare "cc-clip" substring is what let the Wayland companion install
	// overwrite a user's own wl-copy wrapper.
	os.WriteFile(path, []byte("#!/bin/sh\n# thin wrapper so cc-clip can find the real one\nexec /usr/bin/xclip \"$@\"\n"), 0755)
	if isOurShim(path) {
		t.Fatal("a foreign wrapper mentioning cc-clip must not be claimed as ours")
	}

	// Every shim this package generates is ours.
	for name, content := range map[string]string{
		"xclip":    XclipShim(18339, "/usr/bin/xclip"),
		"wl-paste": WlPasteShim(18339, "/usr/bin/wl-paste"),
		"wl-copy":  WlCopyShim(18339, "/usr/bin/wl-copy"),
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0755)
		if !isOurShim(p) {
			t.Errorf("generated %s shim must be recognized as ours", name)
		}
	}

	// Non-existent
	if isOurShim(filepath.Join(dir, "nonexistent")) {
		t.Fatal("should not detect non-existent as our shim")
	}
}

// TestInstallRefusesForeignWlCopyWithoutHalfInstalling pins both halves of the
// Wayland companion case: a user-owned wl-copy must survive, and the failed
// install must not leave the main shim behind — following the error's own
// advice used to fail a second time because wl-paste now existed.
func TestInstallRefusesForeignWlCopyWithoutHalfInstalling(t *testing.T) {
	dir := t.TempDir()
	wlCopy := filepath.Join(dir, "wl-copy")
	original := "#!/bin/sh\n# my own wrapper, mentions cc-clip in passing\nexec /usr/bin/wl-copy \"$@\"\n"
	if err := os.WriteFile(wlCopy, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(TargetWlPaste, dir, 18339); err == nil {
		t.Fatal("Install must refuse a user-owned wl-copy")
	}

	got, err := os.ReadFile(wlCopy)
	if err != nil || string(got) != original {
		t.Fatalf("user's wl-copy was modified: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "wl-paste")); !os.IsNotExist(err) {
		t.Fatal("a refused install must not leave the main shim installed")
	}
}

func TestXclipShimContent(t *testing.T) {
	content := XclipShim(18339, "/usr/bin/xclip")

	checks := []string{
		"#!/bin/bash",
		"cc-clip",
		"18339",
		"/usr/bin/xclip",
		"TARGETS",
		"image/",
		"_cc_clip_fallback",
		"CC_CLIP_SESSION_FILE",
		"X-CC-Clip-Session",
		"_cc_clip_session_header",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("shim missing expected content: %q", check)
		}
	}
}

// TestInstallRefusesToOverwriteForeignBinary pins the destructive case: a real
// program sitting at the shim path was truncated in place, with no backup and
// no error.
func TestInstallRefusesToOverwriteForeignBinary(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "xclip")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\nexec /usr/bin/xclip \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Install(TargetXclip, dir, 18339); err == nil {
		t.Fatal("Install overwrote a program it did not write")
	}

	after, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("existing program was modified:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestInstallReplacesSymlinkWithoutTouchingTarget pins the symlink case.
//
// Writing THROUGH the link rewrote its target, turning the user's real binary
// into a copy of the shim. Replacing the link itself destroys nothing, so this
// must succeed rather than refuse — and the shim must fall back to the program
// the link pointed at, which a PATH search could never find again because it
// deliberately skips the install dir.
func TestInstallReplacesSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-xclip")
	if err := os.WriteFile(real, []byte("real binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "xclip")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := Install(TargetXclip, dir, 18339)
	if err != nil {
		t.Fatalf("replacing a symlink destroys nothing and must not be refused: %v", err)
	}

	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "real binary" {
		t.Fatalf("symlink target was rewritten: %q", got)
	}
	if res.RealBinPath != real {
		t.Errorf("shim must fall back to the former symlink target; RealBinPath = %q, want %q", res.RealBinPath, real)
	}
	if len(res.Notes) == 0 {
		t.Error("replacing a user's symlink must be reported, not silent")
	}
	if !isOurShim(filepath.Join(dir, "xclip")) {
		t.Error("the shim was not installed")
	}
}

// TestInstallAdoptsForeignFileOnConsent pins the recovery path for the case
// that genuinely would destroy something: the file is moved aside, not deleted,
// and the shim keeps delegating to it.
// TestInstallRollsBackAdoptionsWhenACompanionWriteFails pins the adopt path's
// promise on failure: a program moved aside is moved back, so a failed install
// leaves the user's binaries where they were.
func TestInstallRollsBackAdoptionsWhenACompanionWriteFails(t *testing.T) {
	dir := t.TempDir()
	originals := map[string]string{
		"wl-paste": "#!/bin/sh\nexec /usr/bin/wl-paste \"$@\"\n",
		"wl-copy":  "#!/bin/sh\nexec /usr/bin/wl-copy \"$@\"\n",
	}
	for name, body := range originals {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realWrite := writeShim
	t.Cleanup(func() { writeShim = realWrite })
	writeShim = func(path, content string) error {
		if filepath.Base(path) == "wl-copy" {
			return errors.New("injected write failure")
		}
		return realWrite(path, content)
	}

	if _, err := InstallWithOptions(TargetWlPaste, dir, 18339, InstallOptions{AdoptForeign: true}); err == nil {
		t.Fatal("expected the injected wl-copy write failure to fail the install")
	}

	for name, body := range originals {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s must be back at its original path: %v", name, err)
		}
		if string(got) != body {
			t.Errorf("%s at its original path is not the user's program: %q", name, got)
		}
		if _, err := os.Lstat(path + AdoptedSuffix); !os.IsNotExist(err) {
			t.Errorf("%s must not be left moved aside at %s", name, path+AdoptedSuffix)
		}
	}
}

func TestInstallAdoptsForeignFileOnConsent(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "xclip")
	original := "#!/bin/sh\nexec /usr/bin/xclip \"$@\"\n"
	if err := os.WriteFile(victim, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	// Without consent it still refuses, and names the flag.
	_, err := Install(TargetXclip, dir, 18339)
	if err == nil {
		t.Fatal("a foreign regular file must still be refused by default")
	}
	if !strings.Contains(err.Error(), AdoptFlagHint) {
		t.Errorf("the refusal must name the recovery flag, got: %v", err)
	}

	res, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true})
	if err != nil {
		t.Fatalf("adopting install failed: %v", err)
	}

	moved := victim + AdoptedSuffix
	got, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("the user's program must be moved aside, not deleted: %v", err)
	}
	if string(got) != original {
		t.Fatalf("the moved program was modified: %q", got)
	}
	if res.RealBinPath != moved {
		t.Errorf("shim must fall back to the adopted program; RealBinPath = %q, want %q", res.RealBinPath, moved)
	}
	if !isOurShim(victim) {
		t.Error("the shim was not installed at the original path")
	}
}
