// Package runner executes a JobAssignment end-to-end on the local host:
// clones the declared git materials, runs the shell scripts, streams the
// stdout/stderr lines back to the server as LogLine events, and finishes
// with a JobResult. Docker/plugin execution lands in a later slice.
package runner

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// Config wires the runner. Send is the single outbound callback; callers plug
// in a function that enqueues onto the gRPC send pump.
type Config struct {
	WorkspaceRoot string
	Logger        *slog.Logger
	Send          func(*gocdnextv1.AgentMessage)

	// Uploader handles artifact tar+upload when a job declares
	// `artifacts: [paths]`. Nil means "no-op" — the job still succeeds
	// but no refs are attached to JobResult.
	Uploader ArtifactUploader

	// IsolatedUploader is the isolated-mode counterpart of
	// Uploader: tars files from inside the job pod's housekeeper
	// sidecar via PodExecutor and PUTs to the signed URL.
	// Concrete impl is rpc.ArtifactUploader (same struct
	// implements both interfaces).  Nil means "no-op" in
	// isolated mode — required artifacts get a 0-length result.
	IsolatedUploader IsolatedUploader

	// Cache handles pipeline cache fetch/store when a job declares
	// `cache: [{key, paths}]`. Nil means "no-op" — the job runs
	// without any pre-populated cache dir and never uploads one.
	// Cache failures never fail a job: it's acceleration, not
	// correctness.
	Cache CacheClient

	// IsolatedCache is the isolated-mode counterpart of Cache.
	// Concrete impl is rpc.CacheClient (same struct implements
	// both). Nil → cache no-op in isolated mode.
	IsolatedCache IsolatedCacheClient

	// Engine executes each script task. Nil defaults to engine.Shell
	// — the pre-F3 behaviour (`sh -c` on the agent host). K8s-native
	// deployments set engine.Kubernetes.
	Engine engine.Engine

	// AgentTags is the set of tags this agent advertises at register
	// time. The runner forwards them to engine.ScriptSpec so the
	// Kubernetes engine can paint each as a Pod label — a quick way
	// to ask "which pool ran this job" without reading agent logs.
	AgentTags []string

	// KeepWorkspace keeps the job's working directory on disk after Execute
	// finishes. Useful for debugging; default is to remove on success.
	KeepWorkspace bool
}

// Runner is safe to share across concurrent Execute calls — each call uses
// its own workspace subdirectory. The in-flight registry (inflight + mu)
// lets the server push a CancelJob mid-execution and have the runner
// cancel that specific job's context without affecting siblings.
type Runner struct {
	cfg Config

	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc // job_id → cancel
}

func New(cfg Config) *Runner {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = filepath.Join(os.TempDir(), "gocdnext-workspace")
	}
	if cfg.Engine == nil {
		cfg.Engine = engine.NewShell()
	}
	return &Runner{cfg: cfg, inflight: map[string]context.CancelFunc{}}
}

// serviceLifecycleEmitter returns a callback that translates
// engine.ServiceLifecycleEvent into a ServiceLifecycle proto
// message and pushes it through cfg.Send (the outbound gRPC
// channel). nil Send → noop. Returns a fresh closure per call
// so a future per-run filter (e.g. dedup) can be added without
// rewriting the EnsureServices caller.
func (r *Runner) serviceLifecycleEmitter(runID string) func(engine.ServiceLifecycleEvent) {
	if r.cfg.Send == nil {
		return func(engine.ServiceLifecycleEvent) {}
	}
	return func(evt engine.ServiceLifecycleEvent) {
		r.cfg.Send(&gocdnextv1.AgentMessage{
			Kind: &gocdnextv1.AgentMessage_ServiceLifecycle{
				ServiceLifecycle: &gocdnextv1.ServiceLifecycle{
					RunId:   runID,
					Name:    evt.Name,
					Image:   evt.Image,
					PodName: evt.PodName,
					Status:  evt.Status,
					Error:   evt.Error,
					At:      timestamppb.New(time.Now().UTC()),
				},
			},
		})
	}
}

// Cancel signals the in-flight job with the given ID to stop. Returns
// true when a matching job was running (and its context was canceled),
// false when the job had already finished or never registered. Safe to
// call concurrently with Execute from the gRPC message dispatch loop.
func (r *Runner) Cancel(jobID string) bool {
	r.inflightMu.Lock()
	cancel, ok := r.inflight[jobID]
	r.inflightMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

func (r *Runner) registerInflight(jobID string, cancel context.CancelFunc) {
	r.inflightMu.Lock()
	r.inflight[jobID] = cancel
	r.inflightMu.Unlock()
}

func (r *Runner) deregisterInflight(jobID string) {
	r.inflightMu.Lock()
	delete(r.inflight, jobID)
	r.inflightMu.Unlock()
}

// Execute runs the assignment to completion: checkout each material, run
// each script task until one fails, emit a JobResult. Never panics on task
// failure — exit != 0 and checkout errors both resolve to RUN_STATUS_FAILED.
//
// Mode dispatch: when the engine is a Kubernetes engine configured for
// WorkspaceModeIsolated, the agent never touches the workspace — control
// flows through executeIsolated which spins up a Pod with an init container
// for prep, a task container for the user command, and a housekeeper
// sidecar the agent execs into for post-task work. Shared mode (default
// for backward compatibility) keeps the historical per-task RunScript loop.
