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
	"errors"
	"fmt"
	"path"
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

	// Outputs (issue #10 isolated parity, since v0.12):
	//   - agent computes outputsRel from job id (same convention as
	//     shared mode's prepareOutputsFile)
	//   - prep init container mkdir+touches the file inside the
	//     pod's PVC (workspace-relative, world-writable so any
	//     task USER can write to it)
	//   - engine injects GOCDNEXT_OUTPUT_FILE pointing at the
	//     absolute container-side path via spec.OutputsRelPath
	//   - on task success, agent execs `cat -- <abs path>` inside
	//     the housekeeper sidecar via PodExecutor and parses the
	//     stream with parseOutputsReader (same cap/charset/dedupe
	//     guarantees as shared mode)
	//   - on task failure, outputs are NOT read — failed jobs ship
	//     no outputs in JobResult, matching shared mode
	//
	// Empty outputs map → entire path skipped; behaviour identical
	// to v0.11 jobs that didn't declare outputs.
	var outputsRel string
	if len(a.GetOutputs()) > 0 {
		outputsRel = OutputsRelPath(a.GetJobId())
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

	// test_reports collection runs post-task via
	// scanTestReportsFromPod below (housekeeper exec). The scan
	// fires on BOTH task-success and non-zero-exit / wait-error
	// paths — failed builds are exactly when the Tests tab carries
	// the highest signal (which assertion failed, which stack
	// trace), and shared mode's scanTestReports has the same
	// behaviour (runner.go::Execute calls it on every post-task
	// branch including the failure ones). Pre-v0.14.4 this point
	// in the flow emitted a warn telling operators to switch back
	// to ReadWriteMany for Tests-tab visibility; that gap is now
	// closed and both workspace modes ship the same data.

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
	// Detect templated cache keys upfront — drives both the pod
	// spec (cache-fetch init container) and the post-prep
	// orchestration below. Doing the scan here keeps the spec
	// shape deterministic at pod-create time.
	needsCacheFetch := false
	for _, e := range a.GetCaches() {
		if containsTemplate(e.GetKey()) {
			needsCacheFetch = true
			break
		}
	}

	spec := engine.IsolatedJobSpec{
		RunID:               a.GetRunId(),
		JobID:               a.GetJobId(),
		WorkDir:             scriptWorkDir,
		Env:                 a.GetEnv(),
		Docker:              a.GetDocker(),
		Resources:           assignmentResources(a),
		Profile:             a.GetProfile(),
		AgentTags:           append([]string(nil), r.cfg.AgentTags...),
		HostAliases:         servicesPhase.hostAliases,
		OutputsRelPath:      outputsRel,
		NodeSelector:        assignmentNodeSelector(a),
		Tolerations:         assignmentTolerations(a),
		NeedsCacheFetchInit: needsCacheFetch,
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
		r.cleanupIsolatedPod(ctx, k, podName, false)
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
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}
	if prepExit != 0 {
		msg := fmt.Sprintf("prep init container exited %d", prepExit)
		r.emitLog(a, &seq, "stderr", msg)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(prepExit), msg)
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}
	log.Info("runner: prep ok, task starting")

	// Cache key template resolution (issue #17). When the assignment
	// declared `{{ hash "..." }}` keys, the engine appended a
	// `cache-fetch` init container that polls for a marker file. It
	// runs AFTER prep (so the workspace is materialised) and BEFORE
	// the main containers start. The agent execs into it to:
	//   1. List workspace files via `find` (podfs.ListFiles).
	//   2. Compute hashes via `cat` (podHashResolver).
	//   3. Resolve the templated keys.
	//   4. Call RequestCacheGet through the agent's existing gRPC
	//      session — same path literal keys take.
	//   5. Stream the cache tarball into the workspace via exec
	//      stdin (so the cache-fetch container doesn't need network).
	//   6. Touch the marker — cache-fetch exits, K8s starts task.
	//
	// Failure mode: cache resolution / fetch errors are best-effort.
	// We touch the marker anyway so the task still starts; the job
	// runs without the cache and the store path on success rebuilds.
	if needsCacheFetch {
		// mountPath (PVC root, marker + cache untar anchor) is
		// distinct from scriptWorkDir (hash resolver's enumeration
		// root, target_dir-aware). See cache_isolated.go for the
		// rationale.
		r.resolveAndFetchTemplatedCaches(ctx, k, exec, podName, cfg.WorkspaceMountPath, scriptWorkDir, a, &seq)
	}

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
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}

	taskStart := time.Now()

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
		// The wait failed — most often the pod vanished mid-run
		// (evicted/preempted/node reclaimed). Classify so the operator
		// gets the real reason instead of a raw poll error.
		term := k.TaskPodTermination(ctx, podName)
		msg := "wait for task: " + err.Error()
		if term.PodGone || term.Disrupted {
			msg = disruptionMessage(term)
		}
		r.emitLog(a, &seq, "stderr", msg)
		// When the pod is gone, the housekeeper sidecar is gone too —
		// scanning would only emit "container not found" noise. Skip it.
		// When only the task container died, the housekeeper may still be
		// alive, so scan test_reports for diagnostic signal (best-effort).
		if !(term.PodGone || term.Disrupted) {
			r.scanTestReportsFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq)
			r.scanCoverageFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq)
		}
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, msg)
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}
	if taskExit != 0 {
		log.Info("runner: task exited non-zero (isolated)", "exit", taskExit)

		// exit 143 == 128+SIGTERM: the task was terminated EXTERNALLY,
		// not by its own logic — usually node preemption/eviction or a
		// cancel. Classify via the pod so the result names the real
		// cause instead of a bare "exited with 143". Other non-zero
		// exits are ordinary task failures and keep the old shape.
		resultMsg := fmt.Sprintf("task exited with %d", taskExit)
		phaseMsg := fmt.Sprintf("task failed after %s (exit %d)", phaseDur(taskStart), taskExit)
		skipScans := false
		if taskExit == 143 {
			term := k.TaskPodTermination(ctx, podName)
			if term.PodGone || term.Disrupted {
				// Platform reclaimed the pod. The housekeeper is gone, so
				// scanning would only emit "container not found" — skip it.
				resultMsg = disruptionMessage(term)
				phaseMsg = resultMsg
				skipScans = true
			} else {
				// Pod + housekeeper still alive: a process inside the job
				// took SIGTERM on its own. Name it, but KEEP the scans —
				// the test reports may still carry useful signal.
				phaseMsg = fmt.Sprintf("task terminated by SIGTERM (exit 143) after %s", phaseDur(taskStart))
			}
		}
		r.emitPhase(a, &seq, phaseMsg)

		// test_reports DO get scanned on a failed task — that's exactly
		// when the Tests tab carries the highest signal (which assertion
		// failed, which stack trace). Mirrors shared-mode behaviour
		// (runner.go::Execute). Skipped only when the pod is gone.
		// (Post-task artifact upload still doesn't run on failure.)
		var failRefs []*gocdnextv1.ArtifactRef
		if !skipScans {
			r.scanTestReportsFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq)
			r.scanCoverageFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq)
			// artifacts.when: on_failure/always still ship on a red job so a
			// blocking scanner's SARIF reaches the dashboard. Requires the
			// housekeeper alive — skipScans (pod gone/disrupted) also skips it.
			failRefs = r.postJobArtifactsOnFailure(ctx, exec, podName, scriptWorkDir, a, &seq)
		}
		r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(taskExit), resultMsg, failRefs)
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}

	r.emitPhase(a, &seq, fmt.Sprintf("task completed in %s", phaseDur(taskStart)))

	// Task succeeded — ship test reports + coverage BEFORE the
	// post-job work. The evidence exists the moment tasks finish;
	// a failed artifact upload or a malformed outputs file must
	// not erase the Tests tab / coverage of a build that actually
	// ran (review-round MEDIUM: the early returns below used to
	// skip both scans, diverging from shared mode's scan-first
	// order in runner.go).
	r.scanTestReportsFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq)
	if gateFailed, reason := r.scanCoverageFromPod(ctx, exec, podName, "housekeeper", scriptWorkDir, a, &seq); gateFailed {
		// fail_under: green build under the declared floor — job fails before
		// cache store, mirroring shared mode and task failures. The pod is
		// still alive (the task succeeded), so artifacts.when=on_failure/always
		// can still ship via the housekeeper — a blocking scanner's SARIF must
		// reach the dashboard even when the coverage gate tripped the job.
		failRefs := r.postJobArtifactsOnFailure(ctx, exec, podName, scriptWorkDir, a, &seq)
		r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1, reason, failRefs)
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}

	// Post-task work via housekeeper exec.
	// PodWorkDir is the SCRIPT working dir, not the PVC mount
	// root: artifact + cache paths in the YAML are relative to
	// where the user's task ran (= scriptWorkDir, post-target_dir
	// resolution), matching shared mode's uploader contract
	// (runner.go::uploadArtifacts passes scriptWorkDir). Using the
	// mount root drops the target_dir prefix and breaks tar in
	// the housekeeper.
	var refs []*gocdnextv1.ArtifactRef
	var postErr error
	r.timedPhase(a, &seq, "post-job (artifacts + caches)", func() {
		refs, postErr = r.PostJob(ctx, PostJobConfig{
			Executor:      exec,
			Uploader:      r.cfg.IsolatedUploader,
			Cache:         r.cfg.IsolatedCache,
			PodName:       podName,
			HousekeeperCt: "housekeeper",
			PodWorkDir:    scriptWorkDir,
			// artifacts.when=on_failure wants the upload only on a red job;
			// skip it here (caches still run). always/on_success upload.
			SkipArtifacts: !shouldUploadArtifacts(a.GetArtifactsWhen(), false),
		}, a, &seq)
	})
	if postErr != nil {
		r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
			"artifact upload failed: "+postErr.Error(), refs)
		r.cleanupIsolatedPod(ctx, k, podName, false)
		return
	}

	// Outputs (isolated parity): cat the file via the same
	// housekeeper sidecar, against the SAME PVC mount path the
	// engine injected as GOCDNEXT_OUTPUT_FILE. Done AFTER artifact
	// upload because (a) artifacts can fail-loud and we'd lose the
	// outputs map anyway, and (b) operators expect "outputs land
	// when the job succeeds end-to-end" — failed artifact upload =
	// failed job = no outputs propagation.
	var producedOutputs map[string]string
	if outputsRel != "" {
		// Join under scriptWorkDir (NOT cfg.WorkspaceMountPath):
		// when a checkout declared target_dir, scriptWorkDir is
		// `<mount>/<target_dir>` and prep wrote the outputs file
		// under that subdir. The engine's GOCDNEXT_OUTPUT_FILE
		// uses the same anchor (see kubernetes_isolated.go),
		// so producer and consumer agree on the path even when
		// target_dir nests the workspace one level down.
		//
		// `path.Join` (not `filepath.Join`) — the value is
		// consumed inside a Linux container regardless of the
		// agent's host OS, matching the engine-side style.
		containerOutputsPath := path.Join(scriptWorkDir, outputsRel)
		got, err := ReadOutputsFromPod(ctx, exec, podName, "housekeeper", containerOutputsPath, a.GetOutputs())
		if err != nil {
			// Never log the file contents — values may be tokens
			// or digests. The parser's error message names the
			// offending KEY / line number, not values, per the
			// hardening in [[roadmap_issue_10_outputs]].
			r.cfg.Logger.Warn("runner: outputs parse failed (isolated)",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
				"outputs: "+err.Error(), refs, nil)
			r.cleanupIsolatedPod(ctx, k, podName, false)
			return
		}
		producedOutputs = got
	}

	outputKeys := make([]string, 0, len(producedOutputs))
	for k := range producedOutputs {
		outputKeys = append(outputKeys, k)
	}
	log.Info("runner: execute (isolated) ok",
		"artifacts", len(refs), "output_keys", outputKeys)
	r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_SUCCESS, 0, "", refs, producedOutputs)
	r.cleanupIsolatedPod(ctx, k, podName, true)
}

// cleanupIsolatedPodDeleteTimeout caps the cancel-override DELETE
// so a wedged kube-apiserver or hung connection can't pin the
// runner indefinitely on the very path that's supposed to be
// "free the slot". 10s matches assignmentSecretCleanupTimeout and
// the engine's cleanupPodDeleteTimeout — one DELETE shouldn't
// take longer.
const cleanupIsolatedPodDeleteTimeout = 10 * time.Second

// cleanupIsolatedPod deletes the pod respecting the engine's
// CleanupOn{Success,Failure} flags. Best-effort: errors are
// logged but never propagated (the pod will be GC'd by k8s
// eventually, and a stuck pod is something the operator wants
// to see).
//
// Cancellation override: when the job's ctx was canceled (the
// signal Runner.Cancel sends in response to a server-side
// CancelJob RPC), the pod is deleted unconditionally — operators
// expect "Cancel" to actually stop the running container, not
// leave it lingering under CleanupOnFailure=false. Using
// ctx.Err() (the in-scope ctx that drove the run) keeps the
// detection local and avoids threading a separate flag through
// every call site. The DELETE itself runs against a fresh
// background ctx (bounded by cleanupIsolatedPodDeleteTimeout) so
// the canceled run ctx doesn't abort it before kube-apiserver
// hears the call, and a hung apiserver can't pin the runner.
func (r *Runner) cleanupIsolatedPod(ctx context.Context, k *engine.Kubernetes, podName string, success bool) {
	cfg := k.Config()
	canceled := errors.Is(ctx.Err(), context.Canceled)
	keep := !canceled && ((!success && !cfg.CleanupOnFailure) || (success && !cfg.CleanupOnSuccess))
	if keep {
		return
	}
	delCtx, cancel := context.WithTimeout(context.Background(), cleanupIsolatedPodDeleteTimeout)
	defer cancel()
	if err := k.DeleteIsolatedJobPod(delCtx, podName); err != nil {
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

// disruptionMessage renders a clear, operator-facing reason for a pod the
// platform reclaimed (eviction / node preemption / node loss / deletion),
// in place of a bare "exited with 143" or a "container not found" scan
// error. Falls back to a generic line when the pod couldn't be classified.
func disruptionMessage(t engine.TaskTermination) string {
	msg := "job pod terminated externally"
	if t.Reason != "" {
		msg += " (" + t.Reason + ")"
	}
	if t.Message != "" {
		msg += ": " + t.Message
	}
	return msg + " — likely node preemption/eviction or cancellation; rerun to retry"
}
