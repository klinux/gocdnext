package runner

import (
	"fmt"
	"sync/atomic"
	"time"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
)

// emitPhase writes a phase-boundary marker into the job log. The
// "──" prefix makes boundaries scannable in a wall of build output,
// and gives the log viewer a stable hook for section folding later
// (#37). The UI already shows per-line elapsed time, so a marker AT
// the boundary is what attributes "where did the time go" — the
// operator-reported failure mode was a job reading "stuck for 4
// minutes" because tasks went quiet and post-task phases printed
// nothing.
func (r *Runner) emitPhase(a *gocdnextv1.JobAssignment, seq *atomic.Int64, msg string) {
	r.emitLog(a, seq, "stdout", "── "+msg)
}

// phaseDur renders an elapsed duration for phase markers — second
// precision; sub-second phases say "<1s" instead of noisy millis.
func phaseDur(start time.Time) string {
	d := time.Since(start).Round(time.Second)
	if d < time.Second {
		return "<1s"
	}
	return d.String()
}

// timedPhase wraps a phase body with start/finish markers sharing
// one label: "── <label> …" then "── <label> done in Xs".
func (r *Runner) timedPhase(a *gocdnextv1.JobAssignment, seq *atomic.Int64, label string, fn func()) {
	start := time.Now()
	r.emitPhase(a, seq, label+" …")
	fn()
	r.emitPhase(a, seq, fmt.Sprintf("%s done in %s", label, phaseDur(start)))
}

// plural picks the right suffix for tiny log grammar.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
