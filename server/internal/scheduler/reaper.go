package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Reaper periodically sweeps running jobs whose agent is gone and either
// re-queues them (attempt < cap) or fails them via CompleteJob (at cap).
// Kept separate from Scheduler.Run because the concerns don't overlap:
// Scheduler dispatches queued work onto live agents; Reaper keeps the
// DB state honest about which of those "running" rows are actually alive.
type Reaper struct {
	store       *store.Store
	log         *slog.Logger
	interval    time.Duration
	staleness   time.Duration
	maxAttempts int32
}

// ReaperDefaults match the agent heartbeat cadence (30s): a tick every 30s
// gives us two missed heartbeats of grace before re-queueing, and a cap of
// 3 attempts prevents infinite retry loops on a job that crashes agents.
const (
	DefaultReaperInterval    = 30 * time.Second
	DefaultReaperStaleness   = 90 * time.Second
	DefaultReaperMaxAttempts = 3
)

// NewReaper constructs a Reaper with sensible defaults. Use the With*
// setters to tune for tests.
func NewReaper(s *store.Store, log *slog.Logger) *Reaper {
	if log == nil {
		log = slog.Default()
	}
	return &Reaper{
		store:       s,
		log:         log,
		interval:    DefaultReaperInterval,
		staleness:   DefaultReaperStaleness,
		maxAttempts: DefaultReaperMaxAttempts,
	}
}

// WithInterval / WithStaleness / WithMaxAttempts let tests compress the
// cadence without fiddling with internal fields directly.
func (r *Reaper) WithInterval(d time.Duration) *Reaper {
	if d > 0 {
		r.interval = d
	}
	return r
}

func (r *Reaper) WithStaleness(d time.Duration) *Reaper {
	if d > 0 {
		r.staleness = d
	}
	return r
}

func (r *Reaper) WithMaxAttempts(n int32) *Reaper {
	if n > 0 {
		r.maxAttempts = n
	}
	return r
}

// Run blocks until ctx is canceled, ticking every `interval` and sweeping
// stale jobs on each tick.
func (r *Reaper) Run(ctx context.Context) error {
	r.log.Info("reaper started",
		"interval", r.interval,
		"staleness", r.staleness,
		"max_attempts", r.maxAttempts)

	// Run once on startup to catch anything that was running when the server
	// last died — otherwise those jobs wait out a full interval tick.
	r.Sweep(ctx)

	t := time.NewTicker(r.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reaper stopping")
			return nil
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep runs one pass. Exposed so tests (and admin endpoints later) can
// trigger it deterministically.
func (r *Reaper) Sweep(ctx context.Context) {
	results, err := r.store.ReclaimStaleJobs(ctx, r.maxAttempts, r.staleness)
	if err != nil {
		r.log.Warn("reaper: sweep failed", "err", err)
		return
	}
	if len(results) == 0 {
		return
	}

	var requeued, failed, skipped, errored int
	for _, res := range results {
		switch {
		case res.Err != nil:
			errored++
			r.log.Warn("reaper: reclaim entry error",
				"job_id", res.JobRunID, "err", res.Err)
		case res.Action == store.ReclaimActionRequeued:
			requeued++
			r.log.Info("reaper: job re-queued",
				"run_id", res.RunID, "job_id", res.JobRunID,
				"job_name", res.JobName, "attempt", res.Attempt,
				"previous_agent", res.AgentID)
		case res.Action == store.ReclaimActionFailed:
			failed++
			r.log.Warn("reaper: job failed at max attempts",
				"run_id", res.RunID, "job_id", res.JobRunID,
				"job_name", res.JobName, "attempts", res.Attempt+1)
		default:
			skipped++
		}
	}
	r.log.Info("reaper: sweep done",
		"requeued", requeued, "failed", failed, "skipped", skipped, "errors", errored)
}
