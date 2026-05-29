package runner

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// stubEngine names itself but no-ops everything else. Used to drive
// startServices' engine-preflight branch without dragging k8s or
// docker dependencies into the unit test.
type stubEngine struct{ name string }

func (s *stubEngine) Name() string { return s.name }

func (s *stubEngine) RunScript(context.Context, engine.ScriptSpec) (int, error) {
	return 0, nil
}

// TestStartServices_FailsLoudOnNonDockerEngine locks in the v0.4.20
// behaviour: when a pipeline declares services but the agent's
// engine isn't docker, return a clear error pointing at the engine
// + the path forward, BEFORE shelling out to `docker network
// create` (which fails with the cryptic "exit status 1" the
// operator can't act on).
func TestStartServices_FailsLoudOnNonDockerEngine(t *testing.T) {
	cases := []string{"kubernetes", "shell"}
	for _, engineName := range cases {
		t.Run(engineName, func(t *testing.T) {
			r := New(Config{
				WorkspaceRoot: t.TempDir(),
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
				Send:          func(*gocdnextv1.AgentMessage) {},
				Engine:        &stubEngine{name: engineName},
			})
			a := &gocdnextv1.JobAssignment{
				RunId: "r", JobId: "j",
				Services: []*gocdnextv1.ServiceSpec{
					{Name: "postgres", Image: "postgres:16"},
				},
			}
			var seq atomic.Int64
			_, _, err := r.startServices(context.Background(), a, &seq)
			if err == nil {
				t.Fatalf("expected error for engine=%s", engineName)
			}
			msg := err.Error()
			if !strings.Contains(msg, engineName) {
				t.Errorf("error should name the engine %q: %v", engineName, err)
			}
			if !strings.Contains(msg, "docker engine") {
				t.Errorf("error should recommend the docker engine path: %v", err)
			}
		})
	}
}

// TestStartServices_NoopWhenNoServices guards the happy path: a
// pipeline without services on ANY engine must return cleanly so
// the runner moves on to the task phase. The cleanup func returned
// here is a noop — calling it must not panic.
func TestStartServices_NoopWhenNoServices(t *testing.T) {
	r := New(Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          func(*gocdnextv1.AgentMessage) {},
		Engine:        &stubEngine{name: "kubernetes"},
	})
	a := &gocdnextv1.JobAssignment{RunId: "r", JobId: "j"}
	var seq atomic.Int64
	network, cleanup, err := r.startServices(context.Background(), a, &seq)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if network != "" {
		t.Errorf("network = %q, want empty", network)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	// Calling the noop cleanup must not panic.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cleanup() }()
	wg.Wait()
}
