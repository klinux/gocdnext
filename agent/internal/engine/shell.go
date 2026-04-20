package engine

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Shell is the dev/local runtime: `sh -c $script` against the
// agent's own filesystem. Image is ignored — the script executes
// with whatever tools the agent host has installed. Env layers on
// top of the agent's own os.Environ so PATH etc. pass through.
type Shell struct{}

// NewShell returns the ready-to-use value. No configuration needed
// today; kept as a constructor so future knobs (cwd overrides,
// PATH isolation) slot in without breaking callers.
func NewShell() *Shell { return &Shell{} }

// Name returns a stable identifier for logging/metrics.
func (*Shell) Name() string { return "shell" }

// RunScript shells out via `sh -c`. See Engine.RunScript for the
// error contract — exit != 0 is returned as (N, nil); fork or pipe
// failure is returned as (-1, err).
func (*Shell) RunScript(ctx context.Context, spec ScriptSpec) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", spec.Script)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(spec.Env)...)
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
	wg.Add(2)
	go streamLines(stdout, "stdout", spec.OnLine, &wg)
	go streamLines(stderr, "stderr", spec.OnLine, &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// streamLines fans each line from the process's pipe into OnLine.
// Scanner errors (pipe close, etc.) are swallowed; the caller's
// cmd.Wait() is the authoritative "did it finish OK?" signal.
func streamLines(rd io.Reader, stream string, emit func(string, string), wg *sync.WaitGroup) {
	defer wg.Done()
	if emit == nil {
		_, _ = io.Copy(io.Discard, rd)
		return
	}
	scanner := bufio.NewScanner(rd)
	// Raise buffer: long `go test -v` lines or minified JS blow past
	// the default 64 KiB.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		emit(stream, scanner.Text())
	}
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
