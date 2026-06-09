// Package runner — postjob.go owns the post-task work for an
// isolated-mode job. Once the task container has terminated, the
// agent process (not the pod) drives:
//
//   - Required + optional artifact uploads via the existing
//     gRPC RequestArtifactUpload dance plus a tar streamed from
//     the housekeeper sidecar via PodExecutor.
//   - Cache store for literal keys: RequestCachePut → exec tar
//     inside the housekeeper → PUT → MarkCacheReady, mirroring
//     the artifact upload pattern. Templated keys are still
//     skipped (workspace-side hashing required at expand time;
//     the agent doesn't see the runtime key).
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// IsolatedUploader is the post-task upload contract for isolated
// mode: tar files from inside a job pod (via exec) and stream
// them to a signed PUT URL. Mirrors the shared-mode
// ArtifactUploader interface — same gRPC dance for tickets, the
// transport is what changes.
//
// Kept as a separate interface from ArtifactUploader so existing
// test mocks that only need Upload don't have to grow a new
// method. The concrete rpc.ArtifactUploader implements both.
type IsolatedUploader interface {
	UploadFromPod(
		ctx context.Context,
		exec engine.PodExecutor,
		podName, containerName, podWorkDir string,
		runID, jobID string,
		paths []string,
	) ([]*gocdnextv1.ArtifactRef, error)
}

// PostJobConfig packs the inputs PostJob needs without forcing
// the caller to thread a long parameter list.
type PostJobConfig struct {
	Executor      engine.PodExecutor
	Uploader      IsolatedUploader
	Cache         IsolatedCacheClient // nil → cache store no-op
	PodName       string
	HousekeeperCt string // typically "housekeeper"
	PodWorkDir    string // mount path inside the pod, typically /workspace
}

// PostJob runs the post-task phase for an isolated-mode job and
// returns the artefact refs that were successfully uploaded plus
// the first error from a REQUIRED path upload (optional path
// failures log but don't surface). Caches are emitted as a warn
// line if declared (no-op in v0.5.0 isolated mode).
//
// Caller responsibility: ensure the pod is still alive (task
// container terminated, housekeeper still running) before calling.
// PodExecutor will return an error if the housekeeper container
// has already exited.
func (r *Runner) PostJob(
	ctx context.Context,
	cfg PostJobConfig,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) ([]*gocdnextv1.ArtifactRef, error) {
	if cfg.Uploader == nil {
		// No uploader wired → no-op (matches shared-mode behaviour
		// when Config.Uploader is nil — job succeeds with no refs).
		return nil, nil
	}

	var refs []*gocdnextv1.ArtifactRef

	if required := a.GetArtifactPaths(); len(required) > 0 {
		got, err := cfg.Uploader.UploadFromPod(
			ctx, cfg.Executor, cfg.PodName, cfg.HousekeeperCt, cfg.PodWorkDir,
			a.GetRunId(), a.GetJobId(), required)
		refs = append(refs, got...)
		for _, ref := range got {
			r.emitLog(a, seq, "stdout", fmt.Sprintf(
				"artifact uploaded: %s (%d bytes, sha256 %s)",
				ref.GetPath(), ref.GetSize(), ref.GetContentSha256()))
		}
		if err != nil {
			r.emitLog(a, seq, "stderr", fmt.Sprintf("artifact upload failed: %v", err))
			r.cfg.Logger.Warn("runner: required artifact upload failed (isolated)",
				"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			return refs, err
		}
	}

	if optional := a.GetOptionalArtifactPaths(); len(optional) > 0 {
		got, err := cfg.Uploader.UploadFromPod(
			ctx, cfg.Executor, cfg.PodName, cfg.HousekeeperCt, cfg.PodWorkDir,
			a.GetRunId(), a.GetJobId(), optional)
		if err != nil {
			// Distinguish "files don't exist" (the OPTIONAL contract:
			// "if it's not there, no problem") from "real transport
			// failure" (network, RPC, exec error). The first is a
			// neutral info line; the second is a warn-level "failed".
			// Pre-v0.14.8 both used the alarming "failed (continuing)"
			// shape, which scared operators who had legitimately
			// declared an optional Jacoco/screenshot upload that the
			// run didn't produce.
			var missing *podfs.PathsMissingError
			if errors.As(err, &missing) {
				r.emitLog(a, seq, "stdout", fmt.Sprintf(
					"optional artifact: no files matched %s",
					strings.Join(missing.Paths, ", ")))
			} else {
				r.emitLog(a, seq, "stderr", fmt.Sprintf(
					"optional artifact upload failed (continuing): %v", err))
				r.cfg.Logger.Warn("runner: optional artifact upload failed (isolated)",
					"err", err, "run_id", a.GetRunId(), "job_id", a.GetJobId())
			}
		} else {
			for _, ref := range got {
				r.emitLog(a, seq, "stdout", fmt.Sprintf(
					"optional artifact uploaded: %s (%d bytes, sha256 %s)",
					ref.GetPath(), ref.GetSize(), ref.GetContentSha256()))
			}
			refs = append(refs, got...)
		}
	}

	if cfg.Cache != nil {
		for _, entry := range a.GetCaches() {
			if entry.GetKey() == "" {
				continue
			}
			if strings.Contains(entry.GetKey(), "{{") {
				// Templated key — agent couldn't pre-expand,
				// the runtime key is unknown here. Skip with
				// a warning; matches the prep-side warning so
				// the operator sees a consistent message at
				// both ends.
				r.emitLog(a, seq, "stderr", fmt.Sprintf(
					"cache %q: templated key not yet supported in "+
						"isolated mode; store skipped", entry.GetKey()))
				continue
			}
			if len(entry.GetPaths()) == 0 {
				continue
			}
			if err := cfg.Cache.StoreFromPod(ctx, cfg.Executor,
				cfg.PodName, cfg.HousekeeperCt, cfg.PodWorkDir,
				a.GetRunId(), a.GetJobId(), entry); err != nil {
				// Best-effort: log and continue. The build
				// succeeded; the next run will simply rebuild
				// instead of restoring.
				r.emitLog(a, seq, "stderr", fmt.Sprintf(
					"cache %q: store failed (%v) — next run will rebuild",
					entry.GetKey(), err))
				continue
			}
			r.emitLog(a, seq, "stdout", fmt.Sprintf(
				"cache %q: stored", entry.GetKey()))
		}
	}

	return refs, nil
}
