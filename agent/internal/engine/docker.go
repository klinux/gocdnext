package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Docker runs each script inside a `docker run ...` against the
// agent host's own Docker daemon. Use case: dev/local and small
// team CI where the agent box has Docker installed and each job's
// declared `image:` should actually be honoured (Shell engine
// ignores it). Jobs without an image fall back to the Shell
// engine — keeps existing YAML working during rollout.
//
// When ScriptSpec.Docker is true the workspace container also gets
// /var/run/docker.sock mounted and `DOCKER_HOST` set, so anything
// the script spawns (testcontainers, docker compose, buildx) talks
// to the host daemon as siblings. The tradeoff — the container
// effectively has root on the host via the Docker API — is
// acceptable for trusted internal CI and matches the
// GitLab-runner-shell / Woodpecker docker-socket pattern.
type Docker struct {
	cfg      DockerConfig
	fallback Engine
}

type DockerConfig struct {
	// SocketPath is the host path for the Docker API socket, used
	// both to validate availability and as the source for the
	// bind-mount when Spec.Docker is true. Empty → the Linux
	// default (/var/run/docker.sock). Override on macOS/Windows
	// daemons that surface sockets in non-standard locations.
	SocketPath string
	// DefaultImage is what the engine falls back to when a job
	// omits `image:` and no Shell fallback is configured. Empty
	// means "use the Shell engine for image-less jobs".
	DefaultImage string
	// ExtraDockerArgs are appended to every `docker run` invocation
	// — hook for operators that need `--network host`, custom
	// `--user`, or always-on `--init`.
	ExtraDockerArgs []string
	// PullPolicy maps to `docker run --pull=`. Empty leaves Docker
	// at its default ("missing" on modern versions). Valid values:
	// "always", "missing", "never".
	PullPolicy string
}

// NewDocker constructs the engine. Fallback is used for jobs that
// don't declare an image AND DockerConfig.DefaultImage is empty —
// honours the "mixed" case where most pipelines use images but a
// few legacy ones still run as raw shell scripts on the agent
// host. Pass nil fallback to refuse image-less jobs entirely
// (fails loudly instead of silently running on the host).
func NewDocker(cfg DockerConfig, fallback Engine) *Docker {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultDockerSocketPath
	}
	return &Docker{cfg: cfg, fallback: fallback}
}

// Name returns a stable identifier used in logs and metric labels.
func (*Docker) Name() string { return "docker" }

// RunScript dispatches to `docker run` with the workspace
// bind-mounted. Returns (N, nil) for script exit codes, (-1, err)
// for docker lifecycle problems (image pull failed, socket
// missing, etc.). On image-less jobs delegates to the fallback
// engine.
//
// Cancellation: `docker run` does NOT forward SIGTERM to the
// container by default — killing the CLI process (what
// exec.CommandContext does on ctx.Done) leaves the container
// running. We work around that by passing `--cidfile <path>`
// and spawning a watchdog goroutine: when ctx is canceled, read
// the cid and issue `docker kill <cid>`. The CLI process is
// still killed as a belt-and-suspenders step so a hung docker
// CLI doesn't pin the runner goroutine.
func (d *Docker) RunScript(ctx context.Context, spec ScriptSpec) (int, error) {
	image := spec.Image
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		if d.fallback != nil {
			return d.fallback.RunScript(ctx, spec)
		}
		return -1, errors.New("docker engine: job has no image and no DefaultImage configured")
	}

	if spec.Docker {
		if _, err := os.Stat(d.cfg.SocketPath); err != nil {
			return -1, fmt.Errorf(
				"docker engine: docker: true requested but %s is not reachable: %w",
				d.cfg.SocketPath, err)
		}
	}

	// cidfile lets us recover the container id for a forced kill
	// even if the CLI process hasn't logged it. `docker run` creates
	// the file atomically right before the container starts; we
	// remove it on exit.
	cidFile := filepath.Join(os.TempDir(), "gocdnext-"+uuid.NewString()+".cid")
	defer func() { _ = os.Remove(cidFile) }()

	args := d.buildArgs(image, spec, cidFile)
	// Intentionally NOT exec.CommandContext: we manage cancellation
	// manually so we can `docker kill` the container before the CLI
	// process dies, not after.
	cmd := exec.Command("docker", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("docker engine: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("docker engine: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("docker engine: start: %w", err)
	}

	// Watchdog: if ctx is canceled while the container is running,
	// read the cid and kill the container. Also kill the CLI in case
	// it's wedged (e.g. waiting on image pull) — otherwise ctx cancel
	// would hang waiting for Wait() to return.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cid := readCIDFile(cidFile); cid != "" {
				// best-effort; container may already have exited
				_ = exec.Command("docker", "kill", cid).Run()
			}
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(stdout, "stdout", spec.OnLine, &wg)
	go streamLines(stderr, "stderr", spec.OnLine, &wg)
	wg.Wait()

	waitErr := cmd.Wait()
	close(done)

	// Ctx already canceled means the caller asked for a cancel —
	// surface that explicitly so the runner reports "canceled"
	// instead of whatever docker's exit code happened to be
	// (typically 137/SIGKILL).
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("docker engine: wait: %w", waitErr)
	}
	return 0, nil
}

// readCIDFile returns the container id docker wrote into the
// cidfile, or "" when the file never appeared (container didn't
// start yet, e.g. ctx canceled during image pull). Trims the
// trailing newline docker writes.
func readCIDFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (d *Docker) buildArgs(image string, spec ScriptSpec, cidFile string) []string {
	args := []string{"run", "--rm", "-i"}

	if cidFile != "" {
		// Used by the cancel watchdog to find the container id and
		// issue `docker kill` when ctx is canceled mid-run. Docker
		// writes the file atomically right before starting the
		// container; refuses to overwrite an existing one, so we
		// pass a fresh tmp path per invocation.
		args = append(args, "--cidfile", cidFile)
	}

	if spec.WorkDir != "" {
		// Bind the host workspace so the container sees the same
		// checkout + artifact layout as the agent host. Right-hand
		// side is fixed at /workspace so paths in the script stay
		// portable across engines — a future K8s engine uses the
		// same mount point via a PVC/emptyDir.
		args = append(args, "-v", spec.WorkDir+":/workspace")
		args = append(args, "-w", "/workspace")
	}

	for _, kv := range envPairsSorted(spec.Env) {
		args = append(args, "-e", kv)
	}

	if spec.Network != "" {
		// Join the job-scoped docker network the runner provisioned
		// for this assignment. Service containers sit on the same
		// network with a DNS alias matching their declared name, so
		// a script saying `psql -h postgres` resolves to the sidecar
		// without any extra env.
		args = append(args, "--network", spec.Network)
	}

	if spec.Docker {
		// Base plumbing: mount the host socket at a stable path and
		// point DOCKER_HOST at it so docker CLI, docker-go, and any
		// library that reads DOCKER_HOST "just works".
		args = append(args, "-v", d.cfg.SocketPath+":/var/run/docker.sock")
		args = append(args, "-e", "DOCKER_HOST=unix:///var/run/docker.sock")

		// Testcontainers-go hints. It has its own detection ladder
		// (env → docker context → rootless paths) which fails on
		// Linux with plain DOCKER_HOST in surprisingly many cases —
		// the overrides make it deterministic. Harmless when the
		// job isn't using testcontainers (the library simply
		// ignores the vars).
		args = append(args, "-e", "TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock")
		args = append(args, "-e", "TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal")

		// Point host.docker.internal at the host's gateway so the
		// test container can reach siblings testcontainers spawns.
		// Docker 20.10+ resolves the `host-gateway` magic value to
		// the bridge's host IP. Docker Desktop does this
		// automatically; on raw Linux we need the explicit flag.
		args = append(args, "--add-host", "host.docker.internal:host-gateway")
	}

	if d.cfg.PullPolicy != "" {
		args = append(args, "--pull="+d.cfg.PullPolicy)
	}

	if len(d.cfg.ExtraDockerArgs) > 0 {
		args = append(args, d.cfg.ExtraDockerArgs...)
	}

	if spec.Script == "" {
		// Plugin-style task: the image's own ENTRYPOINT is the
		// logic, no shell wrapper. Skip the gitSafe preamble and
		// `sh -c` entirely — `docker run <image>` with no extra
		// cmd lets the image run its declared entrypoint /
		// default cmd. PLUGIN_* env vars are already in `-e …`.
		args = append(args, image)
		return args
	}

	// Git 2.35+ refuses to operate on a repo whose filesystem
	// owner differs from the process UID — the workspace was
	// cloned by the agent (some UID X) and the container runs
	// as root (UID 0), so any `git` call inside the job blows
	// up with "dubious ownership" and takes down `go build`'s
	// VCS stamping + npm postinstall hooks + anything else that
	// reads git metadata. Pre-seeding safe.directory='*' in the
	// container's global gitconfig is a silent no-op on images
	// without git and a blanket fix everywhere else — saves us
	// from making every pipeline remember a workaround line.
	const gitSafe = `git config --global --add safe.directory '*' 2>/dev/null || true; `
	args = append(args, image, "sh", "-c", gitSafe+spec.Script)
	return args
}

// envPairsSorted flattens an env map into KEY=VAL strings in a
// deterministic order so two consecutive runs with the same map
// hit Docker with byte-identical args. Helps debug repros + log
// diffs; Docker itself doesn't care about order.
func envPairsSorted(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Small set (<50 typical), sort.Strings overkill — a linear
	// insert-sort beats allocation of a sort.Interface wrapper and
	// the order only matters for argv stability.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && strings.Compare(out[j-1], out[j]) > 0; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
