// Package engine defines how an agent actually executes the script
// portion of a job. Different engines map to different runtimes:
//
//   - Shell:      exec.Command against the agent's own host. Fast,
//     zero dependencies; ignores Image and Env isolation.
//     The dev/local default.
//   - Kubernetes: (F3.2) spawns one Pod per script with the job's
//     declared Image, streaming logs back via the watch
//     API.
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
	// HostAliases maps service names to IPs the task container can
	// reach. The Kubernetes engine writes these into PodSpec.hostAliases
	// so a `postgres:5432`-style lookup resolves to the service pod's
	// IP. EnsureServices (below) builds the list before RunScript is
	// called. The Docker engine ignores this field — it uses
	// container-network DNS aliases instead.
	HostAliases []HostAlias
	OnLine      func(stream, text string)

	// OutputsHostPath is the absolute path on the host filesystem
	// where the agent will read the job's output file after this
	// RunScript returns. Set by the runner via prepareOutputsFile
	// when the job declared an `outputs:` block; empty string
	// signals "no outputs, skip GOCDNEXT_OUTPUT_FILE injection".
	//
	// The engine uses this to inject the env var with the path
	// the SCRIPT will actually see — host path for Shell-mode,
	// container path for Docker/K8s — so the file the plugin
	// writes inside its execution context lands at the same
	// place the agent will read afterward.
	OutputsHostPath string

	// OutputsRelPath is OutputsHostPath made workspace-relative
	// (e.g. ".gocdnext/outputs/<short-id>.env"). Engines that
	// containerize compose the env value as `/workspace/<rel>`;
	// engines that run on the host use OutputsHostPath directly.
	// Empty when OutputsHostPath is empty.
	OutputsRelPath string
}

// OutputsEnvName is the env variable the plugin / script reads to
// know where to write its structured outputs (issue #10). Exported
// from the engine package so both runner (injection) and engine
// implementations (overwriting on fallback / containerize) name
// the same string without duplication drift.
const OutputsEnvName = "GOCDNEXT_OUTPUT_FILE"

// ContainerWorkspaceMount is the in-container mount point every
// containerizing engine (Docker, Kubernetes) maps the host
// workspace to. Centralised so a future engine that picks a
// different convention has ONE place to change. The Shell engine
// ignores this — its scripts see the host path directly.
const ContainerWorkspaceMount = "/workspace"

// ServiceSpec is the engine-facing shape of a pipeline-level
// service. Mirrors the gocdnextv1.ServiceSpec proto but lives in
// the engine package so engines don't have to depend on the proto
// package directly.
type ServiceSpec struct {
	Name    string
	Image   string
	Env     map[string]string
	Command []string
}

// HostAlias is the engine-agnostic shape an engine plumbs into its
// runtime's hostname-resolution layer. For Kubernetes this maps
// 1-to-1 onto corev1.HostAlias; for Docker it's redundant with the
// network-DNS alias the engine already wires.
type HostAlias struct {
	IP        string
	Hostnames []string
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

// ServiceLifecycleEvent is the engine-agnostic shape an engine
// emits as service pods transition state. The runner forwards
// these to the server as proto ServiceLifecycle messages so the
// UI can render a service node with status + duration alongside
// the job graph. Status values: starting (Pod create issued),
// ready (Pod reached Running / podIP assigned), stopped (Pod
// deleted), failed (Pod never reached Running within startup
// timeout, image pull backoff, etc.).
type ServiceLifecycleEvent struct {
	Name    string
	Image   string
	PodName string
	Status  string
	Error   string
}

// ServicesWireup is what an engine returns from EnsureServices so
// the runner can plumb the result back into the task's ScriptSpec.
// Different engines populate different fields — docker uses Network
// (a docker network the task container joins so service hostnames
// resolve via container DNS), kubernetes uses HostAliases (mapping
// each service name to its pod IP via /etc/hosts entries on the
// task pod). Cleanup is always non-nil and idempotent — safe to
// call whether services started cleanly, partially, or not at all.
type ServicesWireup struct {
	Network     string
	HostAliases []HostAlias
	Cleanup     func()
}

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
	//
	// Outputs (issue #10): when spec.OutputsHostPath is non-empty,
	// the engine MUST inject GOCDNEXT_OUTPUT_FILE into the task's
	// env at the value the task will actually see (host path for
	// Shell, container path for Docker/K8s). The runner only sets
	// the HOST path; the engine is the one who knows whether it'll
	// containerize. This shape catches the Docker-fallback-to-Shell
	// case where the wrong env was being injected.
	RunScript(ctx context.Context, spec ScriptSpec) (exitCode int, err error)

	// EnsureServices brings up the declared pipeline services scoped
	// to a RUN (not a job): the first job of a run that needs a given
	// service creates the pod, every subsequent job of the same run
	// reuses it via the deterministic per-runID name. The wireup
	// returned plumbs into ScriptSpec (Network for docker,
	// HostAliases for kubernetes). Empty services → zero ServicesWireup
	// with a noop Cleanup.
	//
	// runID is the run-level identity used to name + label pods. jobID
	// stays in the label set for observability (`kubectl get pods -l
	// gocdnext.io/job=...` still works to trace which job FIRST brought
	// the service up) but does NOT factor into naming.
	//
	// onLifecycle, when non-nil, receives one event per service-pod
	// state transition: starting (Pod create issued), ready (Pod
	// reached Running / podIP assigned), failed (Pod never started
	// in time). Reused-from-sibling pods emit `ready` immediately
	// (the work already happened). The runner forwards these to the
	// server as proto ServiceLifecycle messages.
	//
	// The returned Cleanup is now a NO-OP: per-job teardown would kill
	// services other jobs in the same run still depend on. Service
	// lifecycle is run-scoped, driven solely by the server's
	// run-terminal CleanupRunServices RPC broadcast. If that
	// dispatch fails (no eligible agent connected at terminal),
	// pods leak until manual cleanup via `kubectl delete pods -l
	// app.kubernetes.io/managed-by=gocdnext-agent,gocdnext.io/run-id=<id>`.
	//
	// Engines that don't implement services return an error when
	// len(services) > 0.
	EnsureServices(ctx context.Context, services []ServiceSpec, runID, jobID string, log func(stream, text string), onLifecycle func(ServiceLifecycleEvent)) (ServicesWireup, error)

	// CleanupRunServices tears down every service pod/container
	// labelled with the given runID. Driven by the server's
	// CleanupRunServices ServerMessage (issued on run terminal,
	// BROADCAST to every agent that ran a job of the run plus every
	// currently-connected agent — the wide target set guarantees a
	// k8s-capable agent receives the message even if the original
	// creator has disconnected). Idempotent: a label-selector delete
	// against an already-cleaned run is a successful no-op.
	// Returns the count of resources actually deleted; errors from
	// partial deletes are aggregated and returned alongside the
	// count.
	//
	// onLifecycle, when non-nil, receives one `stopped` event per
	// successfully deleted service pod so the server can stamp the
	// stopped_at timestamp on the corresponding service_runs row.
	// NotFound deletes (raced by sibling agents) don't emit — only
	// THIS agent's confirmed deletions, otherwise the timestamp
	// races between agents.
	//
	// Engines that don't host services (Shell, Docker today) return
	// (0, nil). The server now filters the broadcast at SQL layer
	// (agents.engine='kubernetes' or legacy '') AND at the
	// in-memory session layer (Session.Engine match), so non-k8s
	// agents shouldn't normally receive this message. The
	// defensive no-op stays in place to cover legacy/unknown
	// engines during a rolling upgrade.
	CleanupRunServices(ctx context.Context, runID string, onLifecycle func(ServiceLifecycleEvent)) (int, error)
}
