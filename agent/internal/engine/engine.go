// Package engine defines how an agent actually executes the script
// portion of a job. Different engines map to different runtimes:
//
//   - Shell:      exec.Command against the agent's own host. Fast,
//                 zero dependencies; ignores Image and Env isolation.
//                 The dev/local default.
//   - Kubernetes: (F3.2) spawns one Pod per script with the job's
//                 declared Image, streaming logs back via the watch
//                 API.
//
// The runner (`agent/internal/runner`) owns everything outside the
// engine's responsibility — checkout, artifact download/upload, log
// masking, JobResult emission. The engine only has to "run this
// script and emit lines as you see them".
package engine

import "context"

// ScriptSpec is the contract: a pre-materialised workspace, an
// optional image (engines that don't containerise ignore this), env
// vars, and a shell script. OnLine is the callback for each stdout/
// stderr line — called from goroutines owned by the engine; the
// runner is responsible for thread-safety if it shares state.
type ScriptSpec struct {
	WorkDir string
	Image   string
	Env     map[string]string
	Script  string
	// Docker asks the engine to make a Docker API reachable from
	// inside the script — docker.sock mount for Shell/Docker
	// engines, DinD sidecar for Kubernetes. Engines that can't
	// satisfy this should return an error before running.
	Docker bool
	// Network, when non-empty, joins the task container to the
	// named docker network — how pipeline-level services get
	// reached by hostname. The runner creates the network + brings
	// service containers up on it BEFORE calling RunScript. Shell
	// engine ignores this field (no container to attach).
	Network string
	// Resources is the per-script compute envelope. Strings carry
	// the raw k8s quantity literal ("100m", "256Mi"); empty means
	// "not set" and the engine falls through to its own default.
	// Honoured by the Kubernetes engine (mapped into the Pod
	// container's resources block); ignored by Shell/Docker.
	Resources Resources
	// Profile is the name of the resolved runner profile that
	// produced this script. The Kubernetes engine surfaces it as
	// a Pod label so operators can grep `kubectl get pods -l
	// gocdnext.io/profile=gpu` to see which workloads landed on
	// which pool.
	Profile string
	// AgentTags carries the running agent's own tags so the
	// Kubernetes engine can paint each as a label on the spawned
	// Pod. Helps debug "which pool ran this job" without trawling
	// agent logs.
	AgentTags []string
	OnLine    func(stream, text string)
}

// Resources mirrors the proto ResourceRequirements but lives in the
// engine package so the runner doesn't have to round-trip through
// proto types when calling the engine. Empty fields mean "not set".
type Resources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// IsZero reports whether no field is set — engines short-circuit
// the resources mapping when true.
func (r Resources) IsZero() bool { return r == Resources{} }

// Engine is the narrow contract: "run this, tell me when each line
// comes out, give me the exit code". Errors are reserved for
// lifecycle problems (fork/exec failure, Pod creation failure) —
// exit != 0 returns (N, nil).
type Engine interface {
	// Name identifies the engine for log/metric labels.
	Name() string

	// RunScript blocks until the script finishes or ctx is cancelled.
	// Returns the exit code. err != nil signals "could not run at
	// all" (distinct from "ran and failed").
	RunScript(ctx context.Context, spec ScriptSpec) (exitCode int, err error)
}
