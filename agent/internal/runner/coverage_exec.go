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
) (bool, string) {
	spec := a.GetCoverageReport()
	if spec == nil {
		return false, ""
	}
	gated := spec.GetFailUnder() > 0
	if exec == nil {
		msg := "coverage_report: no pod executor wired; skipping"
		r.emitLog(a, seq, "stderr", msg)
		if gated {
			return true, msg + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	full := path.Join(workDir, spec.GetPath())
	raw, warn := catPodFileN(ctx, exec, podName, container, full, maxCoverageFileBytes)
	if warn != "" {
		r.emitLog(a, seq, "stderr", "coverage_report: "+warn)
		// Same bypass-proofing as shared mode: a gate that can't
		// read its evidence fails.
		if gated {
			return true, "coverage_report: " + warn + " (fail_under gate cannot be evaluated)"
		}
		return false, ""
	}
	return r.parseAndSendCoverage(spec, raw, a, seq)
}
