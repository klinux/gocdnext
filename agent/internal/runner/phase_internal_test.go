package runner

import (
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/metrics"
)

// phaseCount reads the histogram sample_count for a given phase label off the
// agent metrics registry.
func phaseCount(t *testing.T, phase string) uint64 {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "gocdnext_agent_job_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "phase" && l.GetValue() == phase {
					return m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	return 0
}

func TestPhaseTimerObservesEachPhaseOnce(t *testing.T) {
	beforePrep := phaseCount(t, "prep")
	beforeTask := phaseCount(t, "task")

	var pt phaseTimer
	pt.enter("prep")
	pt.enter("task") // closes prep (observes it once), opens task
	pt.finish()      // closes task (observes it once)
	pt.finish()      // idempotent — no double count

	if got := phaseCount(t, "prep") - beforePrep; got != 1 {
		t.Fatalf("prep observations = %d, want 1", got)
	}
	if got := phaseCount(t, "task") - beforeTask; got != 1 {
		t.Fatalf("task observations = %d, want 1", got)
	}
}

func TestPhaseTimerFinishWithoutEnterIsNoop(t *testing.T) {
	before := phaseCount(t, "prep")
	var pt phaseTimer
	pt.finish() // never entered a phase
	if got := phaseCount(t, "prep") - before; got != 0 {
		t.Fatalf("unexpected observation from empty timer: %d", got)
	}
}
