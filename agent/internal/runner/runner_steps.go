package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

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
