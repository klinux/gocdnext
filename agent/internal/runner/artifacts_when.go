package runner

import (
	"context"
	"sync/atomic"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// shouldUploadArtifacts reports whether the runner should attempt the
// artifact upload given the job's `artifacts.when` policy and whether a task
// failed. Empty / "on_success" (the default) uploads only on a green job;
// "on_failure" only on a red one; "always" regardless.
//
// This is what lets a blocking scanner (exit-code 1 on a finding) still
// publish its SARIF: the job goes red, but with `artifacts.when: always`
// the report still ships, so the Security dashboard sees the very findings
// that failed the job — the run that matters most.
func shouldUploadArtifacts(when string, taskFailed bool) bool {
	switch when {
	case "on_failure":
		return taskFailed
	case "always":
		return true
	default: // "" == on_success
		return !taskFailed
	}
}

// uploadArtifactsOnFailure ships artifacts from a FAILED shared-mode job
// when `artifacts.when` opts into it (on_failure / always). It is
// best-effort: the job is already failing, so an upload error must not mask
// the real failure reason — it logs and returns whatever uploaded. Returns
// nil when the policy doesn't want a failure-time upload or nothing is
// declared.
func (r *Runner) uploadArtifactsOnFailure(ctx context.Context, scriptWorkDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) []*gocdnextv1.ArtifactRef {
	if !shouldUploadArtifacts(a.GetArtifactsWhen(), true) {
		return nil
	}
	if len(a.GetArtifactPaths()) == 0 && len(a.GetOptionalArtifactPaths()) == 0 {
		return nil
	}
	var refs []*gocdnextv1.ArtifactRef
	r.timedPhase(a, seq, "artifact upload (on failure)", func() {
		var err error
		refs, err = r.uploadArtifacts(ctx, scriptWorkDir, a, seq)
		if err != nil {
			// Best-effort on the failure path: don't turn a missing/failed
			// required artifact into a second, confusing failure — the job
			// already has its real reason. Ship what we can.
			r.cfg.Logger.Warn("runner: artifact upload on failed job (best-effort)",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
		}
	})
	return refs
}

// postJobArtifactsOnFailure is the isolated-mode counterpart: it ships
// artifacts from a FAILED job via the housekeeper (PostJob), with caches
// disabled (never cache a failed build). Same best-effort contract as the
// shared-mode helper. The caller must ensure the pod is still alive (the
// housekeeper exec fails otherwise) — skip it when the pod was disrupted.
func (r *Runner) postJobArtifactsOnFailure(ctx context.Context, exec engine.PodExecutor, podName, podWorkDir string, a *gocdnextv1.JobAssignment, seq *atomic.Int64) []*gocdnextv1.ArtifactRef {
	if !shouldUploadArtifacts(a.GetArtifactsWhen(), true) {
		return nil
	}
	if len(a.GetArtifactPaths()) == 0 && len(a.GetOptionalArtifactPaths()) == 0 {
		return nil
	}
	var refs []*gocdnextv1.ArtifactRef
	r.timedPhase(a, seq, "post-job artifacts (on failure)", func() {
		var err error
		refs, err = r.PostJob(ctx, PostJobConfig{
			Executor:      exec,
			Uploader:      r.cfg.IsolatedUploader,
			Cache:         nil, // never cache a failed build
			PodName:       podName,
			HousekeeperCt: "housekeeper",
			PodWorkDir:    podWorkDir,
		}, a, seq)
		if err != nil {
			r.cfg.Logger.Warn("runner: artifact upload on failed isolated job (best-effort)",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
		}
	})
	return refs
}
