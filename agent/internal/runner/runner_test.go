package runner_test

import (
	"context"
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

func TestExecute_GitCheckoutFromLocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := setupLocalGitRepo(t)

	r, c := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := assignment(`cat fixture/README.md`)
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
