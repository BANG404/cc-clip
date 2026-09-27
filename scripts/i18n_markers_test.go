package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// i18nRepo is a throwaway git repository the marker check runs against.
type i18nRepo struct {
	t   *testing.T
	dir string
	env []string
}

func newI18nRepo(t *testing.T) *i18nRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the marker check is a bash script run by the Linux CI job")
	}
	for _, bin := range []string{"bash", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	home := t.TempDir()
	r := &i18nRepo{t: t, dir: t.TempDir(), env: append(os.Environ(),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)}
	r.git("init", "-q")
	return r
}

func (r *i18nRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the file and commits it, returning the new commit SHA.
func (r *i18nRepo) commit(name, content, msg string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", name)
	r.git("commit", "-q", "--no-verify", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

func (r *i18nRepo) check() (string, error) {
	r.t.Helper()
	script, err := filepath.Abs("check-i18n-markers.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func marker(sha string) string {
	return "<!-- i18n-source: README.md @ " + sha + " -->\n\n译文\n"
}

func TestI18nMarkerCheckPassesWhenTranslationIsCurrent(t *testing.T) {
	r := newI18nRepo(t)
	source := r.commit("README.md", "English v1\n", "docs: source")
	r.commit("README.zh-CN.md", marker(source), "docs(i18n): translate")

	out, err := r.check()
	if err != nil {
		t.Fatalf("current translation must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ok: README.zh-CN.md") {
		t.Fatalf("expected an ok line for the translation:\n%s", out)
	}
}

func TestI18nMarkerCheckFailsWhenSourceChangedAfterMarker(t *testing.T) {
	r := newI18nRepo(t)
	source := r.commit("README.md", "English v1\n", "docs: source")
	r.commit("README.zh-CN.md", marker(source), "docs(i18n): translate")
	newer := r.commit("README.md", "English v2\n", "docs: new section")

	out, err := r.check()
	if err == nil {
		t.Fatalf("a translation behind its source must fail:\n%s", out)
	}
	for _, want := range []string{"README.zh-CN.md is behind README.md", "docs: new section", newer} {
		if !strings.Contains(out, want) {
			t.Errorf("output must contain %q:\n%s", want, out)
		}
	}
}

func TestI18nMarkerCheckFailsOnMalformedOrUnknownSHA(t *testing.T) {
	for _, tc := range []struct {
		name, sha, want string
	}{
		{"short sha", "abc1234", "not a full 40-character commit SHA"},
		{"unknown sha", strings.Repeat("0", 40), "not in the history of HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newI18nRepo(t)
			r.commit("README.md", "English v1\n", "docs: source")
			r.commit("README.zh-CN.md", marker(tc.sha), "docs(i18n): translate")

			out, err := r.check()
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("expected a failure containing %q, got err=%v:\n%s", tc.want, err, out)
			}
		})
	}
}
