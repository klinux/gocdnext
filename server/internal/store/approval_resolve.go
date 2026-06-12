package store

import (
	"encoding/json"
	"sort"
)

// resolveEffectiveQuorum resolves the gate quorum that should be
// persisted in job_runs.approval_required at run materialisation
// time, taking PR labels into account.
//
// Inputs:
//   - cause:       the run's cause string (only "pull_request" can
//     carry PR labels; any other cause short-circuits
//     to baseline)
//   - causeDetail: raw JSONB of the run's cause_detail; expected
//     to carry `pr_labels: []string` when applicable
//   - baseRequired: the pipeline's declared approval.required
//     (already defaulted to 1 by the parser)
//   - overrides:   the job's approval.quorum_by_label map
//     (lowercased keys); nil/empty short-circuits
//
// Returns:
//   - effectiveQuorum: the quorum the state-machine should see.
//     Falls back to baseRequired when no override fires.
//   - label: the PR label whose override won, or "" when no
//     override fired. Persisted to job_runs.approval_quorum_label
//     so the UI + audit log can explain "this gate is X because
//     of label Y, not the pipeline default Z".
//
// Resolution:
//   - Multiple labels match: MAX override wins (two reasons to
//     demand more quorum shouldn't cancel each other).
//   - Ties: lexicographically smallest label wins. Determinism is
//     load-bearing for audit log clarity + reproducible tests.
//   - Malformed cause_detail JSON / missing pr_labels / empty
//     pr_labels: silent fall-back to baseline. Failing closed
//     (require baseline approvers, the strict default) is the
//     safe direction; failing open (auto-pass on a parse glitch)
//     would defeat the gate.
func resolveEffectiveQuorum(
	cause string,
	causeDetail json.RawMessage,
	baseRequired int,
	overrides map[string]int,
) (effectiveQuorum int, label string) {
	if len(overrides) == 0 {
		return baseRequired, ""
	}
	if cause != "pull_request" || len(causeDetail) == 0 {
		return baseRequired, ""
	}
	var d struct {
		Labels []string `json:"pr_labels"`
	}
	if err := json.Unmarshal(causeDetail, &d); err != nil {
		return baseRequired, ""
	}
	if len(d.Labels) == 0 {
		return baseRequired, ""
	}

	// Walk labels in lex order so ties resolve deterministically:
	// scanning sorted labels and replacing only on strictly-greater
	// override means the smallest-named label survives among ties.
	// Sort a copy — labels come from cause_detail (snapshot, write-
	// once) but we don't want to mutate JSON-decoded slices on the
	// off chance a future caller re-reads from the same decode.
	sorted := append(make([]string, 0, len(d.Labels)), d.Labels...)
	sort.Strings(sorted)

	bestQuorum := 0
	bestLabel := ""
	for _, l := range sorted {
		// labels in cause_detail are already lowercased by the
		// webhook normaliser, and overrides keys were lowercased
		// by the parser. So a direct map lookup is sufficient — no
		// case-fold here.
		if q, ok := overrides[l]; ok && q > bestQuorum {
			bestQuorum = q
			bestLabel = l
		}
	}
	if bestQuorum == 0 {
		return baseRequired, ""
	}
	return bestQuorum, bestLabel
}
