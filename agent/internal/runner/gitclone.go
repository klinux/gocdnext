package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// gitclone.go centralises the ONE git clone+checkout used by both workspace
// modes (isolated Prep and shared checkout). It bounds the transfer with a
// partial clone (--filter=blob:none), keeps every credential out of logs and
// errors, and falls back to a full clone once — and only — when the failure is
// specifically a remote that does not support partial clone.

// urlCredentialRE matches the userinfo of any `scheme://user:pass@host` URL —
// scheme-agnostic (https/http/ssh/git+ssh/…) — so a credential embedded in a
// git message, a redirect ("redirected from X to Y"), or an SSH remote is
// scrubbed regardless of surrounding punctuation or quoting. Userinfo cannot
// contain '/', whitespace, quotes or '@', so the class is safe and stops at the
// first '@'. The scp-like form `git@host:path` has no `://` and is not a
// credential, so it is deliberately left untouched. Compiled once.
var urlCredentialRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s'"@]*@`)

// redactURLCredential strips the credential from EVERY credentialed URL in an
// arbitrary log line, leaving scheme+host for debuggability. It does not parse
// the line as a URL (fragile on a sentence); it pattern-matches, so an
// unparseable-but-credentialed substring is still redacted conservatively.
func redactURLCredential(s string) string {
	return urlCredentialRE.ReplaceAllString(s, "$1***@")
}

// cloneEmit surfaces one already-sanitized git output line to the caller's log
// sink (LogLine protos in shared mode, an io.Writer in isolated Prep).
type cloneEmit func(stream, line string)

// cloneClassifier accumulates sticky signals from git's streamed stderr. The
// decision must not read a bounded tail buffer alone: the deciding line can
// appear early and be pushed out by later noise, so every line updates the
// flags as it streams.
type cloneClassifier struct {
	filterUnsupported bool
	promisorMissing   bool
	filterIgnored     bool
	authOrNetwork     bool // negative precedence — vetoes the fallback
}

func (c *cloneClassifier) observe(line string) {
	l := strings.ToLower(line)

	// Auth / network first: these VETO the fallback even when a promisor/filter
	// signal also fires, so a real credential or connectivity failure never pays
	// a doomed second clone. Over-matching here is safe (it only suppresses a
	// retry), so the set is deliberately broad.
	for _, s := range []string{
		"authentication failed", "could not read username", "could not read password",
		"invalid username or password", "http basic: access denied", "access denied",
		"403", "401", "permission denied",
		"could not resolve host", "connection refused", "connection timed out",
		"timed out", "failed to connect", "network is unreachable", "no route to host",
	} {
		if strings.Contains(l, s) {
			c.authOrNetwork = true
			break
		}
	}

	if strings.Contains(l, "filter") &&
		(strings.Contains(l, "unsupported") || strings.Contains(l, "does not") ||
			strings.Contains(l, "unknown") || strings.Contains(l, "invalid")) {
		c.filterUnsupported = true
	}
	if strings.Contains(l, "unadvertised object") {
		c.promisorMissing = true
	}
	if (strings.Contains(l, "missing") || strings.Contains(l, "promisor")) &&
		(strings.Contains(l, "promisor") || strings.Contains(l, "partial") || strings.Contains(l, "filter")) {
		c.promisorMissing = true
	}
	// A remote that accepts the command but ignores the filter (full clone, exit
	// 0). Not a failure — a diagnostic for a prep time that did not improve.
	if strings.Contains(l, "filter") && strings.Contains(l, "ignor") {
		c.filterIgnored = true
	}
}

// shouldFallback reports whether a failed filtered attempt should be retried
// unfiltered. Auth/network takes negative precedence over any promisor/filter
// signal.
func (c *cloneClassifier) shouldFallback() bool {
	if c.authOrNetwork {
		return false
	}
	return c.filterUnsupported || c.promisorMissing
}

// cloneCheckout clones mc into baseDir/<targetDir> as a partial clone and checks
// out mc.Revision, as ONE unit — the fallback trigger can surface at either the
// clone or the checkout (the checkout is where blob:none fetches the revision's
// blobs). echo writes the "$ git …" command line; emit writes each streamed
// output line. Both receive credential-free text.
func cloneCheckout(ctx context.Context, baseDir string, mc *gocdnextv1.MaterialCheckout, echo func(string), emit cloneEmit) error {
	if mc == nil {
		return fmt.Errorf("nil checkout")
	}
	if mc.GetUrl() == "" {
		return fmt.Errorf("checkout missing url")
	}
	url := mc.GetUrl()
	branch := mc.GetBranch()
	rev := mc.GetRevision()
	target := filepath.Join(baseDir, mc.GetTargetDir())

	res := runCloneAttempt(ctx, url, branch, target, rev, true, echo, emit)
	if res.err == nil {
		if res.cls.filterIgnored {
			echo("── partial clone not honored by remote (full clone performed)")
		}
		return nil
	}
	if !res.cls.shouldFallback() {
		return res.err
	}

	// Retry once, unfiltered. Remove the partial target first (a plain clone
	// fails if the dir exists non-empty), guarding against ever RemoveAll-ing
	// outside the call's own base dir.
	if err := validateTargetWithinBase(baseDir, target); err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("clean partial checkout target: %w", err)
	}
	echo("── partial clone unsupported by remote, falling back to full clone")
	return runCloneAttempt(ctx, url, branch, target, rev, false, echo, emit).err
}

type cloneAttempt struct {
	err error
	cls cloneClassifier
}

// runCloneAttempt runs one clone (optionally filtered) + checkout, sanitising
// and classifying every streamed line. Returned errors never contain the URL.
func runCloneAttempt(ctx context.Context, url, branch, target, rev string, filter bool, echo func(string), emit cloneEmit) cloneAttempt {
	var cls cloneClassifier
	onLine := func(stream, raw string) {
		// Sanitize BEFORE classification and BEFORE emit: the classifier's
		// signatures are outside the credential, so redaction does not blind it,
		// and no raw token is ever retained or logged.
		line := redactURLCredential(raw)
		cls.observe(line)
		emit(stream, line)
	}

	args := []string{"clone", "--quiet"}
	if filter {
		args = append(args, "--filter=blob:none")
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if rev != "" {
		// Skip the initial checkout of the default branch's tip — otherwise its
		// blobs are fetched before we reach the real revision (wasteful, and wrong
		// for an off-branch SHA). The explicit checkout below hydrates only rev.
		args = append(args, "--no-checkout")
	}
	// `--` before the positionals: the URL is an attacker-influenceable material
	// value, so without the separator a URL like `--upload-pack=…` would be
	// parsed as a git option (arbitrary command execution).
	args = append(args, "--", url, target)

	echo(fmt.Sprintf("$ git %v", redactCloneArgs(args, url)))
	if code, err := runGitStreaming(ctx, "", args, onLine); err != nil {
		return cloneAttempt{err: fmt.Errorf("git clone failed: %w", err), cls: cls}
	} else if code != 0 {
		return cloneAttempt{err: fmt.Errorf("git clone exited %d", code), cls: cls}
	}

	if rev != "" {
		revArgs := []string{"-C", target, "checkout", "--quiet", rev}
		echo(fmt.Sprintf("$ git %v", revArgs))
		if code, err := runGitStreaming(ctx, "", revArgs, onLine); err != nil {
			return cloneAttempt{err: fmt.Errorf("git checkout failed: %w", err), cls: cls}
		} else if code != 0 {
			return cloneAttempt{err: fmt.Errorf("git checkout %s exited %d", rev, code), cls: cls}
		}
	}
	return cloneAttempt{cls: cls}
}

// validateTargetWithinBase refuses a RemoveAll target that is not strictly
// inside the call's own base dir. Uses Abs on BOTH sides (the shared
// WorkspaceRoot can arrive relative) and Rel — never a string prefix, which
// would accept "/workspace2" for base "/workspace".
func validateTargetWithinBase(baseDir, target string) error {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("resolve base dir: %w", err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve target dir: %w", err)
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return fmt.Errorf("target outside workspace: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to clean target %q outside workspace", target)
	}
	return nil
}

// runGitStreaming execs `git args...`, invoking onLine(stream, rawLine) for each
// stdout/stderr line. Returns the exit code (0 on success) and an error ONLY for
// lifecycle failures (fork/wait); a non-zero exit is NOT an error. It is the one
// exec path for the clone flow, so sanitisation and classification sit in a
// single place.
func runGitStreaming(ctx context.Context, dir string, args []string, onLine func(stream, line string)) (int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	var wg sync.WaitGroup
	scan := func(rd io.Reader, stream string) {
		defer wg.Done()
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			onLine(stream, sc.Text())
		}
	}
	wg.Add(2)
	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
