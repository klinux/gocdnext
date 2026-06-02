package scheduler

import (
	"fmt"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// buildJobStatusMap folds the store's flat (name, matrix_key,
// status) projection into a name-keyed lookup for the gate.
// Matrix fanouts under the same name land in the same slice in
// store-iteration order (the SQL ORDER BY name, matrix_key NULLS
// FIRST is what makes the slice deterministic).
//
// Allocated once per dispatch tick — the gate then reads it
// O(1)-per-name for every candidate's `needs:` list. The
// alternative (re-query per candidate) would be N×M queries on a
// run with N candidates and M needs each; with N=10 jobs in a stage
// declaring needs on 4 siblings, that's 40 round-trips vs 1.
func buildJobStatusMap(rows []store.JobStatusForRun) JobStatusMap {
	m := make(JobStatusMap, len(rows))
	for _, r := range rows {
		m[r.Name] = append(m[r.Name], JobStatusRow{
			MatrixKey: r.MatrixKey,
			Status:    r.Status,
		})
	}
	return m
}

// JobStatusRow is the per-(name, matrix_key) status carried in the
// status map the scheduler builds once per dispatch tick. The
// scheduler.sql query loads name+matrix_key+status for every job in
// the run; needs-checking iterates this map for each candidate
// dispatch.
type JobStatusRow struct {
	MatrixKey string
	Status    string
}

// JobStatusMap groups statuses by job name. Matrix jobs surface as
// multiple rows under the same name; an empty-matrix-key job appears
// as a single row with MatrixKey == "". needs-checking against a
// matrix-fanout job requires EVERY row under that name to be
// terminal-succeeded — conservative, matches GitHub Actions and
// GoCD semantics.
type JobStatusMap map[string][]JobStatusRow

// NeedsCheck is the verdict the gate returns. Ok=true means the
// downstream job can be dispatched. When Ok=false, UpstreamTerminal
// distinguishes two follow-up paths:
//
//   - false → an upstream is still running/queued; leave the
//     downstream in `queued` and rely on the next dispatch tick
//     (triggered by the upstream's CompleteJob NOTIFY).
//   - true → an upstream is in a non-success terminal state
//     (failed/canceled/skipped, or is missing from the run
//     entirely — which is a parse-time bug that survived to
//     dispatch). The downstream must be skipped now so the stage
//     can close, otherwise the run hangs forever.
//
// Detail is the human-readable "waiting on X: running" or
// "X: failed" string the scheduler stamps onto the job row's
// `error` column for operator visibility.
type NeedsCheck struct {
	Ok               bool
	UpstreamTerminal bool
	Detail           string
}

// needsSatisfied evaluates a downstream job's `needs:` list against
// a snapshot of all jobs in the run. The first unsatisfied entry
// determines the verdict — short-circuit so the operator sees the
// most relevant blocker, not a concatenation.
//
// Edge cases captured:
//
//   - Empty `needs` → trivially satisfied (Ok=true). The dispatch
//     loop already handles "no needs" jobs without calling this,
//     but the function is defined to make that explicit and the
//     unit tests can pin the behaviour.
//
//   - Upstream name not in the status map → treat as Terminal with
//     a "not in this run" detail. Could only happen via a parse-time
//     bug (parser should reject `needs:` referencing unknown jobs)
//     or a schema drift mid-run; either way the downstream can't
//     proceed and should fail loud rather than wait forever.
//
//   - Matrix fanout: a single name maps to N (matrix_key, status)
//     rows. ALL must be succeeded — one running/failed row blocks.
//     The "one matrix child must succeed" alternative was rejected
//     because it makes the `needs:` semantic depend on hidden
//     downstream knowledge of which matrix combo actually matters.
//     "All succeed" is conservative and matches what the user
//     wrote literally.
//
//   - Status precedence on multi-row failures: succeeded < running
//     < queued < awaiting_approval < skipped < canceled < failed.
//     We DON'T sort by precedence — first-non-succeeded wins. This
//     keeps the implementation O(N) and the detail string matches
//     the iteration order, which is deterministic (matrix keys are
//     sorted at row insert time per store/runs.go).
func needsSatisfied(needs []string, status JobStatusMap) NeedsCheck {
	for _, name := range needs {
		rows, found := status[name]
		if !found {
			return NeedsCheck{
				Ok:               false,
				UpstreamTerminal: true,
				Detail:           fmt.Sprintf("%s: not in this run", name),
			}
		}
		if len(rows) == 0 {
			// Status map shouldn't produce empty slices, but be defensive:
			// an empty entry is structurally indistinguishable from
			// "missing" for the purpose of waiting forever.
			return NeedsCheck{
				Ok:               false,
				UpstreamTerminal: true,
				Detail:           fmt.Sprintf("%s: no job_run rows", name),
			}
		}
		for _, r := range rows {
			switch r.Status {
			case "success":
				continue
			case "failed", "canceled", "skipped":
				return NeedsCheck{
					Ok:               false,
					UpstreamTerminal: true,
					Detail:           describeBlocker(name, r),
				}
			default:
				// queued, running, awaiting_approval, or anything else
				// not in the terminal set — wait for next tick.
				return NeedsCheck{
					Ok:               false,
					UpstreamTerminal: false,
					Detail:           describeBlocker(name, r),
				}
			}
		}
	}
	return NeedsCheck{Ok: true}
}

// describeBlocker formats the per-job detail string. For a matrix
// fanout, include the specific matrix_key so the operator knows
// which combo is blocking: "types-generate[node-20]: running".
// For a non-matrix job, drop the bracket noise.
func describeBlocker(name string, r JobStatusRow) string {
	if r.MatrixKey == "" {
		return fmt.Sprintf("%s: %s", name, r.Status)
	}
	return fmt.Sprintf("%s[%s]: %s", name, r.MatrixKey, r.Status)
}

// summarizeNeeds is a logger-friendly summary of the entire needs
// list against the status map. Used in the "leaving queued" Info
// log so the operator sees ALL blockers at once, not just the
// first — saves a round of "fix one, find next" when several
// upstreams are in flight. Trimmed at a couple hundred chars to
// keep the structured log line readable.
func summarizeNeeds(needs []string, status JobStatusMap) string {
	parts := make([]string, 0, len(needs))
	for _, name := range needs {
		rows, found := status[name]
		if !found {
			parts = append(parts, name+":missing")
			continue
		}
		// Pick the first non-succeeded row to represent this name in
		// the summary. If all rows are succeeded, skip the name (it's
		// not a blocker).
		var blocker *JobStatusRow
		for i := range rows {
			if rows[i].Status != "success" {
				blocker = &rows[i]
				break
			}
		}
		if blocker == nil {
			continue
		}
		parts = append(parts, describeBlocker(name, *blocker))
	}
	out := strings.Join(parts, ", ")
	if len(out) > 240 {
		out = out[:240] + "…"
	}
	return out
}
