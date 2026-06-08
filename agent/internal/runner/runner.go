// Package runner executes a JobAssignment end-to-end on the local host:
// clones the declared git materials, runs the shell scripts, streams the
// stdout/stderr lines back to the server as LogLine events, and finishes
// with a JobResult. Docker/plugin execution lands in a later slice.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

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

// Cancel signals the in-flight job with the given ID to stop. Returns
// true when a matching job was running (and its context was canceled),
// false when the job had already finished or never registered. Safe to
// call concurrently with Execute from the gRPC message dispatch loop.
// CleanupRunServices is the runner-side entry point for the
// server's CleanupRunServices RPC (handled in rpc/client.go).
// Delegates to the engine and translates emitted lifecycle
// events into ServiceLifecycle messages on the outbound gRPC
// channel so the server can stamp stopped_at on the
// service_runs row.
func (r *Runner) CleanupRunServices(ctx context.Context, runID string) (int, error) {
	emit := r.serviceLifecycleEmitter(runID)
	return r.cfg.Engine.CleanupRunServices(ctx, runID, emit)
}

// serviceLifecycleEmitter returns a callback that translates
// engine.ServiceLifecycleEvent into a ServiceLifecycle proto
// message and pushes it through cfg.Send (the outbound gRPC
// channel). nil Send → noop. Returns a fresh closure per call
// so a future per-run filter (e.g. dedup) can be added without
// rewriting both EnsureServices and CleanupRunServices callers.
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
			r.scanTestReports(ctx, scriptWorkDir, a, &seq)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d: %v", i, err))
			return
		}
		if exitCode != 0 {
			log.Info("runner: task exited non-zero", "task", i, "exit", exitCode)
			r.scanTestReports(ctx, scriptWorkDir, a, &seq)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d exited with %d", i, exitCode))
			return
		}
	}

	// Successful task loop — scan any declared test_reports and
	// ship them before the artifact upload so the server has the
	// per-case tally persisted by the time JobResult lands and
	// the cascade fires.
	r.scanTestReports(ctx, scriptWorkDir, a, &seq)

	// Cache store runs after every task succeeded — there's no
	// point caching a half-built node_modules from a failed
	// `pnpm install`. Same scriptWorkDir base as fetch so the
	// paths round-trip exactly. Failures log but don't block
	// the successful JobResult below.
	r.storeCaches(ctx, scriptWorkDir, a, &seq)

	// Artifact paths in YAML are repo-relative (user writes
	// `bin/gocdnext-agent` because that's where `go build` puts
	// it from the repo root). The script ran inside scriptWorkDir
	// (which follows the first checkout's target_dir), so that's
	// the correct base for resolving artifact paths — passing
	// workDir would miss the `src/<id>` checkout prefix and 404
	// on every single-material pipeline.
	refs, uploadErr := r.uploadArtifacts(ctx, scriptWorkDir, a, &seq)
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
func (r *Runner) downloadArtifact(ctx context.Context, workDir string, dl *gocdnextv1.ArtifactDownload, a *gocdnextv1.JobAssignment, seq *atomic.Int64) error {
	r.emitLog(a, seq, "stdout", fmt.Sprintf("$ download artifact %s (from %s)", dl.GetPath(), dl.GetFromJob()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl.GetGetUrl(), nil)
	if err != nil {
		return fmt.Errorf("build GET: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http GET: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET returned %s", resp.Status)
	}

	dest := dl.GetDest()
	if dest == "" {
		dest = "./"
	}
	destAbs := filepath.Join(workDir, dest)
	if err := UntarGz(destAbs, resp.Body, dl.GetContentSha256()); err != nil {
		return err
	}
	r.emitLog(a, seq, "stdout", fmt.Sprintf("  unpacked into %s", dest))
	return nil
}

// uploadArtifacts tars + uploads declared paths. Required paths
// (from `artifacts.paths:` in YAML) fail the job on any upload
// error — the YAML declared the file as a build output, so a
// missing file means the build didn't deliver what it promised.
// Optional paths (from `artifacts.optional:`) log on failure but
// don't surface an error, so flaky coverage/screenshot uploads
// never gate the build. Returns refs for everything that did
// upload successfully plus the first required-path error (if any).
func (r *Runner) uploadArtifacts(ctx context.Context, workDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) ([]*gocdnextv1.ArtifactRef, error) {
	if r.cfg.Uploader == nil {
		return nil, nil
	}
	var refs []*gocdnextv1.ArtifactRef

	if required := a.GetArtifactPaths(); len(required) > 0 {
		got, err := r.cfg.Uploader.Upload(ctx, workDir, a.GetRunId(), a.GetJobId(), required)
		if err != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf("artifact upload failed: %v", err))
			r.cfg.Logger.Warn("runner: required artifact upload failed", "err", err,
				"run_id", a.GetRunId(), "job_id", a.GetJobId())
			return got, err
		}
		for _, ref := range got {
			r.emitLog(a, seq, "stdout", fmt.Sprintf(
				"artifact uploaded: %s (%d bytes, sha256 %s)",
				ref.GetPath(), ref.GetSize(), ref.GetContentSha256()))
		}
		refs = append(refs, got...)
	}

	if optional := a.GetOptionalArtifactPaths(); len(optional) > 0 {
		got, err := r.cfg.Uploader.Upload(ctx, workDir, a.GetRunId(), a.GetJobId(), optional)
		if err != nil {
			// Optional semantics: log, carry on. The job still
			// succeeds if everything else did.
			r.emitLog(a, seq, "stderr", fmt.Sprintf(
				"optional artifact upload failed (continuing): %v", err))
			r.cfg.Logger.Warn("runner: optional artifact upload failed", "err", err,
				"run_id", a.GetRunId(), "job_id", a.GetJobId())
		} else {
			for _, ref := range got {
				r.emitLog(a, seq, "stdout", fmt.Sprintf(
					"optional artifact uploaded: %s (%d bytes, sha256 %s)",
					ref.GetPath(), ref.GetSize(), ref.GetContentSha256()))
			}
			refs = append(refs, got...)
		}
	}

	return refs, nil
}

func (r *Runner) checkout(ctx context.Context, workDir string, co *gocdnextv1.MaterialCheckout, a *gocdnextv1.JobAssignment, seq *atomic.Int64) error {
	target := filepath.Join(workDir, co.GetTargetDir())
	args := []string{"clone", "--quiet"}
	if co.GetBranch() != "" {
		args = append(args, "--branch", co.GetBranch())
	}
	args = append(args, co.GetUrl(), target)

	r.emitLog(a, seq, "stdout", fmt.Sprintf("$ git %v", args))
	code, err := r.runCommand(ctx, "", "git", args, nil, a, seq)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git clone exited %d", code)
	}
	if rev := co.GetRevision(); rev != "" {
		r.emitLog(a, seq, "stdout", "$ git -C "+target+" checkout "+rev)
		code, err := r.runCommand(ctx, "", "git", []string{"-C", target, "checkout", "--quiet", rev}, nil, a, seq)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("git checkout %s exited %d", rev, code)
		}
	}
	return nil
}

// runScript delegates the actual execution to the configured engine
// (Shell on the host for dev/local; Kubernetes for cluster deploys).
// The engine calls OnLine for each stdout/stderr line it sees; we
// turn those into LogLine protos via the same emitLog path used
// everywhere else (so masking + seq numbering remain centralised).
func (r *Runner) runScript(ctx context.Context, workDir, script, image string, docker bool, services servicePhase, env map[string]string, outputs outputsPaths, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (int, error) {
	r.emitLog(a, seq, "stdout", "$ "+script)
	return r.cfg.Engine.RunScript(ctx, engine.ScriptSpec{
		WorkDir:         workDir,
		Image:           image,
		Env:             env,
		Script:          script,
		Docker:          docker,
		Network:         services.network,
		HostAliases:     services.hostAliases,
		Resources:       assignmentResources(a),
		Profile:         a.GetProfile(),
		AgentTags:       append([]string(nil), r.cfg.AgentTags...),
		OutputsHostPath: outputs.host,
		OutputsRelPath:  outputs.rel,
		NodeSelector:    assignmentNodeSelector(a),
		Tolerations:     assignmentTolerations(a),
		OnLine: func(stream, text string) {
			r.emitLog(a, seq, stream, text)
		},
	})
}

// outputsPaths bundles the agent-chosen output file location so
// the engine can inject GOCDNEXT_OUTPUT_FILE at the right path
// (host or container) without us blowing up the runScript /
// runPlugin signatures further. Both fields empty when the job
// declared no outputs.
type outputsPaths struct {
	host string // absolute host path the agent reads after the task
	rel  string // workspace-relative path the container script sees
}

// assignmentResources lifts the proto ResourceRequirements into the
// engine-level Resources struct. Returns the zero value when the
// proto carries nothing — the engine treats nil and zero identically
// (fall through to its own defaults).
func assignmentResources(a *gocdnextv1.JobAssignment) engine.Resources {
	r := a.GetResources()
	if r == nil {
		return engine.Resources{}
	}
	return engine.Resources{
		CPURequest:    r.GetCpuRequest(),
		CPULimit:      r.GetCpuLimit(),
		MemoryRequest: r.GetMemoryRequest(),
		MemoryLimit:   r.GetMemoryLimit(),
	}
}

// assignmentNodeSelector copies the proto map into a fresh map so the
// engine can't mutate the proto-owned memory. Empty input → nil so
// the engine's "absent + nil identical" contract holds.
func assignmentNodeSelector(a *gocdnextv1.JobAssignment) map[string]string {
	in := a.GetNodeSelector()
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// assignmentTolerations converts the proto Toleration list into the
// engine-level corev1.Toleration slice the Kubernetes engine drops
// straight onto the PodSpec. TolerationSeconds is COPIED into a
// fresh *int64 so engine mutation can't leak back into the proto
// (same aliasing discipline as scheduler.tolerationsToProto).
func assignmentTolerations(a *gocdnextv1.JobAssignment) []corev1.Toleration {
	in := a.GetTolerations()
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, len(in))
	for i, t := range in {
		out[i] = corev1.Toleration{
			Key:      t.GetKey(),
			Operator: corev1.TolerationOperator(t.GetOperator()),
			Value:    t.GetValue(),
			Effect:   corev1.TaintEffect(t.GetEffect()),
		}
		if t.TolerationSeconds != nil {
			v := *t.TolerationSeconds
			out[i].TolerationSeconds = &v
		}
	}
	return out
}

// runCommand executes a command and streams stdout/stderr as LogLines. Returns
// the exit code (0 on success) and an error ONLY for lifecycle problems (fork
// failed, unexpected wait error). A non-zero exit code is NOT an error.
func (r *Runner) runCommand(ctx context.Context, dir, name string, args []string, env map[string]string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(env)...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go r.streamLines(stdout, "stdout", a, seq, &wg)
	go r.streamLines(stderr, "stderr", a, seq, &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

func (r *Runner) streamLines(rd io.Reader, stream string, a *gocdnextv1.JobAssignment, seq *atomic.Int64, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(rd)
	// Raise buffer size: long `go test -v` lines or minified JS can blow past
	// the default 64 KiB.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		r.emitLog(a, seq, stream, scanner.Text())
	}
	// Scanner errors (e.g., pipe close) are ignored: the Wait() below sees the
	// real process exit which is the authoritative outcome.
}

func (r *Runner) emitLog(a *gocdnextv1.JobAssignment, seq *atomic.Int64, stream, text string) {
	n := seq.Add(1)
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Log{
			Log: &gocdnextv1.LogLine{
				RunId:  a.GetRunId(),
				JobId:  a.GetJobId(),
				Seq:    n,
				At:     timestamppb.New(time.Now().UTC()),
				Stream: stream,
				Text:   applyMasks(text, a.GetLogMasks()),
			},
		},
	})
}

// applyMasks replaces every occurrence of a secret value with "***". Masks
// of length < 4 are ignored so common short words don't accidentally get
// replaced (e.g. "a", "go", "and"). Multi-line values are matched per-line
// only — the scanner splits output on newlines before we see it, so a long
// PEM key might not be fully masked line-by-line. Known limit; the secret
// is still in the job's env either way.
func applyMasks(text string, masks []string) string {
	for _, m := range masks {
		if len(m) < 4 {
			continue
		}
		text = strings.ReplaceAll(text, m, "***")
	}
	return text
}

func (r *Runner) sendResult(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string) {
	r.sendResultWithArtifacts(a, status, exitCode, errMsg, nil)
}

func (r *Runner) sendResultWithArtifacts(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string, refs []*gocdnextv1.ArtifactRef) {
	r.sendResultWithArtifactsAndOutputs(a, status, exitCode, errMsg, refs, nil)
}

// sendResultWithArtifactsAndOutputs is the canonical result sender —
// the other two wrappers exist for call-site readability. Outputs
// (issue #10) is alias → value, already filtered + validated by
// parseOutputsFile against the job's declarations.
func (r *Runner) sendResultWithArtifactsAndOutputs(a *gocdnextv1.JobAssignment, status gocdnextv1.RunStatus, exitCode int32, errMsg string, refs []*gocdnextv1.ArtifactRef, outputs map[string]string) {
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Result{
			Result: &gocdnextv1.JobResult{
				RunId:     a.GetRunId(),
				JobId:     a.GetJobId(),
				Status:    status,
				ExitCode:  exitCode,
				Error:     errMsg,
				Artifacts: refs,
				Outputs:   outputs,
			},
		},
	})
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// sanitize strips path separators so a hostile run_id/job_id can't escape
// the workspace root. run_ids and job_ids are UUIDs in production, but tests
// and future manual triggers may pass arbitrary strings.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', '.', 0:
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
