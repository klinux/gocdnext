// Package runner — execute_isolated.go owns the isolated workspace
// mode execution path: one Pod per Job, with an init container that
// materialises the workspace, a task container that runs the
// (single) declared task, and a housekeeper sidecar the agent execs
// into for post-task artefact upload.
//
// v0.5.0 limitations (explicit refusals, surfaced in JobResult):
//   - Multi-task jobs not yet supported; first task fails the job.
//   - Caches no-op (no gRPC session inside the pod).
//
// Both limitations have follow-up issues. Operators needing either
// stay on accessMode=ReadWriteMany (shared mode), unchanged.
package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// resolveIsolatedScriptWorkDir picks the task container's
// WorkingDir from the primary material's target_dir, matching
// shared mode's "follow the first checkout into its target_dir"
// behaviour. Returns mountPath when no checkouts are declared OR
// when the first checkout has no target_dir. Exposed for tests.
func resolveIsolatedScriptWorkDir(mountPath string, checkouts []*gocdnextv1.MaterialCheckout) string {
	if len(checkouts) == 0 {
		return mountPath
	}
	td := strings.TrimSpace(checkouts[0].GetTargetDir())
	if td == "" {
		return mountPath
	}
	return filepath.Join(mountPath, td)
}

// assignmentSecretCleanupTimeout caps how long we wait when
// deleting the assignment Secret outside the job's own context.
// Short — the call is just one DELETE against the apiserver; if
// it can't finish in 10s the cluster is in trouble.
const assignmentSecretCleanupTimeout = 10 * time.Second

// executeIsolated runs an end-to-end isolated-mode job. Assumes
// r.cfg.Engine is a *engine.Kubernetes already validated by the
// caller (Execute) as configured for WorkspaceModeIsolated.
func (r *Runner) executeIsolated(ctx context.Context, a *gocdnextv1.JobAssignment, k *engine.Kubernetes) {
	ctx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	r.registerInflight(a.GetJobId(), cancelJob)
	defer r.deregisterInflight(a.GetJobId())

	log := r.cfg.Logger.With(
		"run_id", a.GetRunId(),
		"job_id", a.GetJobId(),
		"name", a.GetName(),
		"mode", "isolated",
	)
	log.Info("runner: execute (isolated) start",
		"tasks", len(a.GetTasks()), "checkouts", len(a.GetCheckouts()))

	var seq atomic.Int64

	// v0.5.0 limitation: multi-task jobs not supported. Failing
	// early surfaces the misconfiguration before any pod work.
	if got := len(a.GetTasks()); got != 1 {
		msg := fmt.Sprintf(
			"isolated workspace mode supports exactly one task per job (got %d). "+
				"Switch to agent.workspace.accessMode=ReadWriteMany for multi-task jobs, "+
				"or refactor the job to a single shell-chained task.", got)
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}
	task := a.GetTasks()[0]

	// v0.11 limitation (issue #10 follow-up): structured job
	// outputs require reading $GOCDNEXT_OUTPUT_FILE from inside
	// the task container's filesystem AFTER tasks complete.
	// Shared-workspace mode reads from the same volume the agent
	// has mounted; isolated mode would need a housekeeper-exec +
	// `cat` round-trip (same shape as artifact upload). Not yet
	// implemented — fail loud so the downstream substitution
	// doesn't silently see an empty outputs map.
	//
	// Operators with outputs needs stay on
	// `agent.workspace.accessMode=ReadWriteMany` for now;
	// follow-up issue tracks parity.
	if len(a.GetOutputs()) > 0 {
		msg := fmt.Sprintf(
			"job declares outputs:%d but isolated workspace mode (accessMode=ReadWriteOnce) does not "+
				"yet support reading $GOCDNEXT_OUTPUT_FILE from the pod filesystem. "+
				"Switch the agent to accessMode=ReadWriteMany OR remove the outputs declaration "+
				"and have downstream consume `.gocdnext/*.env` workspace files via artifacts:/needs_artifacts: "+
				"(legacy compat path, still supported).",
			len(a.GetOutputs()))
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}

	// Pipeline services: brought up as standalone Pods by the
	// engine (EnsureServices), independent of the job's
	// workspace. Their HostAliases get wired into the task pod's
	// /etc/hosts via PodSpec.HostAliases so a `postgres:5432`-style
	// lookup resolves to the service Pod's IP — same plumbing as
	// shared mode. Lifecycle is run-scoped: the server's
	// CleanupRunServices broadcast tears them down on run-terminal,
	// so the cleanup returned here is intentionally a noop (calling
	// it per-job would kill services other jobs in the same run
	// still depend on).
	servicesPhase, svcErr := r.startServices(ctx, a, &seq)
	if svcErr != nil {
		msg := "services: " + svcErr.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}
	defer servicesPhase.cleanup()

	// test_reports require scanning JUnit XML on the agent's
	// filesystem; in isolated mode the reports live in the pod's
	// ephemeral PVC and there's no exec-side parser yet. Warn so
	// the operator knows the Tests tab will be empty; the job
	// itself still succeeds/fails on the task exit code.
	if len(a.GetTestReports()) > 0 {
		r.emitLog(a, &seq, "stderr", fmt.Sprintf(
			"test_reports: %d glob(s) declared but JUnit collection is not yet "+
				"supported in workspace isolated mode; the Tests tab will be empty. "+
				"Switch to agent.workspace.accessMode=ReadWriteMany if you need "+
				"per-case reporting on this job.", len(a.GetTestReports())))
	}

	cfg := k.Config()
	exec := k.Executor()
	if exec == nil {
		msg := "isolated mode requires a PodExecutor; agent build is misconfigured (engine.SetExecutor not called)"
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}

	// Mirror runner.Execute's "follow the first checkout into its
	// target_dir" rule so the task container's WorkingDir matches
	// where prep cloned the primary material. Without this
	// propagation, prep clones to /workspace/<target_dir>/ but the
	// task container starts at /workspace/, the plugin sees no
	// lockfile, and exits 2.
	scriptWorkDir := resolveIsolatedScriptWorkDir(cfg.WorkspaceMountPath, a.GetCheckouts())

	// Resolve the engine.IsolatedJobSpec — same shape as the
	// shared-mode ScriptSpec resolution in runner.go::runScript +
	// runner.go::runPlugin, kept here so isolated mode doesn't
	// loop through engine.RunScript (which is per-task).
	spec := engine.IsolatedJobSpec{
		RunID:       a.GetRunId(),
		JobID:       a.GetJobId(),
		WorkDir:     scriptWorkDir,
		Env:         a.GetEnv(),
		Docker:      a.GetDocker(),
		Resources:   assignmentResources(a),
		Profile:     a.GetProfile(),
		AgentTags:   append([]string(nil), r.cfg.AgentTags...),
		HostAliases: servicesPhase.hostAliases,
	}

	if plugin := task.GetPlugin(); plugin != nil {
		spec.Image = plugin.GetImage()
		// Plugin contract: PLUGIN_* env vars carry the settings; the
		// image's ENTRYPOINT is the logic. Merge into the job env
		// (job env wins on conflict, matching shared-mode behaviour).
		merged := make(map[string]string, len(a.GetEnv())+len(plugin.GetSettings()))
		for k, v := range plugin.GetSettings() {
			merged["PLUGIN_"+toUpperEnv(k)] = v
		}
		for k, v := range a.GetEnv() {
			merged[k] = v
		}
		spec.Env = merged
		// spec.Script stays "" so the image's ENTRYPOINT runs.
	} else {
		spec.Image = a.GetImage()
		spec.Script = task.GetScript()
		if spec.Script == "" {
			msg := "task has neither script nor plugin"
			r.emitLog(a, &seq, "stderr", msg)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
			return
		}
	}

	// Pre-resolve literal cache keys via the agent's gRPC session
	// and stamp the signed GET URLs onto each CacheEntry so the
	// init container can fetch via HTTP without a session of its
	// own. Templated keys (`{{ hash "..." }}`) need workspace
	// files to expand and are left unresolved — Prep skips them
	// with a warning. Failures here log + continue: cache is
	// acceleration, not correctness.
	if r.cfg.IsolatedCache != nil {
		for _, entry := range a.GetCaches() {
			if entry.GetKey() == "" {
				continue
			}
			if containsTemplate(entry.GetKey()) {
				continue
			}
			url, sha, found, rerr := r.cfg.IsolatedCache.ResolveGet(ctx,
				a.GetRunId(), a.GetJobId(), entry.GetKey())
			if rerr != nil {
				r.cfg.Logger.Warn("runner: cache pre-resolve failed",
					"err", rerr, "key", entry.GetKey(),
					"run_id", a.GetRunId(), "job_id", a.GetJobId())
				continue
			}
			entry.FetchFound = found
			if found {
				entry.FetchUrl = url
				entry.FetchSha256 = sha
			}
		}
	}

	// Serialise the JobAssignment so the init container can do
	// checkout + artifact-download from inside the pod.
	assignmentBytes, err := proto.Marshal(a)
	if err != nil {
		msg := "marshal assignment: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}

	created, secretName, err := k.CreateIsolatedJobPod(ctx, spec, assignmentBytes)
	if err != nil {
		// CreateIsolatedJobPod may return a pod + an ownerref error;
		// if pod is non-nil, schedule cleanup so we don't leak it.
		// Note: CreateIsolatedJobPod already deletes the Secret on
		// owner-patch failure, so we don't need to here.
		if created != nil {
			_ = k.DeleteIsolatedJobPod(context.Background(), created.Name)
		}
		msg := "create isolated pod: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		return
	}
	podName := created.Name
	log = log.With("pod", podName)
	log.Info("runner: isolated pod created")

	// The assignment Secret carries env + signed URLs from the
	// JobAssignment. Once the init container has consumed it (i.e.
	// after WaitForInitTerminated returns), it has no further use
	// and SHOULD NOT outlive prep — even when the Pod is kept for
	// debugging via CleanupOnFailure=false. deleteAssignmentSecret
	// is wrapped in sync.Once so the explicit call at "post-prep"
	// is authoritative and the defer below is a belt-and-braces
	// fallback for early-return / panic paths.
	var secretDeleteOnce sync.Once
	deleteAssignmentSecret := func() {
		secretDeleteOnce.Do(func() {
			if secretName == "" {
				return
			}
			delCtx, cancel := context.WithTimeout(context.Background(), assignmentSecretCleanupTimeout)
			defer cancel()
			if err := k.DeleteAssignmentSecret(delCtx, secretName); err != nil {
				r.cfg.Logger.Warn("runner: assignment secret cleanup failed",
					"err", err, "secret", secretName, "job_id", a.GetJobId())
			}
		})
	}
	defer deleteAssignmentSecret()

	// Cap the time it takes for the prep init container to leave
	// Pending. Without this, a stuck PVC bind / image pull / no-
	// schedule condition keeps the job "running" forever.
	if err := k.WaitForInitStarted(ctx, podName, "prep"); err != nil {
		msg := "prep startup timeout: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}

	// Stream init container logs (prep) in a separate goroutine
	// while we wait for it to terminate. Init logs go through the
	// same emit pipeline as task logs but with a "init.prep"
	// stream tag the UI can group on.
	prepDone := make(chan struct{})
	go func() {
		defer close(prepDone)
		k.StreamInitLogs(ctx, podName, "prep", func(_, line string) {
			r.emitLog(a, &seq, "init.prep", line)
		})
	}()

	prepExit, err := k.WaitForInitTerminated(ctx, podName, "prep")

	// Authoritative deletion point: prep has consumed the
	// JobAssignment payload, the Secret has no further use, and
	// every downstream path (failure OR success) proceeds without
	// it. Doing this BEFORE the log-stream join means a stuck
	// streamContainerLogs retry loop (capped at StartupTimeout)
	// can't delay the deletion or downstream progress — the
	// payload is gone the instant prep terminates.
	deleteAssignmentSecret()

	// Join the prep log stream goroutine AFTER secret deletion so
	// a slow / unreachable log endpoint can't gate it. The retry
	// loop bounds itself by StartupTimeout; this wait is a
	// formality to drain in-flight lines before we report
	// JobResult.
	<-prepDone

	if err != nil {
		msg := "wait for prep: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}
	if prepExit != 0 {
		msg := fmt.Sprintf("prep init container exited %d", prepExit)
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(prepExit), msg)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}
	log.Info("runner: prep ok, task starting")

	// Cap the time it takes for the task container to leave
	// Waiting. ImagePullBackOff or similar would otherwise let
	// the Pod sit in a containerStatus=Waiting state indefinitely
	// — the housekeeper is Running so Pod phase is Running, the
	// log streamer gives up after StartupTimeout, but the
	// WaitForTaskTerminated poller has nothing to time out on.
	if err := k.WaitForTaskStarted(ctx, podName); err != nil {
		msg := "task startup timeout: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}

	// Stream task logs.
	taskDone := make(chan struct{})
	go func() {
		defer close(taskDone)
		k.StreamTaskLogs(ctx, podName, func(stream, line string) {
			r.emitLog(a, &seq, stream, line)
		})
	}()

	taskExit, err := k.WaitForTaskTerminated(ctx, podName)
	<-taskDone
	if err != nil {
		msg := "wait for task: " + err.Error()
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}
	if taskExit != 0 {
		log.Info("runner: task exited non-zero (isolated)", "exit", taskExit)
		// Post-task work (artifact upload) requires the
		// housekeeper sidecar to be alive. If the task failed but
		// housekeeper is still around, we COULD still attempt
		// optional artifact upload — but v0.5.0 keeps things
		// simple: failure = no post-task upload.
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(taskExit),
			fmt.Sprintf("task exited with %d", taskExit))
		r.cleanupIsolatedPod(k, podName, false)
		return
	}

	// Task succeeded — run post-task work via housekeeper exec.
	// PodWorkDir is the SCRIPT working dir, not the PVC mount
	// root: artifact + cache paths in the YAML are relative to
	// where the user's task ran (= scriptWorkDir, post-target_dir
	// resolution), matching shared mode's uploader contract
	// (runner.go::uploadArtifacts passes scriptWorkDir). Using the
	// mount root drops the target_dir prefix and breaks tar in
	// the housekeeper.
	refs, postErr := r.PostJob(ctx, PostJobConfig{
		Executor:      exec,
		Uploader:      r.cfg.IsolatedUploader,
		Cache:         r.cfg.IsolatedCache,
		PodName:       podName,
		HousekeeperCt: "housekeeper",
		PodWorkDir:    scriptWorkDir,
	}, a, &seq)
	if postErr != nil {
		r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
			"artifact upload failed: "+postErr.Error(), refs)
		r.cleanupIsolatedPod(k, podName, false)
		return
	}

	log.Info("runner: execute (isolated) ok", "artifacts", len(refs))
	r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_SUCCESS, 0, "", refs)
	r.cleanupIsolatedPod(k, podName, true)
}

// cleanupIsolatedPod deletes the pod respecting the engine's
// CleanupOn{Success,Failure} flags. Best-effort: errors are
// logged but never propagated (the pod will be GC'd by k8s
// eventually, and a stuck pod is something the operator wants
// to see).
func (r *Runner) cleanupIsolatedPod(k *engine.Kubernetes, podName string, success bool) {
	cfg := k.Config()
	keep := (!success && !cfg.CleanupOnFailure) || (success && !cfg.CleanupOnSuccess)
	if keep {
		return
	}
	if err := k.DeleteIsolatedJobPod(context.Background(), podName); err != nil {
		r.cfg.Logger.Warn("runner: cleanup isolated pod failed", "err", err, "pod", podName)
	}
}

// toUpperEnv converts a kebab-case input name to UPPER_SNAKE_CASE
// for PLUGIN_* env var naming. Matches the docker engine's
// plugin invocation logic. Kept here (not factored into runner.go)
// so the isolated path is self-contained.
func toUpperEnv(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c == '-':
			out = append(out, '_')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
