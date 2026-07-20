package scheduler

import (
	"context"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// GroupNeedsOutputs exposes the pure grouping/validation helper for
// black-box tests in the scheduler_test package. The non-exported
// symbol lives in scheduler.go so the scheduler can call it
// without paying for the exported-API churn; the export here keeps
// the test surface narrow to what we want to lock down.
func GroupNeedsOutputs(rows []store.JobOutputs) (NeedsOutputs, MatrixNeedsOutputs, error) {
	return groupNeedsOutputs(rows)
}

// SubstituteNeedsRefs exposes the pre-pass substitution for
// black-box refs tests covering both bare and matrix-selector
// forms (issues #10 + #21).
func SubstituteNeedsRefs(s string, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames) (string, error) {
	return substituteNeedsRefs(s, needs, matrix, dims)
}

// SubstituteNeedsRefsMap is the map-valued lift; same contract.
func SubstituteNeedsRefsMap(in map[string]string, needs NeedsOutputs, matrix MatrixNeedsOutputs, dims MatrixDimNames) (map[string]string, error) {
	return substituteNeedsRefsMap(in, needs, matrix, dims)
}

// ClusterDispatchError exposes the pure oracle-collapsing helper for the
// dispatch-time `cluster:` error path (#155) so black-box tests can lock the
// collapse without spinning the full scheduler.
func ClusterDispatchError(err error) (string, bool) {
	return clusterDispatchError(err)
}

// MintIDTokensForTest exposes the dispatch-path id_token resolution
// for black-box tests: fast-path gating, claims construction per
// cause, and the issuer-disabled configuration error.
func MintIDTokensForTest(s *Scheduler, ctx context.Context, run store.RunForDispatch, job store.DispatchableJob) (map[string]string, error) {
	return s.mintIDTokens(ctx, run, job)
}
