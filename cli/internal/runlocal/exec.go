package runlocal

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// docker shells out to the docker CLI instead of linking the SDK:
// the CLI is what every developer with "requires Docker" already
// has, it speaks every daemon version, and run-local's needs are
// four verbs (network create/rm, run, wait-by-running).
type docker struct {
	bin string
	out io.Writer
	mu  sync.Mutex // serialises prefixed log writes
}

func newDocker(out io.Writer) *docker {
	return &docker{bin: "docker", out: out}
}

func (d *docker) check(ctx context.Context) error {
	if err := exec.CommandContext(ctx, d.bin, "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return fmt.Errorf("docker daemon unreachable — run-local requires Docker: %w", err)
	}
	return nil
}

func (d *docker) networkCreate(ctx context.Context, name string) error {
	if out, err := exec.CommandContext(ctx, d.bin, "network", "create", name).CombinedOutput(); err != nil {
		return fmt.Errorf("network create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (d *docker) networkRemove(name string) {
	_ = exec.Command(d.bin, "network", "rm", name).Run()
}

// startService launches a sidecar on the run network with its name
// as DNS alias — jobs reach it exactly like in the cluster. Returns
// the container id for teardown. Env rides the same name-only argv
// pattern as jobs (see envArgs) for consistency.
func (d *docker) startService(ctx context.Context, network string, svc domain.Service) (string, error) {
	args := []string{"run", "-d", "--rm",
		"--network", network,
		"--network-alias", svc.Name,
		"--name", network + "-" + svc.Name,
	}
	keys, kv := envArgs(svc.Env)
	for _, k := range keys {
		args = append(args, "-e", k)
	}
	args = append(args, svc.Image)
	args = append(args, svc.Command...)
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Env = append(os.Environ(), kv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("service %s: %s: %w", svc.Name, strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// envArgs splits an env map into sorted NAMES (for `-e KEY` argv
// references) and KEY=VALUE pairs (for the docker CLI's own process
// env). Mirrors the agent's docker engine: values NEVER touch argv
// — `-e KEY=VAL` would put resolved secrets on /proc/*/cmdline for
// the lifetime of the docker run (review-round HIGH); with `-e KEY`
// docker propagates the value from its own environment.
func envArgs(env map[string]string) (names []string, kv []string) {
	names = make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	kv = make([]string, 0, len(env))
	for _, k := range names {
		kv = append(kv, k+"="+env[k])
	}
	return names, kv
}

func (d *docker) stopContainer(id string) {
	_ = exec.Command(d.bin, "stop", "-t", "2", id).Run()
}

// jobRunArgs builds the docker-run argv for one job. Pure — split
// from runJob so a test can assert no env VALUE ever reaches argv.
func jobRunArgs(j PlannedJob, workspace, network string, envNames []string) []string {
	args := []string{"run", "--rm",
		"-v", workspace + ":/workspace",
		"-w", "/workspace",
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if j.Docker {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	for _, k := range envNames {
		args = append(args, "-e", k)
	}
	if len(j.Script) > 0 {
		// One container per job, every task script in order, set -e
		// semantics per line block — mirrors the agent's task loop
		// closely enough for local iteration.
		script := strings.Join(j.Script, "\n")
		args = append(args, "--entrypoint", "/bin/sh", j.Image, "-ec", script)
	} else {
		// Plugin job: image entrypoint + PLUGIN_* env (named above).
		args = append(args, j.Image)
	}
	return args
}

// runJob executes one planned job as a container and streams its
// output with a job-name prefix. Returns the container exit code.
func (d *docker) runJob(ctx context.Context, j PlannedJob, workspace, network string, env map[string]string) (int, error) {
	names, kv := envArgs(env)
	args := jobRunArgs(j, workspace, network, names)

	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Env = append(os.Environ(), kv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	cmd.Stderr = cmd.Stdout // interleave, same as job logs
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	d.stream(j.Name, stdout)
	err = cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

func (d *docker) stream(prefix string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		d.mu.Lock()
		fmt.Fprintf(d.out, "[%s] %s\n", prefix, sc.Text())
		d.mu.Unlock()
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
