package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// DefaultDockerSocketPath is where every docker engine (moby +
// rootful podman compat) puts its API socket on Linux. The Shell
// and Docker engines both probe this path when a job sets
// `docker: true`; override via DockerConfig.SocketPath only if the
// agent runs on a host with a non-default layout.
const DefaultDockerSocketPath = "/var/run/docker.sock"

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
	// `docker: true` on the Shell engine means "the script is about
	// to poke the host's docker daemon". Fail fast if the socket
	// isn't there so the user gets a clear error instead of a
	// misleading "docker: command not found" once the script runs.
	if spec.Docker {
		if _, err := os.Stat(DefaultDockerSocketPath); err != nil {
			return -1, fmt.Errorf(
				"shell engine: docker: true requested but %s is not reachable: %w",
				DefaultDockerSocketPath, err)
		}
	}
	if spec.Script == "" {
		// Plugin task: nothing we can usefully do on the host
		// shell. The plugin's logic lives in a container image
		// and we have no runtime to run it here. Report a clear
		// error instead of silently succeeding — the operator
		// declared a plugin step expecting something to happen.
		return -1, fmt.Errorf(
			"shell engine: plugin task (image=%q) cannot run — use the docker engine for plugins",
			spec.Image)
	}
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
