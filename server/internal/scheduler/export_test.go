package scheduler

import "github.com/gocdnext/gocdnext/server/internal/store"

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
