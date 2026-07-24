package runner_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// fakeGit installs a stub `git` on PATH that records every invocation's argv to
// a log file and behaves per GIT_FAKE_MODE. It lets the tests assert the exact
// args git received (the `--`, `--no-checkout` and fallback contracts) and count
// clone attempts without needing a real remote.
func fakeGit(t *testing.T, mode string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "invocations.log")
	script := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "` + logPath + `"
sub="$1"
last=""; prev=""
for a in "$@"; do prev="$last"; last="$a"; done   # prev=url, last=target after '--'
has_filter=0
for a in "$@"; do [ "$a" = "--filter=blob:none" ] && has_filter=1; done
case "$GIT_FAKE_MODE" in
  filter_fail_then_ok)
    if [ "$sub" = "clone" ]; then
      if [ "$has_filter" = "1" ]; then
        echo "fatal: invalid filter-spec: server does not support --filter partial clone" >&2
        exit 128
      fi
      mkdir -p "$last"; exit 0
    fi
    exit 0 ;;
  auth_fail)
    if [ "$sub" = "clone" ]; then
      echo "fatal: unable to access, args were: $*" >&2
      echo "fatal: Authentication failed" >&2
      exit 128
    fi
    exit 0 ;;
  ok)
    if [ "$sub" = "clone" ]; then mkdir -p "$last"; fi
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_FAKE_MODE", mode)
	return logPath
}

func cloneLines(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "clone ") {
			out = append(out, l)
		}
	}
	return out
}

func TestCheckout_FallsBackWhenFilterUnsupported(t *testing.T) {
	log := fakeGit(t, "filter_fail_then_ok")
	base := t.TempDir()
	var buf bytes.Buffer

	err := runner.Checkout(context.Background(), base, &gocdnextv1.MaterialCheckout{
		Url: "https://host.example/org/repo.git", Branch: "main", TargetDir: "fixture",
	}, &buf)
	if err != nil {
		t.Fatalf("Checkout returned error: %v\nlog:\n%s", err, buf.String())
	}

	clones := cloneLines(t, log)
	if len(clones) != 2 {
		t.Fatalf("want 2 clone attempts (filtered then fallback), got %d:\n%v", len(clones), clones)
	}
	if !strings.Contains(clones[0], "--filter=blob:none") {
		t.Fatalf("first attempt should be filtered: %q", clones[0])
	}
	if strings.Contains(clones[1], "--filter=blob:none") {
		t.Fatalf("fallback attempt must drop --filter: %q", clones[1])
	}
	for i, c := range clones {
		if !strings.Contains(c, " -- ") {
			t.Fatalf("attempt %d missing `--` separator: %q", i, c)
		}
	}
}

func TestCheckout_NoFallbackOnAuthFailureAndRedactsURL(t *testing.T) {
	log := fakeGit(t, "auth_fail")
	base := t.TempDir()
	var buf bytes.Buffer

	const secret = "SUPERSECRET"
	url := "https://x-access-token:" + secret + "@host.example/org/repo.git"
	err := runner.Checkout(context.Background(), base, &gocdnextv1.MaterialCheckout{
		Url: url, Branch: "main", TargetDir: "fixture",
	}, &buf)
	if err == nil {
		t.Fatal("expected an error on auth failure")
	}
	// Auth veto: exactly one clone attempt, never a doomed second.
	if clones := cloneLines(t, log); len(clones) != 1 {
		t.Fatalf("want exactly 1 clone attempt (no fallback on auth), got %d:\n%v", len(clones), clones)
	}
	// Redaction across the streamed git output AND the returned error.
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked into streamed log:\n%s", buf.String())
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked into returned error: %v", err)
	}
}

func TestCheckout_NoCheckoutFlagFollowsRevision(t *testing.T) {
	base := t.TempDir()
	var buf bytes.Buffer

	// rev set → --no-checkout present.
	log := fakeGit(t, "ok")
	if err := runner.Checkout(context.Background(), base, &gocdnextv1.MaterialCheckout{
		Url: "https://host.example/r.git", Branch: "main", Revision: "deadbeef", TargetDir: "a",
	}, &buf); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if c := cloneLines(t, log); len(c) != 1 || !strings.Contains(c[0], "--no-checkout") {
		t.Fatalf("expected --no-checkout when rev set: %v", c)
	}

	// rev empty → no --no-checkout.
	log2 := fakeGit(t, "ok")
	if err := runner.Checkout(context.Background(), t.TempDir(), &gocdnextv1.MaterialCheckout{
		Url: "https://host.example/r.git", Branch: "main", TargetDir: "b",
	}, &buf); err != nil {
		t.Fatalf("Checkout (no rev): %v", err)
	}
	if c := cloneLines(t, log2); len(c) != 1 || strings.Contains(c[0], "--no-checkout") {
		t.Fatalf("did not expect --no-checkout when rev empty: %v", c)
	}
}

func TestCheckout_URLStartingWithDashIsPositional(t *testing.T) {
	log := fakeGit(t, "ok")
	base := t.TempDir()
	var buf bytes.Buffer

	// A malicious material URL that looks like a git option must be passed after
	// `--`, so it is a path argument, never `git clone --upload-pack=…`.
	url := "--upload-pack=/tmp/evil"
	_ = runner.Checkout(context.Background(), base, &gocdnextv1.MaterialCheckout{
		Url: url, TargetDir: "fixture",
	}, &buf)

	clones := cloneLines(t, log)
	if len(clones) == 0 {
		t.Fatal("no clone recorded")
	}
	// The `--` must appear before the URL token in the recorded argv.
	sepIdx := strings.Index(clones[0], " -- ")
	urlIdx := strings.Index(clones[0], url)
	if sepIdx < 0 || urlIdx < 0 || urlIdx < sepIdx {
		t.Fatalf("URL not passed positionally after `--`: %q", clones[0])
	}
}

func TestCheckout_OffBranchRevisionRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// A revision that lives on a SECOND branch, not the tip of the cloned
	// branch — proves blob:none (all refs kept) plus the explicit checkout
	// hydrates it, i.e. we did NOT add --single-branch.
	repo := setupTwoBranchRepo(t)
	base := t.TempDir()
	var buf bytes.Buffer

	err := runner.Checkout(context.Background(), base, &gocdnextv1.MaterialCheckout{
		Url: "file://" + repo.dir, Branch: "main", Revision: repo.otherSHA, TargetDir: "fixture",
	}, &buf)
	if err != nil {
		t.Fatalf("off-branch checkout failed: %v\n%s", err, buf.String())
	}
	got, err := os.ReadFile(filepath.Join(base, "fixture", "other.txt"))
	if err != nil {
		t.Fatalf("expected other.txt from the off-branch commit: %v", err)
	}
	if !strings.Contains(string(got), "on other branch") {
		t.Fatalf("wrong content: %q", got)
	}
}

type twoBranchRepo struct {
	dir      string
	otherSHA string
}

func setupTwoBranchRepo(t *testing.T) twoBranchRepo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "main commit")
	run("checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("on other branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "other.txt")
	run("commit", "-m", "feature commit")
	sha := run("rev-parse", "HEAD")
	// leave HEAD on main so `--branch main` clones main; feature's SHA is off-branch
	run("checkout", "main")
	return twoBranchRepo{dir: dir, otherSHA: sha}
}
