package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// dockerAvailable tells us whether the test host can exercise the
// Docker engine end-to-end. When docker CLI or daemon is missing
// we skip the integration cases — still run the arg-building unit
// checks that don't actually invoke docker.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return false
	}
	return true
}

func TestDockerEngine_FallsBackToShellWhenImageMissing(t *testing.T) {
	// Image-less jobs should hit the fallback engine (Shell in
	// production, a trivial stub here). No docker CLI needed to
	// run this test — the engine returns before ever invoking
	// `docker`.
	called := false
	stub := stubEngine(func(_ context.Context, _ engine.ScriptSpec) (int, error) {
		called = true
		return 0, nil
	})
	d := engine.NewDocker(engine.DockerConfig{}, stub)
	code, err := d.RunScript(context.Background(), engine.ScriptSpec{Script: "true"})
	if err != nil {
		t.Fatalf("fallback path: %v", err)
	}
	if code != 0 || !called {
		t.Fatalf("fallback wasn't invoked (code=%d, called=%v)", code, called)
	}
}

func TestDockerEngine_StrictRefusesImagelessJobs(t *testing.T) {
	// No fallback + no DefaultImage = the engine should error
	// instead of silently succeeding or running bare shell.
	d := engine.NewDocker(engine.DockerConfig{}, nil)
	_, err := d.RunScript(context.Background(), engine.ScriptSpec{Script: "true"})
	if err == nil {
		t.Fatalf("expected error for image-less strict mode")
	}
	if !strings.Contains(err.Error(), "no image") {
		t.Fatalf("error should mention missing image: %q", err.Error())
	}
}

func TestDockerEngine_FailsFastWhenSocketMissing(t *testing.T) {
	// docker:true + socket path that doesn't exist = fail with a
	// clear error before any container is pulled. Uses a bogus
	// socket path so the test works even on hosts with docker.
	tmp := t.TempDir()
	d := engine.NewDocker(engine.DockerConfig{
		SocketPath: filepath.Join(tmp, "does-not-exist.sock"),
	}, nil)
	_, err := d.RunScript(context.Background(), engine.ScriptSpec{
		Image:  "alpine:3.19",
		Script: "true",
		Docker: true,
	})
	if err == nil {
		t.Fatal("expected error for missing docker socket")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error should mention unreachable socket: %q", err.Error())
	}
}

func TestDockerEngine_RunsScriptInContainer(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker daemon not available")
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "hello.txt"), []byte("hi from host\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	d := engine.NewDocker(engine.DockerConfig{PullPolicy: "missing"}, nil)
	cap := &captured{}
	code, err := d.RunScript(context.Background(), engine.ScriptSpec{
		WorkDir: tmp,
		Image:   "alpine:3.19",
		Script:  `echo "from container"; cat hello.txt`,
		OnLine:  cap.onLine,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — logs:\n%s", code, strings.Join(cap.snapshot(), "\n"))
	}
	lines := strings.Join(cap.snapshot(), "\n")
	if !strings.Contains(lines, "from container") {
		t.Fatalf("missing echo output:\n%s", lines)
	}
	if !strings.Contains(lines, "hi from host") {
		t.Fatalf("workspace not mounted:\n%s", lines)
	}
}

func TestDockerEngine_JoinsProvisionedNetwork(t *testing.T) {
	// When the runner creates a job-scoped docker network and
	// passes its name through ScriptSpec.Network, the task
	// container must join that network — otherwise pipeline
	// services (on that same network with DNS aliases) are
	// unreachable from scripts. Full end-to-end: create a
	// network, stand up a tiny echo service with an alias, run
	// a script that resolves it, verify the output.
	if !dockerAvailable(t) {
		t.Skip("docker daemon not available")
	}
	netName := "gocdnext-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if out, err := exec.Command("docker", "network", "create", netName).CombinedOutput(); err != nil {
		t.Fatalf("create network: %v (%s)", err, out)
	}
	defer exec.Command("docker", "network", "rm", netName).Run()

	// Service: alpine container sitting idle on the network with
	// a "svc" alias. Scripts in the sibling container resolve
	// "svc" by DNS when they share the network.
	svcName := "gocdnext-test-svc-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if out, err := exec.Command("docker", "run", "-d", "--rm",
		"--name", svcName,
		"--network", netName,
		"--network-alias", "svc",
		"alpine:3.19", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("start svc: %v (%s)", err, out)
	}
	defer exec.Command("docker", "rm", "-f", svcName).Run()

	d := engine.NewDocker(engine.DockerConfig{PullPolicy: "missing"}, nil)
	cap := &captured{}
	code, err := d.RunScript(context.Background(), engine.ScriptSpec{
		WorkDir: t.TempDir(),
		Image:   "alpine:3.19",
		// getent returns the alias's resolved IP; a failure here
		// would mean the task container wasn't on the network.
		Script:  `getent hosts svc`,
		Network: netName,
		OnLine:  cap.onLine,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — logs:\n%s", code, strings.Join(cap.snapshot(), "\n"))
	}
	if !strings.Contains(strings.Join(cap.snapshot(), "\n"), "svc") {
		t.Fatalf("svc hostname didn't resolve — network not attached. logs:\n%s",
			strings.Join(cap.snapshot(), "\n"))
	}
}

func TestDockerEngine_PropagatesExitCode(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("docker daemon not available")
	}
	d := engine.NewDocker(engine.DockerConfig{PullPolicy: "missing"}, nil)
	code, err := d.RunScript(context.Background(), engine.ScriptSpec{
		WorkDir: t.TempDir(),
		Image:   "alpine:3.19",
		Script:  "exit 42",
	})
	if err != nil {
		t.Fatalf("unexpected lifecycle err: %v", err)
	}
	if code != 42 {
		t.Fatalf("exit = %d, want 42", code)
	}
}

// stubEngine is a minimal Engine that just calls the provided fn
// — used to verify routing logic without touching the shell or
// docker runtimes.
type stubEngineFn func(context.Context, engine.ScriptSpec) (int, error)

func (f stubEngineFn) Name() string { return "stub" }
func (f stubEngineFn) RunScript(ctx context.Context, spec engine.ScriptSpec) (int, error) {
	return f(ctx, spec)
}

func stubEngine(fn stubEngineFn) engine.Engine {
	return fn
}

// Compile-time assertion that our stub actually satisfies the
// interface — catches interface drift without running the test.
var _ engine.Engine = stubEngine(nil)

// Silence "imported and not used" if the env-sort test below is
// disabled behind a tag — kept close to usage so churn doesn't
// leave orphans.
var _ = errors.New
var _ = fmt.Sprintf
