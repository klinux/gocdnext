package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// dockerServiceNameRE is the strict charset for a pipeline service
// name reaching `docker run --name` / `docker network create`. We
// intentionally pin it tighter than docker's own grammar (which
// allows `.`) because the value also becomes a DNS alias on the
// network and gets concatenated into a hostname — anything that's
// not [a-z0-9-] risks "Error response from daemon: Invalid network
// name" or "Invalid container name" at run time.
//
// Critical: this is part of the substitution-time security perimeter.
// The service name + image come from pipeline YAML that may live in
// a public PR branch; without this check, a malicious `name: "--rm"`
// or `name: "$(touch /tmp/x)"` would land verbatim in argv. exec.Cmd
// doesn't run a shell, so we're safe from `$( )`-style command
// substitution, but argv injection — `name: "x --network host"`
// breaking out of the --name slot — is still possible without a
// strict charset gate.
var dockerServiceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// EnsureServices brings up each declared service as a standalone
// container on a job-scoped docker network. The task container joins
// the same network later via ScriptSpec.Network (the engine reads
// Wireup.Network from the runner and threads it into `docker run`).
// Service names become DNS aliases on the network, so the script
// reaches `postgres:5432` without any host-aliases plumbing.
//
// Cleanup tears containers down first (they hold references to the
// network) then the network. It's safe to call on partial startup —
// `docker network rm` of an in-use network is a no-op error we
// swallow, and we only collect ids for containers that actually
// reported `docker run` success.
//
// runID is accepted to satisfy the Engine interface but currently
// IGNORED here: docker engine is typically single-host and the
// existing per-job scoping is fine. If a future deployment hits the
// per-job-network cost the k8s engine fix (run-scoped naming + reuse)
// can be ported here too — TODO when there's a real consumer.
// CleanupRunServices is a no-op for the docker engine today —
// docker services are still per-job (see EnsureServices doc) so
// there's no run-scoped state to tear down. Future work that
// ports the k8s run-scoped model to docker would implement this
// by listing+removing containers labelled with the runID; for now
// returning (0, nil) keeps the Engine contract satisfied without
// pretending we did anything.
func (d *Docker) CleanupRunServices(_ context.Context, _ string, _ func(ServiceLifecycleEvent)) (int, error) {
	return 0, nil
}

func (d *Docker) EnsureServices(ctx context.Context, services []ServiceSpec, runID, jobID string, log func(stream, text string), onLifecycle func(ServiceLifecycleEvent)) (ServicesWireup, error) {
	_ = onLifecycle // docker stays per-job; lifecycle tracking ports along with the run-scoped rewrite
	_ = runID // see docstring; docker engine stays per-job for now
	noop := ServicesWireup{Cleanup: func() {}}
	if len(services) == 0 {
		return noop, nil
	}
	if jobID == "" {
		return noop, errors.New("docker engine: EnsureServices needs a non-empty jobID for collision-free naming")
	}

	jobShort := shortDockerID(jobID)
	network := "gocdnext-" + jobShort

	if out, err := exec.CommandContext(ctx, "docker", "network", "create", network).CombinedOutput(); err != nil {
		return noop, fmt.Errorf("docker engine: create network %s: %w (docker said: %s)",
			network, err, strings.TrimSpace(string(out)))
	}

	var started []string
	cleanup := func() {
		for _, cid := range started {
			_ = exec.Command("docker", "rm", "-f", cid).Run()
		}
		_ = exec.Command("docker", "network", "rm", network).Run()
	}

	for _, svc := range services {
		if !dockerServiceNameRE.MatchString(svc.Name) {
			cleanup()
			return noop, fmt.Errorf(
				"docker engine: service name %q is invalid — expected lowercase alphanumerics and dashes only, starting with a letter, max 63 chars",
				svc.Name)
		}
		if svc.Image == "" {
			cleanup()
			return noop, fmt.Errorf("docker engine: service %q has empty image", svc.Name)
		}
		container := "gocdnext-" + jobShort + "-" + svc.Name
		args := []string{
			"run", "-d", "--rm",
			"--name", container,
			"--network", network,
			"--network-alias", svc.Name,
		}
		// Reference env values by NAME on argv (`-e KEY`) and
		// propagate the actual values via cmd.Env. Service env
		// often carries credentials (POSTGRES_PASSWORD, etc.); the
		// previous `-e KEY=VAL` form leaked them to `ps auxww`.
		for _, key := range envKeysSorted(svc.Env) {
			args = append(args, "-e", key)
		}
		args = append(args, svc.Image)
		args = append(args, svc.Command...)

		if log != nil {
			log("stdout", fmt.Sprintf("$ starting service %s (%s)", svc.Name, svc.Image))
		}
		svcCmd := exec.CommandContext(ctx, "docker", args...)
		svcCmd.Env = append(os.Environ(), envPairsForCmd(svc.Env)...)
		out, err := svcCmd.CombinedOutput()
		if err != nil {
			cleanup()
			return noop, fmt.Errorf("docker engine: start service %s: %w (docker said: %s)",
				svc.Name, err, strings.TrimSpace(string(out)))
		}
		started = append(started, container)
	}

	return ServicesWireup{Network: network, Cleanup: cleanup}, nil
}

// shortDockerID trims a UUID-ish id down to a dns-safe prefix.
// Docker network + container names get an overall length cap (63
// chars for DNS labels); 12 chars of hex is plenty of entropy for
// per-job scoping on a single agent.
func shortDockerID(id string) string {
	clean := strings.ReplaceAll(id, "-", "")
	if len(clean) > 12 {
		clean = clean[:12]
	}
	return clean
}

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
			// Fallback to Shell: don't translate GOCDNEXT_OUTPUT_FILE
			// here — the Shell engine sets it from
			// spec.OutputsHostPath, which is the right place for a
			// host-execution path. If we hardcoded /workspace here
			// the fallback would write to a path that doesn't exist
			// on the host (the bug Kleber caught).
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

	// Outputs (issue #10): containerized path — workspace is
	// bind-mounted to ContainerWorkspaceMount, so the script's
	// view of the output file lives there. Only set when the
	// runner asked for outputs; engines with no outputs request
	// pass the env through verbatim.
	if spec.OutputsHostPath != "" && spec.OutputsRelPath != "" {
		spec.Env = withOutputsEnv(spec.Env, filepath.Join(ContainerWorkspaceMount, spec.OutputsRelPath))
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
	// Container env values live in the agent's child-process env so
	// `docker run -e KEY` (name-only on argv) inherits them without
	// the values ever appearing in `ps auxww`. buildArgs sets the
	// argv references; envPairsForCmd populates the matching pairs
	// here. The DOCKER_HOST + TESTCONTAINERS_* literals buildArgs
	// emits for `docker: true` aren't secrets — they ride on argv
	// as `-e KEY=VAL` directly and don't need the propagation path.
	cmd.Env = append(os.Environ(), envPairsForCmd(spec.Env)...)

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

	// Pass env values via the agent's child-process env (cmd.Env in
	// RunScript) and reference them on the docker argv by NAME ONLY
	// (`-e KEY`, no `=VAL`). docker propagates the value from its
	// own env into the container. This keeps secret-bearing values
	// (PEM-encoded keys, registry tokens, generic `secrets:` env)
	// off `ps auxww` — the previous `-e KEY=VAL` form put them in
	// the docker CLI's argv where any process on the host could
	// read them. Multi-line values (PEM) round-trip cleanly because
	// env vars don't have the line-based parsing limits of
	// docker's `--env-file`.
	//
	// envKeysSorted gives deterministic argv ordering; the actual
	// value-population happens in RunScript via cmd.Env.
	for _, key := range envKeysSorted(spec.Env) {
		args = append(args, "-e", key)
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
	// `--` after -c stops sh option parsing so a user script literal
	// starting with `-` runs as the command instead of being parsed
	// as a flag. gitSafe always prefixes a `git config …; ` so the
	// command string here never actually starts with `-`, but adding
	// `--` keeps the engines consistent with kubernetes + shell.
	args = append(args, image, "sh", "-c", "--", gitSafe+spec.Script)
	return args
}

// envKeysSorted returns env variable NAMES sorted lexicographically.
// Used to build `-e KEY` (name-only) argv entries — the value lives
// in the docker CLI's own process env via RunScript's cmd.Env so
// secret-bearing values never touch the host's process list.
// Insert-sort is fine for the small (<50) typical set.
func envKeysSorted(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && strings.Compare(out[j-1], out[j]) > 0; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// envPairsForCmd flattens an env map into KEY=VAL strings the
// same way os/exec expects in cmd.Env. Sorted in the same order
// envKeysSorted produces so argv layout and process-env layout
// align across runs. Used by RunScript to set the docker CLI's
// own process env; the container then inherits each value via
// `docker run -e KEY` references.
func envPairsForCmd(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, k := range envKeysSorted(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}
