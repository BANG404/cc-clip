package shim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// Set only in isolated overlay subprocesses, never in production code.
var uninstallExchangeHook func(string, string) error
var uninstallProbeExchangeHook func(string, string) error
var uninstallExchangeCapability func(string, string) error
var uninstallCleanupHook func(string) error
var uninstallNoReplaceHook func(string, string) error

// Instrument the restore syscall boundary, not the preflight or syscall result.
// Recognizing Rename also lets this regression test run against 64728d6.
func runUninstallExchangeProbe(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("atomic exchange is only supported on Darwin and Linux")
	}
	// Read the Go toolchain locations before HOME moves: the child `go test`
	// otherwise derives GOPATH and GOCACHE from the temp HOME, re-downloads
	// modules into it, and t.TempDir cannot remove the read-only module cache.
	toolEnv := goToolchainEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CC_CLIP_TOKEN_DIR", filepath.Join(dir, "tokens"))
	if os.Getenv("CC_CLIP_TEST_EXCHANGE") == "1" {
		return true
	}
	sourcePath, err := filepath.Abs("install.go")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	call := "exchangePrograms(sidecar, path)"
	delegate := "exchangePrograms(from, to)"
	if !strings.Contains(string(source), call) {
		call = "os.Rename(sidecar, path)"
		delegate = "os.Rename(from, to)"
	}
	patched := strings.Replace(string(source), call, "uninstallTestExchange(sidecar, path)", 1)
	patched = strings.ReplaceAll(patched, "exchangePrograms(a.Name(), moved)", "uninstallTestProbeExchange(a.Name(), moved)")
	for _, cleanup := range []string{"os.Remove(sidecar)", "os.Remove(quarantine)"} {
		patched = strings.ReplaceAll(patched, cleanup, strings.Replace(cleanup, "os.Remove", "uninstallTestCleanup", 1))
	}
	if strings.Contains(patched, "renameProgramNoReplace(") {
		patched = strings.ReplaceAll(patched, "renameProgramNoReplace(", "uninstallTestNoReplace(")
		patched += `
func uninstallTestNoReplace(from, to string) error {
	if uninstallNoReplaceHook != nil {
		if err := uninstallNoReplaceHook(from, to); err != nil { return err }
	}
	return renameProgramNoReplace(from, to)
}
`
	}
	patched += `
func uninstallTestCleanup(path string) error {
	if uninstallCleanupHook != nil {
		if err := uninstallCleanupHook(path); err != nil { return err }
	}
	return os.Remove(path)
}

func init() {
	uninstallExchangeCapability = func(from, to string) error { return ` + delegate + ` }
}
func uninstallTestProbeExchange(from, to string) error {
	if uninstallProbeExchangeHook != nil {
		if err := uninstallProbeExchangeHook(from, to); err != nil { return err }
	}
	return exchangePrograms(from, to)
}
func uninstallTestExchange(from, to string) error {
	if uninstallExchangeHook != nil {
		if err := uninstallExchangeHook(from, to); err != nil { return err }
	}
	return ` + delegate + `
}
`
	replacement := filepath.Join(dir, "install.go")
	if err := os.WriteFile(replacement, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{sourcePath: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-race", "-overlay", overlayPath, ".", "-run", "^"+t.Name()+"$", "-count=1", "-v")
	cmd.Env = append(append(os.Environ(), toolEnv...), "CC_CLIP_TEST_EXCHANGE=1")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("exchange probe: %v", err)
	}
	return false
}

// goToolchainEnv returns the caller's GOPATH, GOMODCACHE and GOCACHE as
// KEY=value pairs, so a child `go` run under an isolated HOME still uses the
// real module and build caches.
func goToolchainEnv(t *testing.T) []string {
	t.Helper()
	keys := []string{"GOPATH", "GOMODCACHE", "GOCACHE"}
	out, err := exec.Command("go", append([]string{"env"}, keys...)...).Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(values) != len(keys) {
		t.Fatalf("go env returned %d values for %d keys: %q", len(values), len(keys), out)
	}
	env := make([]string, len(keys))
	for i, k := range keys {
		env[i] = k + "=" + values[i]
	}
	return env
}

func setupExchangeAdoption(t *testing.T) (dir, path, original string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "xclip")
	original = "#!/bin/sh\necho ORIGINAL\n"
	if err := os.WriteFile(path, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uninstallExchangeHook = nil; uninstallCleanupHook = nil; uninstallNoReplaceHook = nil })
	return
}

func TestUninstallExchangeConcurrentReplacement(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	// Only this test requires native exchange. Probe disposable entries on
	// the same temporary filesystem before scheduling the competing installer.
	probeDir := t.TempDir()
	from, to := filepath.Join(probeDir, "from"), filepath.Join(probeDir, "to")
	for _, p := range []string{from, to} {
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := uninstallExchangeCapability(from, to); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skipf("filesystem or kernel lacks atomic exchange: %v", err)
		}
		t.Fatal(err)
	}
	dir, path, original := setupExchangeAdoption(t)
	sidecar := path + AdoptedSuffix
	incoming := filepath.Join(dir, "incoming")
	const newcomer = "#!/bin/sh\necho NEW-FOREIGN-PROGRAM\n"
	if err := os.WriteFile(incoming, []byte(newcomer), 0o755); err != nil {
		t.Fatal(err)
	}
	originalInfo, _ := os.Stat(sidecar)
	newcomerInfo, _ := os.Stat(incoming)
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { <-ready; done <- os.Rename(incoming, path) }()
	uninstallExchangeHook = func(_, to string) error {
		if !isOurShim(to) {
			t.Error("shim missing before concurrent replacement")
		}
		close(ready)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return nil
	}
	err := Uninstall(TargetXclip, dir)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), sidecar) {
		t.Errorf("conflict must report both preserved programs: %v", err)
	}
	for p, want := range map[string]struct {
		data string
		info os.FileInfo
	}{path: {original, originalInfo}, sidecar: {newcomer, newcomerInfo}} {
		data, readErr := os.ReadFile(p)
		info, statErr := os.Stat(p)
		if readErr != nil || statErr != nil || string(data) != want.data || !os.SameFile(want.info, info) {
			t.Errorf("program not preserved at %s: got %q, read=%v stat=%v", p, data, readErr, statErr)
		}
	}
	t.Logf("concurrent replacement: uninstall=%v", err)
}

// The writer commits at the final removal boundary, after ownership inspection.
func TestUninstallCleanupConcurrentSidecarReplacement(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	dir, path, original := setupExchangeAdoption(t)
	sidecar := path + AdoptedSuffix
	incoming := filepath.Join(dir, "incoming-sidecar")
	const newcomer = "#!/bin/sh\necho NEW-FOREIGN-PROGRAM\n"
	if err := os.WriteFile(incoming, []byte(newcomer), 0755); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(incoming)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	uninstallCleanupHook = func(p string) error {
		// The capability probe also removes a private, empty quarantine.
		if !isOurShim(p) {
			return nil
		}
		calls++
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() { <-ready; done <- os.Rename(incoming, sidecar) }()
		close(ready)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		return nil
	}
	err = Uninstall(TargetXclip, dir)
	got, readErr := os.ReadFile(path)
	info, statErr := os.Lstat(sidecar)
	sidecarData, sidecarErr := os.ReadFile(sidecar)
	t.Logf("uninstall=%v; path=%q; sidecar=%q; sidecarRead=%v; cleanupCalls=%d", err, got, sidecarData, sidecarErr, calls)
	if calls != 1 {
		t.Errorf("expected one cleanup boundary, got %d", calls)
	}
	if statErr != nil || !os.SameFile(newInfo, info) || string(sidecarData) != newcomer {
		t.Errorf("NEW REGRESSION: concurrent sidecar update deleted: %v", statErr)
	}
	if readErr != nil || string(got) != original {
		t.Errorf("restored original changed: %q %v", got, readErr)
	}
}

func TestUninstallUnsupportedExchangeConcurrentNewcomer(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, f := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"ENOSYS", syscall.ENOSYS}, {"EINVAL", syscall.EINVAL}, {"ENOTSUP", syscall.ENOTSUP}, {"EOPNOTSUPP", syscall.EOPNOTSUPP},
	} {
		t.Run(f.name, func(t *testing.T) {
			dir, path, original := setupExchangeAdoption(t)
			sidecar := path + AdoptedSuffix
			incoming := filepath.Join(dir, "incoming")
			const newcomer = "#!/bin/sh\necho NEW-FOREIGN-PROGRAM\n"
			if err := os.WriteFile(incoming, []byte(newcomer), 0755); err != nil {
				t.Fatal(err)
			}
			originalInfo, _ := os.Stat(sidecar)
			newInfo, _ := os.Stat(incoming)
			uninstallExchangeHook = func(from, to string) error {
				if !isOurShim(to) {
					t.Error("missing initial shim")
				}
				ready, done := make(chan struct{}), make(chan error, 1)
				go func() { <-ready; done <- os.Rename(incoming, to) }()
				close(ready)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				return &os.LinkError{Op: "exchange", Old: from, New: to, Err: f.errno}
			}
			err := Uninstall(TargetXclip, dir)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), sidecar) {
				t.Errorf("expected conflict naming both paths: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "mv ") || strings.Contains(err.Error(), "removing the shim")) {
				t.Errorf("foreign destination must not receive destructive shim-removal guidance: %v", err)
			}
			for p, want := range map[string]struct {
				data string
				info os.FileInfo
			}{
				path: {newcomer, newInfo}, sidecar: {original, originalInfo},
			} {
				data, readErr := os.ReadFile(p)
				info, statErr := os.Lstat(p)
				t.Logf("uninstall=%v; %s=%q; read=%v stat=%v", err, p, data, readErr, statErr)
				if readErr != nil || statErr != nil || string(data) != want.data || !os.SameFile(want.info, info) {
					t.Errorf("program inode lost at %s", p)
				}
			}
		})
	}
}

func TestUninstallExchangeUnsupportedRefusal(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, fault := range []syscall.Errno{syscall.ENOSYS, syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			dir, path, _ := setupExchangeAdoption(t)
			sidecar := path + AdoptedSuffix
			shimInfo, _ := os.Stat(path)
			originalInfo, _ := os.Stat(sidecar)
			calls := 0
			uninstallExchangeHook = func(from, to string) error {
				calls++
				return &os.LinkError{Op: "exchange", Old: from, New: to, Err: fault}
			}
			err := Uninstall(TargetXclip, dir)
			if !errors.Is(err, fault) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), sidecar) {
				t.Errorf("unsupported exchange must refuse and identify retained entries: %v", err)
			}
			for p, want := range map[string]os.FileInfo{path: shimInfo, sidecar: originalInfo} {
				got, statErr := os.Stat(p)
				if statErr != nil || !os.SameFile(want, got) {
					t.Errorf("refusal changed %s: %v", p, statErr)
				}
			}
			if calls != 1 {
				t.Errorf("expected one exchange attempt, got %d", calls)
			}
			if err != nil && !strings.Contains(err.Error(), "mv "+shSingleQuote(sidecar)+" "+shSingleQuote(path)) {
				t.Errorf("refusal omits manual restoration guidance: %v", err)
			}
		})
	}
}

func TestUninstallNoReplaceUnsupportedRefusal(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, f := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"ENOSYS", syscall.ENOSYS}, {"EINVAL", syscall.EINVAL}, {"ENOTSUP", syscall.ENOTSUP}, {"EOPNOTSUPP", syscall.EOPNOTSUPP},
	} {
		t.Run(f.name, func(t *testing.T) {
			dir, path, _ := setupExchangeAdoption(t)
			sidecar := path + AdoptedSuffix
			shimInfo, _ := os.Stat(path)
			originalInfo, _ := os.Stat(sidecar)
			probeCalls, exchangeCalls := 0, 0
			uninstallExchangeHook = func(_, _ string) error { exchangeCalls++; return nil }
			uninstallNoReplaceHook = func(from, to string) error {
				probeCalls++
				if from == path || from == sidecar || to == path || to == sidecar {
					t.Error("capability probe touched shared program name")
				}
				return &os.LinkError{Op: "rename-noreplace", Old: from, New: to, Err: f.errno}
			}
			err := Uninstall(TargetXclip, dir)
			if !errors.Is(err, f.errno) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), sidecar) {
				t.Errorf("expected unsupported refusal naming retained programs: %v", err)
			}
			if probeCalls == 0 || exchangeCalls != 0 {
				t.Errorf("probe=%d exchange=%d; unsupported must precede exchange", probeCalls, exchangeCalls)
			}
			for p, want := range map[string]os.FileInfo{path: shimInfo, sidecar: originalInfo} {
				got, statErr := os.Stat(p)
				if statErr != nil || !os.SameFile(want, got) {
					t.Errorf("unsupported mutated %s: %v", p, statErr)
				}
			}
			t.Logf("uninstall=%v; probe=%d exchange=%d", err, probeCalls, exchangeCalls)
		})
	}
}

func TestUninstallQuarantineCollision(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	dir, path, original := setupExchangeAdoption(t)
	sidecar := path + AdoptedSuffix
	var collisions []string
	uninstallNoReplaceHook = func(from, to string) error {
		if from == sidecar {
			if err := os.WriteFile(to, []byte("FOREIGN QUARANTINE"), 0600); err != nil {
				t.Fatal(err)
			}
			collisions = append(collisions, to)
		}
		return nil
	}
	err := Uninstall(TargetXclip, dir)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Errorf("expected quarantine collision: %v", err)
	}
	if len(collisions) == 0 {
		t.Error("quarantine boundary not reached")
	}
	for _, p := range collisions {
		if err != nil && !strings.Contains(err.Error(), p) {
			t.Errorf("error omits quarantine collision location: %v", err)
		}
		got, readErr := os.ReadFile(p)
		if readErr != nil || string(got) != "FOREIGN QUARANTINE" {
			t.Errorf("collision overwritten or removed: %q %v", got, readErr)
		}
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original || !isOurShim(sidecar) {
		t.Errorf("unexpected post-exchange collision state: %q %v; error=%v", got, readErr, err)
	}
	t.Logf("uninstall=%v; collisions=%v", err, collisions)
}

func TestUninstallQuarantineForeignRestoration(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, collision := range []bool{false, true} {
		t.Run(fmt.Sprintf("restoration_collision_%t", collision), func(t *testing.T) {
			dir, path, original := setupExchangeAdoption(t)
			sidecar := path + AdoptedSuffix
			incoming := filepath.Join(dir, "incoming")
			if err := os.WriteFile(incoming, []byte("FOREIGN SIDECAR"), 0755); err != nil {
				t.Fatal(err)
			}
			foreignInfo, _ := os.Stat(incoming)
			quarantine := ""
			restoreCalls := 0
			uninstallNoReplaceHook = func(from, to string) error {
				if from == sidecar {
					quarantine = to
					if err := os.Rename(incoming, sidecar); err != nil {
						t.Fatal(err)
					}
				} else if from == quarantine && to == sidecar {
					restoreCalls++
					if collision {
						if err := os.WriteFile(sidecar, []byte("SECOND FOREIGN"), 0755); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			}
			uninstallCleanupHook = func(p string) error {
				data, _ := os.ReadFile(p)
				if len(data) == 0 {
					return nil
				} // private capability probe
				t.Errorf("foreign entry must never reach cleanup: %s", p)
				return syscall.EPERM
			}
			err := Uninstall(TargetXclip, dir)
			if err == nil || restoreCalls != 1 {
				t.Errorf("expected conflict and foreign restoration attempt: err=%v calls=%d", err, restoreCalls)
			}
			foreignPath := sidecar
			if collision {
				foreignPath = quarantine
			}
			info, statErr := os.Stat(foreignPath)
			if statErr != nil || !os.SameFile(foreignInfo, info) {
				t.Errorf("foreign inode lost at %s: %v", foreignPath, statErr)
			}
			if err != nil && !strings.Contains(err.Error(), foreignPath) {
				t.Errorf("error omits retained foreign location: %v", err)
			}
			if collision {
				got, readErr := os.ReadFile(sidecar)
				if readErr != nil || string(got) != "SECOND FOREIGN" {
					t.Errorf("restoration overwrote newcomer: %q %v", got, readErr)
				}
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != original {
				t.Errorf("original changed: %q %v", got, readErr)
			}
			t.Logf("uninstall=%v; foreignPath=%s", err, foreignPath)
		})
	}
}

func TestUninstallExchangeFailurePreservesBothEntries(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, fault := range []syscall.Errno{syscall.EPERM, syscall.EXDEV} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			dir, path, original := setupExchangeAdoption(t)
			sidecar := path + AdoptedSuffix
			shimInfo, _ := os.Stat(path)
			originalInfo, _ := os.Stat(sidecar)
			uninstallExchangeHook = func(_, _ string) error { return fault }
			err := Uninstall(TargetXclip, dir)
			if !errors.Is(err, fault) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), sidecar) {
				t.Errorf("failure must identify retained entries and cause: %v", err)
			}
			for p, before := range map[string]os.FileInfo{path: shimInfo, sidecar: originalInfo} {
				after, err := os.Stat(p)
				if err != nil || !os.SameFile(before, after) {
					t.Errorf("failed exchange changed %s: %v", p, err)
				}
			}
			if !isOurShim(path) {
				t.Error("failed exchange removed shim")
			}
			uninstallExchangeHook = nil
			if err := Uninstall(TargetXclip, dir); err != nil {
				t.Fatal(err)
			}
			assertAdoptedProgramRestored(t, path, original)
		})
	}
}

func assertAdoptedProgramRestored(t *testing.T, path, original string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatalf("uninstall must restore the original program at %s: got %q, err %v", path, got, err)
	}
	if _, err := os.Lstat(path + AdoptedSuffix); !os.IsNotExist(err) {
		t.Fatalf("restored program must not leave a sidecar at %s: %v", path+AdoptedSuffix, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("restored program lost its mode: %v", info.Mode())
	}
}

// Use a subprocess overlay to deny hard links without requiring a privileged
// Linux fixture or a mounted filesystem. All other filesystem operations are real.
func TestUninstallWithoutHardLinkSupport(t *testing.T) {
	if os.Getenv("CC_CLIP_TEST_DENY_LINK") != "1" {
		sourcePath, err := filepath.Abs("install.go")
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		for _, failure := range []string{"operation not permitted", "operation not supported"} {
			t.Run(failure, func(t *testing.T) {
				patched := strings.ReplaceAll(string(source), "os.Link(", "uninstallTestDeniedLink(")
				patched = strings.ReplaceAll(patched, "os.Rename(", "uninstallTestRename(")
				patched += fmt.Sprintf("\nfunc uninstallTestDeniedLink(_, _ string) error { return errors.New(%q) }\n", failure)
				// Observe the restore boundary too: remove-then-rename must not
				// pass merely because the final executable is eventually present.
				patched += `
func uninstallTestRename(from, to string) error {
	if strings.HasSuffix(from, AdoptedSuffix) && !isOurShim(to) {
		return errors.New("restore must rename directly over our still-present shim")
	}
	return os.Rename(from, to)
}
`
				dir := t.TempDir()
				replacement := filepath.Join(dir, "install.go")
				if err := os.WriteFile(replacement, []byte(patched), 0o600); err != nil {
					t.Fatal(err)
				}
				overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{sourcePath: replacement}})
				if err != nil {
					t.Fatal(err)
				}
				overlayPath := filepath.Join(dir, "overlay.json")
				if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("go", "test", "-overlay", overlayPath, ".", "-run", "^TestUninstallWithoutHardLinkSupport$", "-count=1", "-v")
				cmd.Env = append(os.Environ(), "CC_CLIP_TEST_DENY_LINK=1")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("hard-link fault probe: %v\n%s", err, out)
				}
			})
		}
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "xclip")
	original := "#!/bin/sh\necho ORIGINAL\n"
	if err := os.WriteFile(path, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
		t.Fatal(err)
	}
	err := Uninstall(TargetXclip, dir)
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("original executable path disappeared: %v", statErr)
	}
	if err != nil {
		t.Fatalf("renameable program must restore without hard links: %v", err)
	}
	assertAdoptedProgramRestored(t, path, original)
}

func TestUninstallWaylandPreflightsAllPaths(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		for _, conflict := range []string{"destination", "main-sidecar", "companion-sidecar"} {
			t.Run(fmt.Sprintf("adopted=%t/%s", adopted, conflict), func(t *testing.T) {
				dir := t.TempDir()
				main, companion := filepath.Join(dir, "wl-paste"), filepath.Join(dir, "wl-copy")
				for _, path := range []string{main, companion} {
					if path == main && !adopted {
						continue
					}
					if err := os.WriteFile(path, []byte("#!/bin/sh\necho ORIGINAL\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := InstallWithOptions(TargetWlPaste, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
					t.Fatal(err)
				}
				badPath := companion
				if conflict == "destination" {
					if err := os.WriteFile(badPath, []byte("#!/bin/sh\necho FOREIGN\n"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if conflict == "main-sidecar" {
						badPath = main
					}
					badPath += AdoptedSuffix
					if err := os.Remove(badPath); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if err := os.Symlink("missing-original", badPath); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				// Refusal must preserve identity, mode and bytes/link targets of
				// every entry, including absent sidecars.
				for _, path := range []string{main, companion, main + AdoptedSuffix, companion + AdoptedSuffix} {
					path := path
					info, statErr := os.Lstat(path)
					data, _ := os.ReadFile(path)
					link, _ := os.Readlink(path)
					t.Cleanup(func() {
						after, err := os.Lstat(path)
						if os.IsNotExist(statErr) && os.IsNotExist(err) {
							return
						}
						if err != nil || info == nil || !os.SameFile(info, after) || info.Mode() != after.Mode() {
							t.Errorf("entry changed on refused uninstall: %s (%v)", path, err)
							return
						}
						got, _ := os.ReadFile(path)
						gotLink, _ := os.Readlink(path)
						if string(got) != string(data) || gotLink != link {
							t.Errorf("contents changed on refused uninstall: %s", path)
						}
					})
				}
				if err := Uninstall(TargetWlPaste, dir); err == nil || !strings.Contains(err.Error(), badPath) {
					t.Fatalf("conflict must name preserved path %s: %v", badPath, err)
				}
			})
		}
	}
}

func TestUninstallRestoresAdoptedWlPaste(t *testing.T) {
	testUninstallRestoresWaylandProgram(t, "wl-paste")
}

func TestUninstallRestoresAdoptedWlCopyCompanion(t *testing.T) {
	testUninstallRestoresWaylandProgram(t, "wl-copy")
}

func testUninstallRestoresWaylandProgram(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	original := "#!/bin/sh\necho CUSTOM-" + name + "\n"
	if err := os.WriteFile(path, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallWithOptions(TargetWlPaste, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(TargetWlPaste, dir); err != nil {
		t.Fatal(err)
	}
	assertAdoptedProgramRestored(t, path, original)
	other := "wl-copy"
	if name == other {
		other = "wl-paste"
	}
	if _, err := os.Lstat(filepath.Join(dir, other)); !os.IsNotExist(err) {
		t.Fatalf("unadopted %s shim must be removed: %v", other, err)
	}
}

func TestUninstallPreservesAdoptedProgramWhenDestinationOccupied(t *testing.T) {
	for _, name := range []string{"xclip", "wl-copy"} {
		for _, occupant := range []string{"file", "dangling-symlink"} {
			t.Run(name+"/"+occupant, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, name)
				original := "#!/bin/sh\necho ORIGINAL\n"
				if err := os.WriteFile(path, []byte(original), 0o755); err != nil {
					t.Fatal(err)
				}
				target := TargetXclip
				if name == "wl-copy" {
					target = TargetWlPaste
				}
				if _, err := InstallWithOptions(target, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if occupant == "file" {
					if err := os.WriteFile(path, []byte("new occupant"), 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("missing", path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				err := Uninstall(target, dir)
				if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), path+AdoptedSuffix) {
					t.Errorf("restore conflict must name destination %s and preserved sidecar %s, got: %v", path, path+AdoptedSuffix, err)
				}
				if got, err := os.ReadFile(path + AdoptedSuffix); err != nil || string(got) != original {
					t.Fatalf("failed restore must preserve the sidecar: %q, %v", got, err)
				}
				if occupant == "file" {
					if got, err := os.ReadFile(path); err != nil || string(got) != "new occupant" {
						t.Fatalf("destination was overwritten: %q, %v", got, err)
					}
				} else if got, err := os.Readlink(path); err != nil || got != "missing" {
					t.Fatalf("destination symlink was overwritten: %q, %v", got, err)
				}
			})
		}
	}
}

func TestUninstallReportsUnrestorableSidecar(t *testing.T) {
	dir := t.TempDir()
	res, err := Install(TargetXclip, dir, 18339)
	if err != nil {
		t.Fatal(err)
	}
	// An unexpected sidecar type must be reported and preserved, not moved
	// into the executable path or silently abandoned.
	sidecar := res.ShimPath + AdoptedSuffix
	if err := os.Mkdir(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "program"), []byte("saved program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(TargetXclip, dir); err == nil || !strings.Contains(err.Error(), sidecar) {
		t.Errorf("failed restoration must name the preserved sidecar %s, got: %v", sidecar, err)
	}
	if got, err := os.ReadFile(filepath.Join(sidecar, "program")); err != nil || string(got) != "saved program" {
		t.Fatalf("unrestorable sidecar must be preserved: %q, %v", got, err)
	}
}

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

// TestInstallReportsAnIncompleteRollback pins that a failed undo is surfaced:
// the user must learn their program is still at the .cc-clip-real path.
func TestInstallReportsAnIncompleteRollback(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "xclip")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	realWrite := writeShim
	t.Cleanup(func() { writeShim = realWrite })
	writeShim = func(path, content string) error {
		// Make the undo impossible, then fail the write that triggers it.
		if err := os.Remove(victim + AdoptedSuffix); err != nil {
			t.Fatal(err)
		}
		return errors.New("injected write failure")
	}

	_, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true})
	if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
		t.Fatalf("a failed undo must be reported, got: %v", err)
	}
}

// Uninstall restores the user's program; reinstall needs fresh consent to adopt it.
func TestReinstallKeepsTheAdoptedProgramAsFallback(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "xclip")
	original := "#!/bin/sh\necho CUSTOM-FALLBACK\n"
	if err := os.WriteFile(victim, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
		t.Fatalf("adopting install failed: %v", err)
	}
	if err := Uninstall(TargetXclip, dir); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}

	assertAdoptedProgramRestored(t, victim, original)
	if _, err := Install(TargetXclip, dir, 18339); err == nil || !strings.Contains(err.Error(), AdoptFlagHint) {
		t.Fatalf("plain reinstall must refuse and name %s, got: %v", AdoptFlagHint, err)
	}
	assertAdoptedProgramRestored(t, victim, original)

	res, err := InstallWithOptions(TargetXclip, dir, 18339, InstallOptions{AdoptForeign: true})
	if err != nil {
		t.Fatalf("explicit re-adoption failed: %v", err)
	}

	adopted := victim + AdoptedSuffix
	if got, err := os.ReadFile(adopted); err != nil || string(got) != original {
		t.Fatalf("re-adoption must preserve the program: %q, %v", got, err)
	}
	if res.RealBinPath != adopted {
		t.Fatalf("reinstall must keep falling back to the adopted program; RealBinPath = %q, want %q", res.RealBinPath, adopted)
	}
	shimBody, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shimBody), adopted) {
		t.Fatal("the reinstalled shim does not delegate to the adopted program")
	}
}

// TestUninstallWaylandCapabilityRefusalChangesNothing pins the batch
// capability probe: with an unadopted wl-paste shim and an adopted wl-copy,
// an unsupported exchange refused only after wl-paste had been removed. The
// probe now runs on private entries before any shared name is touched.
func TestUninstallWaylandCapabilityRefusalChangesNothing(t *testing.T) {
	if !runUninstallExchangeProbe(t) {
		return
	}
	for _, f := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"ENOSYS", syscall.ENOSYS}, {"EINVAL", syscall.EINVAL}, {"ENOTSUP", syscall.ENOTSUP}, {"EOPNOTSUPP", syscall.EOPNOTSUPP},
	} {
		t.Run(f.name, func(t *testing.T) {
			dir := t.TempDir()
			wlCopy := filepath.Join(dir, "wl-copy")
			if err := os.WriteFile(wlCopy, []byte("#!/bin/sh\necho ORIGINAL-WL-COPY\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := InstallWithOptions(TargetWlPaste, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
				t.Fatal(err)
			}
			wlPaste := filepath.Join(dir, "wl-paste")
			before := map[string]os.FileInfo{}
			for _, p := range []string{wlPaste, wlCopy, wlCopy + AdoptedSuffix} {
				info, err := os.Stat(p)
				if err != nil {
					t.Fatalf("setup: %s: %v", p, err)
				}
				before[p] = info
			}
			realExchanges := 0
			uninstallExchangeHook = func(_, _ string) error { realExchanges++; return nil }
			uninstallProbeExchangeHook = func(_, _ string) error { return f.errno }
			t.Cleanup(func() { uninstallExchangeHook = nil; uninstallProbeExchangeHook = nil })

			err := Uninstall(TargetWlPaste, dir)
			if !errors.Is(err, f.errno) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("expected a refusal before any change: %v", err)
			}
			if realExchanges != 0 {
				t.Errorf("refusal must precede every real exchange, got %d", realExchanges)
			}
			for p, want := range before {
				got, statErr := os.Stat(p)
				if statErr != nil || !os.SameFile(want, got) {
					t.Errorf("%s changed by a refused uninstall: %v", p, statErr)
				}
			}
		})
	}
}

// TestUninstallResumesAfterACompanionRefusal pins that a Wayland uninstall
// interrupted after wl-paste was removed can be completed by running it again:
// the missing main shim with no sidecar counts as already uninstalled.
func TestUninstallResumesAfterACompanionRefusal(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("restore needs the atomic exchange available on Linux and Darwin")
	}
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	wlCopy := filepath.Join(dir, "wl-copy")
	original := "#!/bin/sh\necho ORIGINAL-WL-COPY\n"
	if err := os.WriteFile(wlCopy, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallWithOptions(TargetWlPaste, dir, 18339, InstallOptions{AdoptForeign: true}); err != nil {
		t.Fatal(err)
	}
	// The state an earlier run leaves when it removed wl-paste and then
	// refused on wl-copy.
	if err := os.Remove(filepath.Join(dir, "wl-paste")); err != nil {
		t.Fatal(err)
	}

	if err := Uninstall(TargetWlPaste, dir); err != nil {
		t.Fatalf("resumed uninstall must complete: %v", err)
	}
	got, err := os.ReadFile(wlCopy)
	if err != nil || string(got) != original {
		t.Fatalf("wl-copy not restored: %q, %v", got, err)
	}
	if _, err := os.Lstat(wlCopy + AdoptedSuffix); !os.IsNotExist(err) {
		t.Errorf("sidecar must be consumed by the restore: %v", err)
	}
	if err := Uninstall(TargetWlPaste, dir); err == nil {
		t.Error("with nothing left to uninstall, uninstall must still report it")
	}
}
