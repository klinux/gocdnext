package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func (r *Runner) Execute(ctx context.Context, a *gocdnextv1.JobAssignment) {
	if k, ok := r.cfg.Engine.(*engine.Kubernetes); ok && k.Config().WorkspaceMode == engine.WorkspaceModeIsolated {
		r.executeIsolated(ctx, a, k)
		return
	}
	// Derive a cancelable context scoped to this one job and register
	// it so the gRPC side can `Cancel(jobID)` mid-run. Deferred
	// deregister ensures a late Cancel after the job finished is a
	// no-op instead of racing with a newer job on the same ID.
	ctx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	r.registerInflight(a.GetJobId(), cancelJob)
	defer r.deregisterInflight(a.GetJobId())

	log := r.cfg.Logger.With("run_id", a.GetRunId(), "job_id", a.GetJobId(), "name", a.GetName())
	log.Info("runner: execute start", "tasks", len(a.GetTasks()), "checkouts", len(a.GetCheckouts()))

	workDir := filepath.Join(r.cfg.WorkspaceRoot, sanitize(a.GetRunId()), sanitize(a.GetJobId()))
	// Clean any leftover workspace from a prior attempt — if the previous
	// agent was kill-9'd the deferred RemoveAll never ran, and a half-cloned
	// source tree makes the next `git clone` fail with "destination exists".
	if err := os.RemoveAll(workDir); err != nil {
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, "workspace cleanup: "+err.Error())
		return
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1, "workspace: "+err.Error())
		return
	}
	defer func() {
		if !r.cfg.KeepWorkspace {
			_ = os.RemoveAll(workDir)
		}
	}()

	var seq atomic.Int64

	// scriptWorkDir is where user scripts run. Defaults to workspace
	// root when there are no checkouts (plugin-only jobs), otherwise
	// follows the first checkout into its target dir so relative
	// paths in the user's script match the layout of the repo they
	// cloned. Multi-material pipelines reach sibling checkouts via
	// `../<other-target>` — the first is the de-facto "primary".
	scriptWorkDir := workDir
	for i, co := range a.GetCheckouts() {
		if err := r.checkout(ctx, workDir, co, a, &seq); err != nil {
			log.Warn("runner: checkout failed", "err", err, "url", co.GetUrl())
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1,
				fmt.Sprintf("checkout %s: %v", co.GetUrl(), err))
			return
		}
		if i == 0 && co.GetTargetDir() != "" {
			scriptWorkDir = filepath.Join(workDir, co.GetTargetDir())
		}
	}

	// Dependency artefacts from upstream jobs land BEFORE tasks run —
	// the whole point of the download is to feed `script:`. A bad
	// download (http error, sha mismatch, tar escape) fails the job
	// cleanly with the producing job's name so the user knows where to
	// look. We don't mask the error message here; artefact paths are
	// declared config, not secrets.
	//
	// Extract into scriptWorkDir — matches the base the scripts run
	// in AND the base the producer used when taring paths up. Using
	// plain workDir here landed node_modules at
	// <workDir>/web/node_modules while `cd web` put scripts inside
	// <workDir>/<target>/web, so node_modules looked missing.
	for _, dl := range a.GetArtifactDownloads() {
		if err := r.downloadArtifact(ctx, scriptWorkDir, dl, a, &seq); err != nil {
			log.Warn("runner: artifact download failed",
				"err", err, "path", dl.GetPath(), "from", dl.GetFromJob())
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1,
				fmt.Sprintf("artifact %s (from %s): %v", dl.GetPath(), dl.GetFromJob(), err))
			return
		}
	}

	// Pipeline services: each engine brings them up in its own
	// runtime (docker → network + containers, k8s → pod-per-service
	// + hostAliases) via engine.EnsureServices. The wireup result
	// flows into ScriptSpec downstream — Network for docker, the
	// hostAliases list for k8s. Cleanup is deferred so it runs on
	// task failure, cancel, or successful exit alike.
	servicesPhase, svcErr := r.startServices(ctx, a, &seq)
	if svcErr != nil {
		log.Warn("runner: services startup failed", "err", svcErr)
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1,
			fmt.Sprintf("services: %v", svcErr))
		return
	}
	defer servicesPhase.cleanup()

	// Expand `{{ hash "..." }}` templates on cache keys BEFORE
	// fetchCaches reads them. Workspace is materialised (checkout
	// + artifact downloads already ran), so glob+sha256 resolves
	// against the real tree. Plain literal keys (no `{{`) take
	// the no-op fast path — see expandCacheKeys.
	//
	// Fail-loud: a parse error (server-agent skew) or zero-match
	// glob aborts the job. A silent "couldn't expand the key" would
	// let two distinct lockfiles land on the same cache key — the
	// exact regression the feature is shipping to prevent.
	if err := r.expandCacheKeys(ctx, scriptWorkDir, a, &seq); err != nil {
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1,
			"cache key expansion: "+err.Error())
		return
	}

	// Cache fetch happens AFTER services come up (so a cache-hit
	// doesn't block on a broken postgres sidecar) but BEFORE tasks
	// run — the whole point of the cache is to pre-populate the
	// dirs the scripts are about to touch. Misses and transport
	// errors log but never escalate: cache is acceleration, not
	// correctness.
	r.fetchCaches(ctx, scriptWorkDir, a, &seq)

	// Outputs (issue #10): when the job's YAML declared an
	// `outputs:` block, prepare an empty file under
	// .gocdnext/outputs/<short-id>.env. The runner does NOT inject
	// GOCDNEXT_OUTPUT_FILE into the env directly — the engine does
	// that inside RunScript, because only the engine knows whether
	// the task will containerize (and which mount it'll use) or
	// fall back to host execution (Docker→Shell fallback). Without
	// this split, a Docker job with no `image:` would receive
	// `/workspace/...` as env value and then write to a path that
	// doesn't exist on the host. The shape: runner sets
	// OutputsHostPath + OutputsRelPath on the spec; engine picks
	// the right value when constructing the task env.
	//
	// Empty declarations → skip the whole path; spec fields stay
	// "" and engines see no outputs request.
	taskEnv := a.GetEnv()
	var outputsHostPath string
	var outputs outputsPaths
	if len(a.GetOutputs()) > 0 {
		var err error
		outputsHostPath, err = prepareOutputsFile(scriptWorkDir, a.GetJobId())
		if err != nil {
			log.Warn("runner: prepare outputs file", "err", err)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, -1,
				"outputs file prep: "+err.Error())
			return
		}
		outputs.host = outputsHostPath
		outputs.rel = filepath.Join(outputsRelDir, shortJobID(a.GetJobId())+".env")
	}

	tasksStart := time.Now()
	for i, task := range a.GetTasks() {
		var (
			exitCode int
			err      error
		)
		if plugin := task.GetPlugin(); plugin != nil {
			// Plugin task: run the plugin's own container image
			// with PLUGIN_* env vars derived from `with:` settings.
			// No user script — the image's ENTRYPOINT IS the logic
			// (Woodpecker's model: plugins are regular containers,
			// their contract is "look at PLUGIN_FOO env, do X").
			exitCode, err = r.runPlugin(ctx, scriptWorkDir, plugin, servicesPhase, taskEnv, outputs, a, &seq)
		} else {
			script := task.GetScript()
			if script == "" {
				// Neither script nor plugin — malformed task. Skip
				// with a loud log so the operator notices instead
				// of watching the job succeed silently.
				r.emitLog(a, &seq, "stderr", fmt.Sprintf("task %d: empty task (no script, no plugin); skipping", i))
				continue
			}
			exitCode, err = r.runScript(ctx, scriptWorkDir, script, a.GetImage(), a.GetDocker(), servicesPhase, taskEnv, outputs, a, &seq)
		}
		if err != nil {
			log.Warn("runner: task error", "err", err, "task", i)
			// Tests often produce reports even on failure — a test
			// runner exits non-zero precisely BECAUSE cases failed,
			// but the XML file it wrote is what the Tests tab needs
			// to render the per-case breakdown. Scan before reporting
			// so failed runs surface their evidence.
			r.emitPhase(a, &seq, fmt.Sprintf("tasks failed after %s (task %d: error)", phaseDur(tasksStart), i))
			r.scanTestReports(ctx, scriptWorkDir, a, &seq)
			r.scanCoverage(scriptWorkDir, a, &seq)
			// artifacts.when: on_failure/always still ship on a red job so a
			// blocking scanner's SARIF reaches the dashboard.
			refs := r.uploadArtifactsOnFailure(ctx, scriptWorkDir, a, &seq)
			r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d: %v", i, err), refs, nil)
			return
		}
		if exitCode != 0 {
			log.Info("runner: task exited non-zero", "task", i, "exit", exitCode)
			r.emitPhase(a, &seq, fmt.Sprintf("tasks failed after %s (task %d, exit %d)", phaseDur(tasksStart), i, exitCode))
			r.scanTestReports(ctx, scriptWorkDir, a, &seq)
			r.scanCoverage(scriptWorkDir, a, &seq)
			refs := r.uploadArtifactsOnFailure(ctx, scriptWorkDir, a, &seq)
			r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d exited with %d", i, exitCode), refs, nil)
			return
		}
	}

	// Phase boundary marker: the "4 silent minutes" class of
	// confusion (operator-reported) was a job whose tasks went
	// quiet and whose post-task phases were invisible — every
	// boundary now prints, and the UI's per-line elapsed does the
	// attribution.
	r.emitPhase(a, &seq, fmt.Sprintf("tasks completed in %s", phaseDur(tasksStart)))

	// Successful task loop — scan any declared test_reports and
	// ship them before the artifact upload so the server has the
	// per-case tally persisted by the time JobResult lands and
	// the cascade fires.
	r.scanTestReports(ctx, scriptWorkDir, a, &seq)
	if gateFailed, reason := r.scanCoverage(scriptWorkDir, a, &seq); gateFailed {
		// fail_under: the build is functionally green but the
		// declared coverage floor wasn't met — the job fails like
		// any other failed contract (no cache store, no artifacts,
		// mirroring the task-failure path).
		r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1, reason)
		return
	}

	// Cache store runs after every task succeeded — there's no
	// point caching a half-built node_modules from a failed
	// `pnpm install`. Same scriptWorkDir base as fetch so the
	// paths round-trip exactly. Failures log but don't block
	// the successful JobResult below.
	// Marker gated on the SAME condition storeCaches acts on
	// (declared entries AND a wired client) — an agent without a
	// CacheClient must stay byte-silent about caches.
	if len(a.GetCaches()) > 0 && r.cfg.Cache != nil {
		r.timedPhase(a, &seq, fmt.Sprintf("cache store (%d entr%s)", len(a.GetCaches()), plural(len(a.GetCaches()), "y", "ies")), func() {
			r.storeCaches(ctx, scriptWorkDir, a, &seq)
		})
	}

	// Artifact paths in YAML are repo-relative (user writes
	// `bin/gocdnext-agent` because that's where `go build` puts
	// it from the repo root). The script ran inside scriptWorkDir
	// (which follows the first checkout's target_dir), so that's
	// the correct base for resolving artifact paths — passing
	// workDir would miss the `src/<id>` checkout prefix and 404
	// on every single-material pipeline.
	var refs []*gocdnextv1.ArtifactRef
	var uploadErr error
	// On success we upload unless artifacts.when is on_failure (which wants
	// the upload only on a red job — handled in the failure branches above).
	if (len(a.GetArtifactPaths()) > 0 || len(a.GetOptionalArtifactPaths()) > 0) &&
		shouldUploadArtifacts(a.GetArtifactsWhen(), false) {
		r.timedPhase(a, &seq, "artifact upload", func() {
			refs, uploadErr = r.uploadArtifacts(ctx, scriptWorkDir, a, &seq)
		})
	}
	if uploadErr != nil {
		// Required artifact upload failed — the YAML declared the
		// file as a build output so a missing file means the build
		// didn't deliver what it promised. Fail the job loudly
		// instead of the silent "success with missing artifact" the
		// old best-effort behaviour allowed.
		log.Warn("runner: required artifact upload failed",
			"err", uploadErr, "run_id", a.GetRunId(), "job_id", a.GetJobId())
		r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
			fmt.Sprintf("artifact upload failed: %v", uploadErr), refs, nil)
		return
	}

	// Outputs (issue #10): when the job declared an `outputs:` block,
	// parse the file the plugin wrote. Parse failures (oversized,
	// bad shape, missing declared key) fail the job — output is
	// part of the build's CONTRACT for downstream jobs that depend
	// on it. Better to fail the producer here than have downstream
	// fail later with a confusing missing-reference error.
	//
	// outputsHostPath is "" when the job declared no outputs (we
	// skipped the path entirely above); the parser also no-ops on
	// nil declared map, but the early return keeps the success path
	// clean.
	var producedOutputs map[string]string
	if outputsHostPath != "" {
		out, err := parseOutputsFile(outputsHostPath, a.GetOutputs())
		if err != nil {
			log.Warn("runner: outputs parse failed",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			// We deliberately do NOT log the file's contents — the
			// failure message includes the line number / key name
			// for the operator to diagnose, never the value (which
			// might be a digest/token).
			r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
				"outputs: "+err.Error(), refs, nil)
			return
		}
		producedOutputs = out
	}

	// Log the OUTPUT KEYS at info but NEVER the values — values may
	// be tokens, digests, or other content that the operator marked
	// as needs-output for substitution, not for log inspection.
	outputKeys := make([]string, 0, len(producedOutputs))
	for k := range producedOutputs {
		outputKeys = append(outputKeys, k)
	}
	log.Info("runner: execute ok",
		"artifacts", len(refs),
		"output_keys", outputKeys)
	r.sendResultWithArtifactsAndOutputs(a, gocdnextv1.RunStatus_RUN_STATUS_SUCCESS, 0, "", refs, producedOutputs)
}

// downloadArtifact pulls a single upstream artefact, verifies its
// sha256, and untars it into `dl.dest` (relative to the workspace).
// net/http is plenty for the signed URL fetch — no retry yet; we fail
// the job and let the reaper re-queue.
