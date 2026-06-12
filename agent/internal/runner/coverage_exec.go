// Package runner — coverage_exec.go is the workspace-isolated
// variant of scanCoverage: the declared file is read out of the
// pod's housekeeper via exec (same transport test_reports uses),
// parsed agent-side, and only the summary ships.
package runner

import (
	"context"
	"path"
	"sync/atomic"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

func (r *Runner) scanCoverageFromPod(
	ctx context.Context,
	exec engine.PodExecutor,
	podName, container, workDir string,
	a *gocdnextv1.JobAssignment,
	seq *atomic.Int64,
) {
	spec := a.GetCoverageReport()
	if spec == nil {
		return
	}
	if exec == nil {
		r.emitLog(a, seq, "stderr", "coverage_report: no pod executor wired; skipping")
		return
	}
	full := path.Join(workDir, spec.GetPath())
	raw, warn := catPodFileN(ctx, exec, podName, container, full, maxCoverageFileBytes)
	if warn != "" {
		r.emitLog(a, seq, "stderr", "coverage_report: "+warn)
		return
	}
	r.parseAndSendCoverage(spec, raw, a, seq)
}
