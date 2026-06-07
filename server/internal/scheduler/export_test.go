package scheduler

import "github.com/gocdnext/gocdnext/server/internal/store"

// GroupNeedsOutputs exposes the pure grouping/validation helper for
// black-box tests in the scheduler_test package. The non-exported
// symbol lives in scheduler.go so the scheduler can call it
// without paying for the exported-API churn; the export here keeps
// the test surface narrow to what we want to lock down.
func GroupNeedsOutputs(rows []store.JobOutputs) (NeedsOutputs, error) {
	return groupNeedsOutputs(rows)
}
