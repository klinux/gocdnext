package runner_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// collector captures everything the runner ships through its Send callback.
// Tests inspect logs + final result without needing a gRPC stream.
type collector struct {
	mu     sync.Mutex
	logs   []*gocdnextv1.LogLine
	result *gocdnextv1.JobResult
}

func (c *collector) Send(msg *gocdnextv1.AgentMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch k := msg.GetKind().(type) {
	case *gocdnextv1.AgentMessage_Log:
		c.logs = append(c.logs, k.Log)
	case *gocdnextv1.AgentMessage_Result:
		c.result = k.Result
	}
}

func (c *collector) allLogText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, l := range c.logs {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func newRunner(t *testing.T) (*runner.Runner, *collector) {
	t.Helper()
	c := &collector{}
	r := runner.New(runner.Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          c.Send,
	})
	return r, c
}

func assignment(tasks ...string) *gocdnextv1.JobAssignment {
	out := &gocdnextv1.JobAssignment{
		RunId: "run-1", JobId: "job-1", Name: "compile",
	}
	for _, script := range tasks {
		out.Tasks = append(out.Tasks, &gocdnextv1.TaskSpec{
			Kind: &gocdnextv1.TaskSpec_Script{Script: script},
		})
	}
	return out
}

func TestExecute_SingleSuccessfulScript(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	r.Execute(ctx, assignment("echo hello"))

	if c.result == nil {
		t.Fatalf("no result emitted")
	}
	if c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("status = %s, want SUCCESS", c.result.Status)
	}
	if c.result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", c.result.ExitCode)
	}
	if !strings.Contains(c.allLogText(), "hello") {
		t.Fatalf("expected 'hello' in logs, got:\n%s", c.allLogText())
	}
}

func TestExecute_CancelMidRunAbortsJob(t *testing.T) {
	// Regression cover for the cancel-kills-container flow. The job
	// runs a script that sleeps longer than the test's patience;
	// we Cancel() after it's clearly started and expect the shell
	// engine's context to be canceled so the script exits early
	// and the runner reports a non-success result within seconds.
	r, c := newRunner(t)
	a := assignment("sleep 30")

	done := make(chan struct{})
	go func() {
		r.Execute(context.Background(), a)
		close(done)
	}()

	// Give Execute enough time to reach the script loop and
	// register itself in the in-flight map. CI runners are slow
	// enough that 3s used to flake — 10s gives headroom without
	// hiding a real regression.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r.Cancel(a.GetJobId()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never registered in-flight within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return within 15s after Cancel")
	}

	if c.result == nil {
		t.Fatal("no result emitted")
	}
	if c.result.Status == gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("status = SUCCESS, want FAILED/CANCELED after cancel")
	}
}

func TestRunner_CancelUnknownReturnsFalse(t *testing.T) {
	// Late-arriving Cancel (job already finished or never ran on
	// this agent) is a no-op — caller logs it and moves on.
	r, _ := newRunner(t)
	if r.Cancel("ghost-job-id") {
		t.Fatal("Cancel on unknown job should return false")
	}
}

func TestExecute_FailingScriptReportsExitCode(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	r.Execute(ctx, assignment("exit 7"))

	if c.result == nil {
		t.Fatalf("no result emitted")
	}
	if c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("status = %s, want FAILED", c.result.Status)
	}
	if c.result.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7", c.result.ExitCode)
	}
}

func TestExecute_StopsOnFirstFailedTask(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	r.Execute(ctx, assignment("echo first", "exit 3", "echo should-not-run"))

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("result = %+v", c.result)
	}
	if strings.Contains(c.allLogText(), "should-not-run") {
		t.Fatalf("third task executed despite earlier failure")
	}
}

func TestExecute_PropagatesEnvAndStderr(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	a := assignment(`echo "FOO=$FOO"; echo err-to-stderr 1>&2`)
	a.Env = map[string]string{"FOO": "bar"}

	r.Execute(ctx, a)

	text := c.allLogText()
	if !strings.Contains(text, "FOO=bar") {
		t.Fatalf("env not propagated: %s", text)
	}
	if !strings.Contains(text, "err-to-stderr") {
		t.Fatalf("stderr not captured: %s", text)
	}

	var stderrSeen bool
	for _, l := range c.logs {
		if l.Stream == "stderr" && strings.Contains(l.Text, "err-to-stderr") {
			stderrSeen = true
		}
	}
	if !stderrSeen {
		t.Fatalf("stderr stream label missing")
	}
}

func TestExecute_LogLinesCarryRunAndJobIDsAndMonotonicSeq(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	a := assignment(`printf 'a\nb\nc\n'`)
	a.RunId = "run-xyz"
	a.JobId = "job-xyz"

	r.Execute(ctx, a)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) < 3 {
		t.Fatalf("logs = %d, want >= 3", len(c.logs))
	}
	var lastSeq int64
	for _, l := range c.logs {
		if l.RunId != "run-xyz" || l.JobId != "job-xyz" {
			t.Fatalf("log identity: %+v", l)
		}
		if l.Seq <= lastSeq {
			t.Fatalf("seq not monotonic: %d after %d", l.Seq, lastSeq)
		}
		lastSeq = l.Seq
	}
}

func TestExecute_MasksSecretValuesInLogs(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	// Secret value must be >= 4 chars so the length filter doesn't skip it.
	const secretValue = "ghp_supersecret_12345"
	a := assignment(`echo "token=$GH_TOKEN"; echo "prefix ghp_supersecret_12345 suffix"`)
	a.Env = map[string]string{"GH_TOKEN": secretValue}
	a.LogMasks = []string{secretValue}

	r.Execute(ctx, a)

	all := c.allLogText()
	if strings.Contains(all, secretValue) {
		t.Fatalf("secret value leaked in logs:\n%s", all)
	}
	if !strings.Contains(all, "token=***") {
		t.Fatalf("expected masked form 'token=***' in logs:\n%s", all)
	}
	if !strings.Contains(all, "prefix *** suffix") {
		t.Fatalf("expected masked middle 'prefix *** suffix':\n%s", all)
	}
	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("result = %+v", c.result)
	}
}

func TestExecute_ShortMaskValuesAreIgnored(t *testing.T) {
	r, c := newRunner(t)
	ctx := context.Background()

	// 3-char mask — below the length floor, must be left alone.
	a := assignment(`echo "xyz-hit"`)
	a.LogMasks = []string{"xyz"}

	r.Execute(ctx, a)

	if !strings.Contains(c.allLogText(), "xyz-hit") {
		t.Fatalf("short mask (<4) should not be applied:\n%s", c.allLogText())
	}
}

func TestExecute_GitCheckoutFromLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := setupLocalGitRepo(t)

	r, c := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Script uses a relative path ("README.md") — the runner
	// must cd into the checkout target so the user doesn't have
	// to hardcode the scheduler-assigned TargetDir in every step.
	a := assignment(`cat README.md`)
	a.Checkouts = []*gocdnextv1.MaterialCheckout{{
		MaterialId: "mat-1",
		Url:        "file://" + repoDir,
		Branch:     "main",
		TargetDir:  "fixture",
	}}

	r.Execute(ctx, a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("result = %+v, logs:\n%s", c.result, c.allLogText())
	}
	if !strings.Contains(c.allLogText(), "hello from fixture") {
		t.Fatalf("repo content missing from logs:\n%s", c.allLogText())
	}
}

func TestExecute_GitCheckoutFailureReportsFailed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r, c := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a := assignment("true")
	a.Checkouts = []*gocdnextv1.MaterialCheckout{{
		MaterialId: "mat-1",
		Url:        "file:///nonexistent/repo",
		Branch:     "main",
		TargetDir:  "x",
	}}

	r.Execute(ctx, a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("result = %+v", c.result)
	}
	if !strings.Contains(c.result.Error, "checkout") {
		t.Fatalf("error = %q, want to mention checkout", c.result.Error)
	}
}

// setupLocalGitRepo creates a bare-bones local git repo with one commit on
// `main` containing a README so the runner's git clone/checkout can exercise
// against a real filesystem URL (no network).
func setupLocalGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	if err := writeFile(dir+"/README.md", "hello from fixture\n"); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// fakeUploader records every Upload call and returns a canned
// outcome. The runner treats required vs. optional paths
// differently, so tests drive both lists via a single mock that
// can be configured to succeed on one call and fail on another.
type fakeUploader struct {
	mu    sync.Mutex
	calls [][]string // paths passed per call, in order
	// failWhen returns a non-nil error for a given call index to
	// simulate a partial failure; nil = success and caller gets
	// one ArtifactRef per input path.
	failWhen func(idx int) error
}

func (f *fakeUploader) Upload(
	_ context.Context,
	_ string,
	_ string,
	_ string,
	paths []string,
) ([]*gocdnextv1.ArtifactRef, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, append([]string(nil), paths...))
	fail := f.failWhen
	f.mu.Unlock()

	if fail != nil {
		if err := fail(idx); err != nil {
			return nil, err
		}
	}
	refs := make([]*gocdnextv1.ArtifactRef, 0, len(paths))
	for _, p := range paths {
		refs = append(refs, &gocdnextv1.ArtifactRef{
			Path: p, Size: 1, ContentSha256: "deadbeef",
		})
	}
	return refs, nil
}

func runnerWithUploader(t *testing.T, u runner.ArtifactUploader) (*runner.Runner, *collector) {
	t.Helper()
	c := &collector{}
	r := runner.New(runner.Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          c.Send,
		Uploader:      u,
	})
	return r, c
}

func TestExecute_RequiredArtifactUploadFailureFailsJob(t *testing.T) {
	up := &fakeUploader{failWhen: func(idx int) error {
		// First call = required upload; fail it.
		if idx == 0 {
			return fmt.Errorf("stat bin/gocdnext-agent: no such file")
		}
		return nil
	}}
	r, c := runnerWithUploader(t, up)
	a := assignment("echo ran")
	a.ArtifactPaths = []string{"bin/gocdnext-agent"}

	r.Execute(context.Background(), a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("want FAILED, got %+v", c.result)
	}
	if !strings.Contains(c.result.Error, "artifact upload failed") {
		t.Fatalf("error should mention upload: %q", c.result.Error)
	}
	if !strings.Contains(c.allLogText(), "artifact upload failed") {
		t.Fatalf("stderr should include upload failure:\n%s", c.allLogText())
	}
}

func TestExecute_OptionalArtifactUploadFailureDoesNotFailJob(t *testing.T) {
	// Two calls: required succeeds, optional fails.
	up := &fakeUploader{failWhen: func(idx int) error {
		if idx == 1 {
			return fmt.Errorf("coverage.xml not found")
		}
		return nil
	}}
	r, c := runnerWithUploader(t, up)
	a := assignment("echo ran")
	a.ArtifactPaths = []string{"bin/agent"}
	a.OptionalArtifactPaths = []string{"coverage.xml"}

	r.Execute(context.Background(), a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("want SUCCESS (optional failure), got %+v", c.result)
	}
	// Required ref should be reported; optional ref should not.
	if len(c.result.Artifacts) != 1 || c.result.Artifacts[0].Path != "bin/agent" {
		t.Fatalf("artifacts = %+v, want only the required one", c.result.Artifacts)
	}
	logs := c.allLogText()
	if !strings.Contains(logs, "optional artifact upload failed") {
		t.Fatalf("expected optional-failure log line:\n%s", logs)
	}
}

func TestExecute_BothArtifactListsUploadedOnSuccess(t *testing.T) {
	up := &fakeUploader{}
	r, c := runnerWithUploader(t, up)
	a := assignment("echo ran")
	a.ArtifactPaths = []string{"bin/agent"}
	a.OptionalArtifactPaths = []string{"coverage.xml", "screenshots/"}

	r.Execute(context.Background(), a)

	if c.result == nil || c.result.Status != gocdnextv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Fatalf("want SUCCESS, got %+v", c.result)
	}
	if len(up.calls) != 2 {
		t.Fatalf("uploader called %d times, want 2 (required + optional)", len(up.calls))
	}
	if len(c.result.Artifacts) != 3 {
		t.Fatalf("artifacts count = %d, want 3", len(c.result.Artifacts))
	}
}
