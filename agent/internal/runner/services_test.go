package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// stubEngine records the EnsureServices invocation and returns a
// caller-controlled ServicesWireup. Used to drive startServices'
// dispatch without dragging k8s/docker into a unit test.
type stubEngine struct {
	name           string
	ensureCalls    int
	ensureSawSpecs []engine.ServiceSpec
	ensureSawJobID string
	wireup         engine.ServicesWireup
	wireupErr      error
}

func (s *stubEngine) Name() string { return s.name }

func (s *stubEngine) RunScript(context.Context, engine.ScriptSpec) (int, error) {
	return 0, nil
}

func (s *stubEngine) EnsureServices(
	_ context.Context,
	services []engine.ServiceSpec,
	jobID string,
	_ func(string, string),
) (engine.ServicesWireup, error) {
	s.ensureCalls++
	s.ensureSawSpecs = append([]engine.ServiceSpec(nil), services...)
	s.ensureSawJobID = jobID
	if s.wireupErr != nil {
		// Mirror the real-engine contract: even on error, callers
		// must be able to safely invoke Cleanup.
		if s.wireup.Cleanup == nil {
			s.wireup.Cleanup = func() {}
		}
		return s.wireup, s.wireupErr
	}
	if s.wireup.Cleanup == nil {
		s.wireup.Cleanup = func() {}
	}
	return s.wireup, nil
}

// TestStartServices_PropagatesEngineError checks that an error from
// EnsureServices reaches the runner unchanged — the runner doesn't
// rewrap with a generic message because the engine already framed it.
func TestStartServices_PropagatesEngineError(t *testing.T) {
	wantErr := errors.New("shell engine: 1 service(s) declared but the shell engine has no isolation layer")
	stub := &stubEngine{name: "shell", wireupErr: wantErr}
	r := New(Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          func(*gocdnextv1.AgentMessage) {},
		Engine:        stub,
	})
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "j-1",
		Services: []*gocdnextv1.ServiceSpec{
			{Name: "postgres", Image: "postgres:16"},
		},
	}
	var seq atomic.Int64
	phase, err := r.startServices(context.Background(), a, &seq)
	if err == nil {
		t.Fatal("expected error from stub engine")
	}
	if !strings.Contains(err.Error(), "shell engine") {
		t.Errorf("error lost engine framing: %v", err)
	}
	// The returned phase must still have a noop cleanup so a deferred
	// call from the runner doesn't panic on error paths.
	if phase.cleanup == nil {
		t.Fatal("phase.cleanup is nil on error path")
	}
	phase.cleanup()
	if stub.ensureCalls != 1 {
		t.Errorf("EnsureServices invoked %d times, want 1", stub.ensureCalls)
	}
}

// TestStartServices_NoopWhenNoServices guards the happy path: a
// pipeline without services on ANY engine must return cleanly so
// the runner moves on to the task phase. EnsureServices MUST NOT be
// called — there's nothing for the engine to do, and avoiding the
// call keeps shell/other engines from rejecting an empty list.
func TestStartServices_NoopWhenNoServices(t *testing.T) {
	stub := &stubEngine{name: "kubernetes"}
	r := New(Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          func(*gocdnextv1.AgentMessage) {},
		Engine:        stub,
	})
	a := &gocdnextv1.JobAssignment{RunId: "r", JobId: "j"}
	var seq atomic.Int64
	phase, err := r.startServices(context.Background(), a, &seq)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if phase.network != "" {
		t.Errorf("network = %q, want empty", phase.network)
	}
	if len(phase.hostAliases) != 0 {
		t.Errorf("hostAliases = %v, want empty", phase.hostAliases)
	}
	if phase.cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	if stub.ensureCalls != 0 {
		t.Errorf("EnsureServices invoked %d times on no-services path, want 0", stub.ensureCalls)
	}
	// Calling the noop cleanup must not panic.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); phase.cleanup() }()
	wg.Wait()
}

// TestStartServices_PassesJobIDAndSpecs locks in the wire-up: specs
// are converted to engine.ServiceSpec and the JobAssignment's job_id
// is threaded through so the engine can build collision-free
// resource names per job.
func TestStartServices_PassesJobIDAndSpecs(t *testing.T) {
	stub := &stubEngine{
		name: "kubernetes",
		wireup: engine.ServicesWireup{
			HostAliases: []engine.HostAlias{
				{IP: "10.0.0.1", Hostnames: []string{"postgres"}},
			},
		},
	}
	r := New(Config{
		WorkspaceRoot: t.TempDir(),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Send:          func(*gocdnextv1.AgentMessage) {},
		Engine:        stub,
	})
	a := &gocdnextv1.JobAssignment{
		RunId: "r", JobId: "job-abc-123",
		Services: []*gocdnextv1.ServiceSpec{
			{Name: "postgres", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "x"}},
		},
	}
	var seq atomic.Int64
	phase, err := r.startServices(context.Background(), a, &seq)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if stub.ensureSawJobID != "job-abc-123" {
		t.Errorf("EnsureServices saw jobID=%q, want job-abc-123", stub.ensureSawJobID)
	}
	if len(stub.ensureSawSpecs) != 1 || stub.ensureSawSpecs[0].Name != "postgres" {
		t.Errorf("EnsureServices saw specs=%+v, want one postgres", stub.ensureSawSpecs)
	}
	if len(phase.hostAliases) != 1 || phase.hostAliases[0].IP != "10.0.0.1" {
		t.Errorf("phase.hostAliases = %v, want one 10.0.0.1 alias", phase.hostAliases)
	}
}
