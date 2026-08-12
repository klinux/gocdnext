package runner

import (
	"context"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// servicePhase is what the runner threads into ScriptSpec after
// calling engine.EnsureServices. Network is set when the docker
// engine brought up a job-scoped network; hostAliases is set when
// the k8s engine brought up one Pod per service. cleanup is
// always non-nil and safe to call from a defer regardless of the
// outcome.
type servicePhase struct {
	network     string
	hostAliases []engine.HostAlias
	cleanup     func()
}

// startServices delegates the per-engine "bring services up" work to
// the configured engine, then funnels the wireup result back into a
// shape the rest of runJob consumes. Engines that can't host
// services (shell) return an error from EnsureServices — the runner
// just propagates it.
//
// The seq pointer is the per-run log-line sequence; we pass a logger
// closure that writes through emitLog so engine-emitted lines
// ("$ starting service postgres (postgres:16)") get the same
// run_id + sequence as the rest of the run's stdout.
func (r *Runner) startServices(
	ctx context.Context,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) (servicePhase, error) {
	noop := servicePhase{cleanup: func() {}}
	services := a.GetServices()
	if len(services) == 0 {
		return noop, nil
	}

	specs := toEngineServiceSpecs(a)
	log := func(stream, text string) { r.emitLog(a, seq, stream, text) }
	onLifecycle := r.serviceLifecycleEmitter(a.GetRunId())

	wireup, err := r.cfg.Engine.EnsureServices(ctx, specs, a.GetRunId(), a.GetJobId(), a.GetServiceGeneration(), log, onLifecycle)
	if err != nil {
		// EnsureServices contract: engines clean up partials internally
		// before returning a non-nil error. We don't double-invoke
		// Cleanup here — the deferred call would also fire on every
		// success path so making this branch invoke it would mean
		// two passes (idempotent, but noisy and harder to reason about
		// when debugging "did cleanup run?" in agent logs).
		return noop, err
	}
	if wireup.Cleanup == nil {
		// Defensive: an engine returning a nil cleanup would crash
		// the defer in runJob. Replace with a noop so the contract
		// the rest of the code relies on holds even when an engine
		// implementation forgets.
		wireup.Cleanup = func() {}
	}
	return servicePhase{
		network:     wireup.Network,
		hostAliases: wireup.HostAliases,
		cleanup:     wireup.Cleanup,
	}, nil
}

// toEngineServiceSpecs converts the proto-side service list into the
// engine-facing shape. Lives in the runner package so the engine
// stays proto-free (it's a runtime contract, not a wire contract).
// Copies maps/slices defensively so a subsequent mutation of the
// proto message can't leak into engine state.
//
// Each service inherits the JOB's profile-resolved scheduling (the same
// node_selector/tolerations/preferred_node_affinity the task pod gets)
// so a run-shared service pod lands on the same pool as the jobs that
// use it rather than wherever the default scheduler places it. The
// assignment* helpers already return fresh copies; the three values are
// read-only in the engine (merged into a new PodSpec), so sharing them
// across the batch is safe.
func toEngineServiceSpecs(a *gocdnextv1.JobAssignment) []engine.ServiceSpec {
	services := a.GetServices()
	if len(services) == 0 {
		return nil
	}
	jobNodeSelector := assignmentNodeSelector(a)
	jobTolerations := assignmentTolerations(a)
	preferredAffinity := assignmentPreferredNodeAffinity(a)
	out := make([]engine.ServiceSpec, 0, len(services))
	for _, svc := range services {
		env := make(map[string]string, len(svc.GetEnv()))
		for k, v := range svc.GetEnv() {
			env[k] = v
		}
		out = append(out, engine.ServiceSpec{
			Name:    svc.GetName(),
			Image:   svc.GetImage(),
			Env:     env,
			Command: append([]string(nil), svc.GetCommand()...),
			// Per-service override WINS over the inherited job scheduling:
			// node_selector keys override, tolerations are appended. Affinity is
			// inherited-only (no per-service affinity override).
			NodeSelector:          overrideNodeSelector(jobNodeSelector, svc.GetNodeSelector()),
			Tolerations:           appendTolerations(jobTolerations, protoTolerationsToCorev1(svc.GetTolerations())),
			PreferredNodeAffinity: preferredAffinity,
		})
	}
	return out
}

// overrideNodeSelector merges the per-service override onto the inherited job
// node selector — override keys WIN on collision (the operator pinned the
// service to a specific pool on purpose). Fresh map; nil when both are empty.
func overrideNodeSelector(inherited, override map[string]string) map[string]string {
	if len(inherited) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(inherited)+len(override))
	for k, v := range inherited {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// appendTolerations concatenates inherited job tolerations with the per-service
// override — tolerations are additive (tolerating a taint the node doesn't carry
// is harmless). Fresh slice; nil when both are empty.
func appendTolerations(inherited, override []corev1.Toleration) []corev1.Toleration {
	if len(inherited) == 0 && len(override) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(inherited)+len(override))
	out = append(out, inherited...)
	out = append(out, override...)
	return out
}
