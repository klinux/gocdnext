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

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/agent/internal/engine"
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

	// Engine executes each script task. Nil defaults to engine.Shell
	// — the pre-F3 behaviour (`sh -c` on the agent host). K8s-native
	// deployments set engine.Kubernetes.
	Engine engine.Engine

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
func (r *Runner) Execute(ctx context.Context, a *gocdnextv1.JobAssignment) {
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

	for i, task := range a.GetTasks() {
		script := task.GetScript()
		if script == "" {
			// Plugin tasks aren't supported by the local runner yet — skip with
			// a log line so the user sees why nothing happened.
			r.emitLog(a, &seq, "stderr", fmt.Sprintf("task %d: plugin step skipped (local runner MVP)", i))
			continue
		}
		exitCode, err := r.runScript(ctx, scriptWorkDir, script, a.GetImage(), a.GetDocker(), a.GetEnv(), a, &seq)
		if err != nil {
			log.Warn("runner: script error", "err", err, "task", i)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d: %v", i, err))
			return
		}
		if exitCode != 0 {
			log.Info("runner: task exited non-zero", "task", i, "exit", exitCode)
			r.sendResult(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, int32(exitCode),
				fmt.Sprintf("task %d exited with %d", i, exitCode))
			return
		}
	}

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
		r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_FAILED, 1,
			fmt.Sprintf("artifact upload failed: %v", uploadErr), refs)
		return
	}

	log.Info("runner: execute ok", "artifacts", len(refs))
	r.sendResultWithArtifacts(a, gocdnextv1.RunStatus_RUN_STATUS_SUCCESS, 0, "", refs)
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
func (r *Runner) runScript(ctx context.Context, workDir, script, image string, docker bool, env map[string]string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (int, error) {
	r.emitLog(a, seq, "stdout", "$ "+script)
	return r.cfg.Engine.RunScript(ctx, engine.ScriptSpec{
		WorkDir: workDir,
		Image:   image,
		Env:     env,
		Script:  script,
		Docker:  docker,
		OnLine: func(stream, text string) {
			r.emitLog(a, seq, stream, text)
		},
	})
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
	r.cfg.Send(&gocdnextv1.AgentMessage{
		Kind: &gocdnextv1.AgentMessage_Result{
			Result: &gocdnextv1.JobResult{
				RunId:     a.GetRunId(),
				JobId:     a.GetJobId(),
				Status:    status,
				ExitCode:  exitCode,
				Error:     errMsg,
				Artifacts: refs,
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
