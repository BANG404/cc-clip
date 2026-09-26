package shim

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Target string

const (
	TargetXclip   Target = "xclip"
	TargetWlPaste Target = "wl-paste"
	TargetAuto    Target = "auto"
)

type InstallResult struct {
	Target      Target
	ShimPath    string
	RealBinPath string
	InstallDir  string
	// Notes describes anything the install did to a file it found in the way,
	// so the caller can tell the user rather than leaving it to be discovered.
	Notes []string
}

func DetectTarget() Target {
	if _, err := exec.LookPath("wl-paste"); err == nil {
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			return TargetWlPaste
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return TargetXclip
	}
	// Default to xclip even if not present (most common on X11 servers)
	return TargetXclip
}

func resolveTarget(target Target) Target {
	if target == TargetAuto {
		return DetectTarget()
	}
	return target
}

func findRealBinary(name string, shimDir string) (string, error) {
	absShimDir, _ := filepath.Abs(shimDir)

	// First: try `which -a` to get all resolved paths, pick the first that isn't our shim dir
	whichCmd := exec.Command("which", "-a", name)
	out, err := whichCmd.Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			resolved := strings.TrimSpace(line)
			if resolved == "" {
				continue
			}
			absResolved, _ := filepath.Abs(resolved)
			if filepath.Dir(absResolved) == absShimDir {
				continue
			}
			return resolved, nil
		}
	}

	// Fallback: manual PATH scan (e.g., `which -a` unavailable)
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if absDir == absShimDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("real %s binary not found in PATH", name)
}

func defaultInstallDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", ".local", "bin")
	}
	return filepath.Join(home, ".local", "bin")
}

// Install writes the shim for target into installDir.
//
// Install reports what it did through InstallResult so callers can tell the
// user when an existing file was adopted rather than silently replaced.
func Install(target Target, installDir string, port int) (InstallResult, error) {
	return InstallWithOptions(target, installDir, port, InstallOptions{})
}

// InstallOptions carries the caller's consent for destinations that cannot be
// handled without touching a file cc-clip did not write.
type InstallOptions struct {
	// AdoptForeign moves an existing, non-cc-clip REGULAR file aside to
	// "<path>.cc-clip-real" and points the shim at it there, instead of
	// refusing. Nothing is deleted and the user's binary keeps serving as the
	// shim's fallback. Symlinks never need this: replacing a link leaves its
	// target untouched.
	AdoptForeign bool
}

func InstallWithOptions(target Target, installDir string, port int, opts InstallOptions) (InstallResult, error) {
	resolved := resolveTarget(target)

	if installDir == "" {
		installDir = defaultInstallDir()
	}

	if err := os.MkdirAll(installDir, 0755); err != nil {
		return InstallResult{}, fmt.Errorf("failed to create install dir %s: %w", installDir, err)
	}

	binName := string(resolved)
	shimPath := filepath.Join(installDir, binName)

	// Check if shim already installed by us
	if isOurShim(shimPath) {
		return InstallResult{}, fmt.Errorf("shim already installed at %s; run 'cc-clip uninstall' first", shimPath)
	}

	// Decide what to do about EVERY destination before touching ANY of them.
	// Writing the wl-paste shim and only then discovering a user-owned wl-copy
	// left a half-installed deployment: the main shim was already on PATH, the
	// install reported failure, and following its own advice (move the
	// conflicting file aside, re-run) then failed again because the main shim
	// now existed.
	shimDelegate, applyShim, notes, err := prepareShimPath(shimPath, opts.AdoptForeign)
	if err != nil {
		return InstallResult{}, err
	}

	var wlCopyPath, wlCopyDelegate string
	var applyWlCopy func() error
	if resolved == TargetWlPaste {
		wlCopyPath = filepath.Join(installDir, "wl-copy")
		var wlNotes []string
		wlCopyDelegate, applyWlCopy, wlNotes, err = prepareShimPath(wlCopyPath, opts.AdoptForeign)
		if err != nil {
			return InstallResult{}, err
		}
		notes = append(notes, wlNotes...)
	}

	// The binary the shim falls back to. An adopted occupant wins: it IS the
	// program that used to answer on this path, and a PATH search deliberately
	// skips installDir so it could never find it again.
	realPath := shimDelegate
	if realPath == "" {
		realPath, err = findRealBinary(binName, installDir)
		if err != nil {
			// No real binary found — that's OK for SSH servers without display
			realPath = fmt.Sprintf("/usr/bin/%s", binName)
		}
	}

	var shimContent string
	switch resolved {
	case TargetXclip:
		shimContent = XclipShim(port, realPath)
	case TargetWlPaste:
		shimContent = WlPasteShim(port, realPath)
	default:
		return InstallResult{}, fmt.Errorf("unsupported target: %s", resolved)
	}

	realWlCopy := wlCopyDelegate
	var wlCopyContent string
	if resolved == TargetWlPaste {
		if realWlCopy == "" {
			realWlCopy, err = findRealBinary("wl-copy", installDir)
			if err != nil {
				realWlCopy = "/usr/bin/wl-copy"
			}
		}
		wlCopyContent = WlCopyShim(port, realWlCopy)
	}

	// Every destination is cleared; now perform the side effects. Each one
	// registers its undo, and any later failure replays them newest first: an
	// adopted program moved aside and never moved back is exactly the loss the
	// adopt path promises not to cause.
	var rollback []func() error
	fail := func(err error) (InstallResult, error) {
		errs := []error{err}
		for i := len(rollback) - 1; i >= 0; i-- {
			if rbErr := rollback[i](); rbErr != nil {
				errs = append(errs, fmt.Errorf("rollback incomplete: %w", rbErr))
			}
		}
		return InstallResult{}, errors.Join(errs...)
	}
	if applyShim != nil {
		if err := applyShim(); err != nil {
			return InstallResult{}, err
		}
		rollback = append(rollback, func() error { return os.Rename(shimDelegate, shimPath) })
	}
	if applyWlCopy != nil {
		if err := applyWlCopy(); err != nil {
			return fail(err)
		}
		rollback = append(rollback, func() error { return os.Rename(wlCopyDelegate, wlCopyPath) })
	}

	if err := writeShim(shimPath, shimContent); err != nil {
		return fail(err)
	}
	rollback = append(rollback, func() error { return os.Remove(shimPath) })

	// Wayland writes go through wl-copy, a separate binary from wl-paste, so
	// the wayland target ships a write-side companion shim (#128 phase 2).
	// xclip needs none: reads and writes share one binary, and the write
	// branch lives in the same script. Best-effort like the main shim: a
	// missing real wl-copy still installs the shim (forward-only remote).
	if wlCopyPath != "" {
		if err := writeShim(wlCopyPath, wlCopyContent); err != nil {
			// The pre-flight above passed, so this is an I/O failure rather
			// than a refusal. Roll back everything this call changed so the
			// deployment is not left half-applied.
			return fail(err)
		}
	}

	return InstallResult{
		Target:      resolved,
		ShimPath:    shimPath,
		RealBinPath: realPath,
		InstallDir:  installDir,
		Notes:       notes,
	}, nil
}

func Uninstall(target Target, installDir string) error {
	resolved := resolveTarget(target)

	if installDir == "" {
		installDir = defaultInstallDir()
	}

	binName := string(resolved)
	shimPath := filepath.Join(installDir, binName)

	if !isOurShim(shimPath) {
		return fmt.Errorf("%s is not a cc-clip shim (or does not exist)", shimPath)
	}

	if err := os.Remove(shimPath); err != nil {
		return fmt.Errorf("failed to remove shim: %w", err)
	}

	// The wayland install ships a wl-copy companion; remove it with its
	// wl-paste sibling, but only when it is genuinely ours.
	if resolved == TargetWlPaste {
		wlCopyPath := filepath.Join(installDir, "wl-copy")
		if isOurShim(wlCopyPath) {
			if err := os.Remove(wlCopyPath); err != nil {
				return fmt.Errorf("failed to remove wl-copy shim: %w", err)
			}
		}
	}

	return nil
}

// writeShimFile installs a shim at path without destroying anything that is
// not ours.
//
// Two failure modes this guards against, both reproduced against a real
// install dir: an existing program at the shim path was truncated in place,
// and a symlink at the shim path had its TARGET rewritten — so
// `~/.local/bin/xclip -> ../real-xclip` turned the user's real binary into a
// copy of the shim, with no backup. Anything at the path that cc-clip did not
// write is now refused rather than overwritten, and the write itself goes to a
// sibling temp file that is renamed over the path, so a symlink is replaced
// instead of followed.
// AdoptedSuffix is appended to a foreign file moved aside by an adopting
// install. The shim then delegates to it, so the program that used to answer on
// that path still answers — one directory entry over.
const AdoptedSuffix = ".cc-clip-real"

// AdoptFlagHint is the flag users are pointed at when an install is refused.
// Kept next to the refusal so the message and the flag cannot drift apart.
const AdoptFlagHint = "--adopt-foreign-shim"

// prepareShimPath decides how to handle whatever occupies a shim destination,
// WITHOUT performing any side effect yet.
//
// The three occupants are not equally dangerous, and treating them alike is
// what made the safe fix feel like a regression:
//
//   - A SYMLINK can be replaced outright. The shim is installed with a
//     rename, which swaps the link itself and never touches the file it points
//     at — the original bug was WriteFile FOLLOWING the link. So the target is
//     adopted as the binary the shim falls back to and the install proceeds
//     normally. No consent is needed to destroy nothing.
//   - A REGULAR foreign file genuinely would be destroyed, so it is refused
//     unless the caller opted in. Opting in moves it aside rather than
//     deleting it.
//   - Our own shim, or nothing at all, needs no decision.
//
// The returned apply function performs the side effect and is called only
// after EVERY destination has been cleared, so a refusal on the second one
// cannot leave the first half-applied.
// adoptedDelegate returns the program an earlier --adopt-foreign-shim install
// moved aside for path, or "" when there is none.
//
// The sidecar is the only durable record of that program: its location was
// otherwise written only into the shim itself, so the uninstall-then-install
// cycle connect runs on --force or a port change fell back to a PATH search
// that skips installDir, and the user's program stopped being called.
func adoptedDelegate(path string) string {
	adopted := path + AdoptedSuffix
	if info, err := os.Lstat(adopted); err == nil && info.Mode().IsRegular() {
		return adopted
	}
	return ""
}

func prepareShimPath(path string, adoptForeign bool) (delegateTo string, apply func() error, notes []string, err error) {
	info, statErr := os.Lstat(path)
	switch {
	case statErr != nil && os.IsNotExist(statErr):
		return adoptedDelegate(path), nil, nil, nil
	case statErr != nil:
		return "", nil, nil, fmt.Errorf("failed to inspect %s: %w", path, statErr)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return "", nil, nil, fmt.Errorf("failed to read the symlink at %s: %w", path, readErr)
		}
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(path), resolved)
		}
		// Only delegate to a target that is actually there and actually a
		// program; a dangling or self-referential link says nothing useful
		// about which real binary to use.
		if resolved != path {
			if ti, tErr := os.Stat(resolved); tErr == nil && ti.Mode().IsRegular() {
				return resolved, nil, []string{fmt.Sprintf(
					"replaced the symlink at %s; it pointed at %s, which is untouched and is now what the shim falls back to",
					path, resolved)}, nil
			}
		}
		return "", nil, []string{fmt.Sprintf("replaced a dangling symlink at %s (its target %s does not exist)", path, resolved)}, nil
	}

	if isOurShim(path) {
		return adoptedDelegate(path), nil, nil, nil
	}

	if !info.Mode().IsRegular() {
		return "", nil, nil, fmt.Errorf("%s exists and is not a regular file (%s); "+
			"move it aside and re-run", path, info.Mode().Type())
	}

	if !adoptForeign {
		return "", nil, nil, fmt.Errorf("%s already exists and was not written by cc-clip.\n"+
			"Overwriting it would destroy it, so nothing was changed.\n"+
			"Re-run with %s to move it to %s and have the shim fall back to it there,\n"+
			"or move it out of the way yourself and re-run",
			path, AdoptFlagHint, path+AdoptedSuffix)
	}

	adopted := path + AdoptedSuffix
	if _, err := os.Lstat(adopted); err == nil {
		return "", nil, nil, fmt.Errorf("cannot move %s to %s: that path is already taken; "+
			"move it aside yourself and re-run", path, adopted)
	}
	apply = func() error {
		if err := os.Rename(path, adopted); err != nil {
			return fmt.Errorf("failed to move %s aside to %s: %w", path, adopted, err)
		}
		return nil
	}
	return adopted, apply, []string{fmt.Sprintf(
		"moved the existing %s to %s and pointed the shim at it there; nothing was deleted",
		path, adopted)}, nil
}

// writeShim is the write InstallWithOptions performs; a variable only so tests
// can fail one write and observe the rollback.
var writeShim = writeShimFile

// writeShimFile installs a shim at path.
//
// The write goes to a sibling temp file that is renamed over the destination,
// so a symlink there is REPLACED rather than followed — writing through one
// rewrote its target, turning the user's real binary into a copy of the shim
// with no backup. Callers decide whether path may be replaced at all; see
// prepareShimPath.
func writeShimFile(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".cc-clip-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write shim: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write shim: %w", err)
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to set shim mode: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to install shim at %s: %w", path, err)
	}
	return nil
}

// ShimOwnerMarker is the exact line every shim this package installs carries in
// its header. Ownership is keyed off it rather than off a bare "cc-clip"
// substring: a user's own wrapper that merely MENTIONS cc-clip in a comment was
// classified as ours and overwritten, which is the same data loss the install
// guard exists to prevent. The line is stable across all three templates and
// across every shim already deployed, so tightening it does not strand an
// existing install from uninstall. connect's remote presence probe reads the
// same constants, so the two ownership decisions cannot drift apart.
const ShimOwnerMarker = "# Installed by: cc-clip install"

// ShimHeaderBytes bounds the ownership read. The marker is in the first few
// lines; reading further would only make a large foreign binary expensive to
// classify.
const ShimHeaderBytes = 512

func isOurShim(path string) bool {
	// Lstat first: a symlink is never something this package wrote, and
	// following one would classify by its TARGET's content.
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, ShimHeaderBytes)
	n, _ := io.ReadFull(f, header)
	return strings.Contains(string(header[:n]), ShimOwnerMarker)
}

func CheckPathPriority(installDir string) (bool, string) {
	absInstall, err := filepath.Abs(installDir)
	if err != nil {
		return false, "cannot resolve install dir"
	}

	// Check what `which xclip` actually resolves to
	for _, binName := range []string{"xclip", "wl-paste"} {
		shimPath := filepath.Join(absInstall, binName)
		if _, err := os.Stat(shimPath); err != nil {
			continue
		}
		// Shim exists — check if `which` resolves to our shim
		whichCmd := exec.Command("which", binName)
		out, err := whichCmd.Output()
		if err != nil {
			continue
		}
		resolved := strings.TrimSpace(string(out))
		absResolved, _ := filepath.Abs(resolved)
		absShim, _ := filepath.Abs(shimPath)

		if absResolved == absShim {
			return true, fmt.Sprintf("'which %s' resolves to %s (shim)", binName, resolved)
		}
		return false, fmt.Sprintf("'which %s' resolves to %s, not %s; shim won't take priority", binName, resolved, shimPath)
	}

	return false, fmt.Sprintf("%s has no shim installed, or is not in PATH", installDir)
}
