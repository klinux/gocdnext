package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

type captured struct {
	mu    sync.Mutex
	lines []string
}

func (c *captured) onLine(stream, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, stream+": "+text)
}

func (c *captured) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

func TestShell_RunScript_CapturesStdout(t *testing.T) {
	cap := &captured{}
	exit, err := engine.NewShell().RunScript(context.Background(), engine.ScriptSpec{
		Script: `echo hello; echo world`,
		OnLine: cap.onLine,
	})
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	got := strings.Join(cap.snapshot(), ",")
	if !strings.Contains(got, "stdout: hello") || !strings.Contains(got, "stdout: world") {
		t.Errorf("output = %v", cap.snapshot())
	}
}

func TestShell_RunScript_ExitCode(t *testing.T) {
	exit, err := engine.NewShell().RunScript(context.Background(), engine.ScriptSpec{
		Script: `exit 7`,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if exit != 7 {
		t.Errorf("exit = %d, want 7", exit)
	}
}

func TestShell_RunScript_CapturesStderr(t *testing.T) {
	cap := &captured{}
	_, _ = engine.NewShell().RunScript(context.Background(), engine.ScriptSpec{
		Script: `echo oops >&2`,
		OnLine: cap.onLine,
	})
	got := strings.Join(cap.snapshot(), ",")
	if !strings.Contains(got, "stderr: oops") {
		t.Errorf("stderr not captured: %v", cap.snapshot())
	}
}

func TestShell_RunScript_PassesEnvAndWorkDir(t *testing.T) {
	cap := &captured{}
	dir := t.TempDir()
	exit, err := engine.NewShell().RunScript(context.Background(), engine.ScriptSpec{
		WorkDir: dir,
		Env:     map[string]string{"FOO": "bar"},
		Script:  `echo "here=$(pwd); foo=$FOO"`,
		OnLine:  cap.onLine,
	})
	if err != nil || exit != 0 {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	out := strings.Join(cap.snapshot(), "\n")
	if !strings.Contains(out, "foo=bar") {
		t.Errorf("env not propagated: %s", out)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("workdir not applied: %s (want contains %s)", out, dir)
	}
}

func TestShell_RunScript_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	exit, err := engine.NewShell().RunScript(ctx, engine.ScriptSpec{
		Script: `sleep 5`,
	})
	// SIGKILL from ctx cancel shows up as exit < 0 OR err != nil
	// depending on how the child reacts; either way, we must not
	// wait the full 5s.
	if err == nil && exit == 0 {
		t.Errorf("ctx cancel should have failed the process (exit=%d err=%v)", exit, err)
	}
}

func TestShell_Name(t *testing.T) {
	if n := engine.NewShell().Name(); n != "shell" {
		t.Errorf("Name = %q, want shell", n)
	}
}
