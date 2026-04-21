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
// its own workspace subdirectory. No per-runner state mutates.
type Runner struct {
	cfg Config
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
	return &Runner{cfg: cfg}
}

// Execute runs the assignment to completion: checkout each material, run
// each script task until one fails, emit a JobResult. Never panics on task
// failure — exit != 0 and checkout errors both resolve to RUN_STATUS_FAILED.
func (r *Runner) Execute(ctx context.Context, a *gocdnextv1.JobAssignment) {
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
	for _, dl := range a.GetArtifactDownloads() {
		if err := r.downloadArtifact(ctx, workDir, dl, a, &seq); err != nil {
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
		exitCode, err := r.runScript(ctx, scriptWorkDir, script, a.GetImage(), a.GetEnv(), a, &seq)
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

	refs := r.uploadArtifacts(ctx, workDir, a, &seq)

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

// uploadArtifacts tars + uploads each declared path. Errors are
// reported as log lines but do NOT fail the job (consistent with
// GitLab CI — the contract is that artifacts are best-effort after a
// successful task sequence). Downstream jobs that `needs_artifacts`
// will fail cleanly when the server reports the missing row.
func (r *Runner) uploadArtifacts(ctx context.Context, workDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) []*gocdnextv1.ArtifactRef {
	if r.cfg.Uploader == nil || len(a.GetArtifactPaths()) == 0 {
		return nil
	}
	refs, err := r.cfg.Uploader.Upload(ctx, workDir, a.GetRunId(), a.GetJobId(), a.GetArtifactPaths())
	if err != nil {
		r.emitLog(a, seq, "stderr", fmt.Sprintf("artifact upload failed: %v", err))
		r.cfg.Logger.Warn("runner: artifact upload failed", "err", err,
			"run_id", a.GetRunId(), "job_id", a.GetJobId())
		return nil
	}
	for _, ref := range refs {
		r.emitLog(a, seq, "stdout", fmt.Sprintf(
			"artifact uploaded: %s (%d bytes, sha256 %s)",
			ref.GetPath(), ref.GetSize(), ref.GetContentSha256()))
	}
	return refs
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
func (r *Runner) runScript(ctx context.Context, workDir, script, image string, env map[string]string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) (int, error) {
	r.emitLog(a, seq, "stdout", "$ "+script)
	return r.cfg.Engine.RunScript(ctx, engine.ScriptSpec{
		WorkDir: workDir,
		Image:   image,
		Env:     env,
		Script:  script,
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
