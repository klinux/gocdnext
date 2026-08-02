package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// ApprovalDispatcher is the slice of the session store the expirer needs to
// stop work belonging to a run it just canceled. Narrow on purpose: the
// expirer must be constructible in tests without a live gRPC session store.
type ApprovalDispatcher interface {
	Dispatch(agentID uuid.UUID, msg *gocdnextv1.ServerMessage) error
	AllAgentIDs(kind string) []uuid.UUID
}

// ApprovalExpirer cancels runs whose approval gate sat unanswered past its
// window. It exists because an abandoned gate has only two other exits — a
// human clicking, or supersede cancelling it when a newer run arrives — and
// neither is guaranteed. Without this, a gate nobody returns to keeps its run
// in `running` forever.
//
// Deliberately NOT folded into Reaper: the Reaper's concern is "the agent is
// gone", a machine-liveness problem with a 90s horizon. This one is "the human
// never came back", a days-long horizon with a completely different cadence
// and failure mode.
//
// An expired gate terminalises the run as CANCELED, never failed — see
// queries/approval_expiry.sql for why that distinction is load-bearing for the
// success-rate metric.
type ApprovalExpirer struct {
	store      *store.Store
	log        *slog.Logger
	dispatcher ApprovalDispatcher
	checks     checksReporter

	interval       time.Duration
	defaultTimeout time.Duration
	pageSize       int32
	scanCap        int
	perSweepLimit  int

	// cursor is the keyset position the next sweep resumes from, carried
	// ACROSS sweeps and rewound at the end of the queue. This is what makes a
	// bounded sweep starvation-free: a gate skipped this pass is reached on a
	// later one instead of hiding behind the same prefix forever. Plain
	// mutable state — Sweep is not safe for concurrent use.
	cursor store.ApprovalGateCursor
}

// ApprovalExpirer defaults. The sweep interval is minutes, not seconds: the
// shortest window a gate can declare is domain.ApprovalTimeoutMin (1m), and the
// fleet default is measured in days, so a tighter cadence would only burn
// queries.
//
// perSweepLimit bounds how many runs one pass may CANCEL — each expiry fans out
// into audit, agent frames, service cleanup and a GitHub check close, so
// draining a large backlog over several ticks keeps that fan-out from arriving
// as one thundering herd.
//
// scanCap bounds how many candidates one pass may READ AND RESOLVE. Resolving a
// window costs a definition read + unmarshal per distinct gate, so an unbounded
// scan over a huge pending queue would make each tick expensive. Neither
// ceiling starves anything, because the cursor resumes where the sweep stopped
// — the remainder is deferred to the next tick, not dropped.
const (
	DefaultApprovalExpirerInterval = time.Minute
	DefaultApprovalPageSize        = 500
	DefaultApprovalScanCap         = 5000
	DefaultApprovalPerSweepLimit   = 50
)

// NewApprovalExpirer builds an expirer with defaults. A zero defaultTimeout
// (operator set GOCDNEXT_APPROVAL_DEFAULT_TIMEOUT=never) disables the
// fleet-wide fallback but NOT the expirer: gates that declare their own
// `timeout:` still expire.
func NewApprovalExpirer(s *store.Store, defaultTimeout time.Duration, log *slog.Logger) *ApprovalExpirer {
	if log == nil {
		log = slog.Default()
	}
	return &ApprovalExpirer{
		store:          s,
		log:            log,
		interval:       DefaultApprovalExpirerInterval,
		defaultTimeout: defaultTimeout,
		pageSize:       DefaultApprovalPageSize,
		scanCap:        DefaultApprovalScanCap,
		perSweepLimit:  DefaultApprovalPerSweepLimit,
	}
}

// WithDispatcher wires the session store so a canceled run's still-running jobs
// get a CancelJob frame and its services get torn down. Optional: without it
// the DB state is still correct and the agent reconnect/reaper paths finalise
// the jobs — the frames only make the stop prompt.
func (e *ApprovalExpirer) WithDispatcher(d ApprovalDispatcher) *ApprovalExpirer {
	e.dispatcher = d
	return e
}

// WithChecksReporter wires the GitHub Checks reporter so an expiry closes the
// run's check. Load-bearing for the same reason supersede needs it: expiry
// terminalises straight to canceled and SKIPS the JobResult completion path
// that normally reports the conclusion — without this, a check opened for the
// run stays `in_progress` forever on the PR. nil = feature off.
//
// Pass the SAME reporter the AgentService and Scheduler get, so every
// terminalisation path reports to the same check runs.
func (e *ApprovalExpirer) WithChecksReporter(r checksReporter) *ApprovalExpirer {
	e.checks = r
	return e
}

// WithInterval / WithLimits let tests compress the cadence and batch sizes.
func (e *ApprovalExpirer) WithInterval(d time.Duration) *ApprovalExpirer {
	if d > 0 {
		e.interval = d
	}
	return e
}

// WithLimits tunes the per-sweep bounds: the keyset page size, the ceiling on
// candidates scanned+resolved, and the ceiling on runs cancelled.
func (e *ApprovalExpirer) WithLimits(pageSize int32, scanCap, perSweep int) *ApprovalExpirer {
	if pageSize > 0 {
		e.pageSize = pageSize
	}
	if scanCap > 0 {
		e.scanCap = scanCap
	}
	if perSweep > 0 {
		e.perSweepLimit = perSweep
	}
	return e
}

// Run blocks until ctx is canceled, sweeping every interval.
func (e *ApprovalExpirer) Run(ctx context.Context) error {
	e.log.Info("approval expirer started",
		"interval", e.interval,
		"default_timeout", e.defaultTimeout,
		"per_sweep_limit", e.perSweepLimit)

	// Sweep once on startup so a server that was down while windows elapsed
	// doesn't make operators wait out a full interval. On the FIRST boot after
	// this feature ships, this is also what drains the pre-existing backlog of
	// abandoned gates — bounded per sweep, so it drains over several ticks
	// rather than as one burst.
	e.Sweep(ctx)

	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.log.Info("approval expirer stopping")
			return nil
		case <-t.C:
			e.Sweep(ctx)
		}
	}
}

// Sweep runs one pass. Exposed so tests can drive it deterministically.
//
// Keyset-paged, and the paging is a CORRECTNESS requirement rather than a
// tuning choice. Whether a candidate actually expired is only knowable after
// reading the run's definition in Go, so any single capped query truncates the
// set BEFORE that filter runs: N older gates that are `never` — or merely still
// inside a 7-day window — would permanently hide a newer gate carrying
// `timeout: 5m`. Paging past them is the only way the filter sees everything.
//
// The cursor RESUMES ACROSS SWEEPS and wraps at the end of the queue. That is
// what keeps the two requirements from fighting: each sweep stays bounded
// (scanCap rows, perSweepLimit cancellations), while every gate is still
// visited eventually. Bounding within a single sweep alone would just move the
// starvation cap higher, not remove it.
//
// Not safe for concurrent calls — the cursor is plain mutable state. Run()
// drives it from one goroutine; tests call it serially.
func (e *ApprovalExpirer) Sweep(ctx context.Context) {
	// The cutoff is the SHORTEST window any gate could declare, not the server
	// default: a gate with `timeout: 5m` must surface even when the fleet
	// default is 7 days.
	cutoff := time.Now().Add(-domain.ApprovalTimeoutMin)

	// Memoised per (run, GATE) — not per run. Two gates in the same run can
	// carry different windows (`approve-staging: never` alongside an
	// approve-prod that inherits the default), so caching one run's answer for
	// all of its gates would apply the wrong window to the siblings.
	windows := make(map[gateKey]runWindow)
	expired, scanned := 0, 0

	for expired < e.perSweepLimit && scanned < e.scanCap {
		page, err := e.store.ListPendingApprovalGates(ctx, cutoff, e.cursor, e.pageSize)
		if err != nil {
			e.log.Warn("approval expirer: list candidates", "err", err)
			return
		}
		if len(page) == 0 {
			// End of the queue: rewind so the next sweep starts from the
			// oldest gate again. Without the wrap, a cursor parked at the tail
			// would never revisit anything behind it.
			e.cursor = store.ApprovalGateCursor{}
			break
		}
		for _, c := range page {
			// Check the ceilings BEFORE consuming the candidate. Advancing the
			// cursor first would move past a gate this sweep never looked at,
			// so it would not be reconsidered until the cursor wrapped the
			// whole queue — breaking the "drains from the cursor next tick"
			// promise, and on a queue that keeps growing, deferring it a long
			// way. The cursor must only ever point past rows actually judged.
			if expired >= e.perSweepLimit || scanned >= e.scanCap {
				break
			}
			e.cursor = e.cursor.Next(c)
			scanned++

			key := gateKey{runID: c.RunID, jobName: c.JobName}
			w, ok := windows[key]
			if !ok {
				d, freezeEnvs, expires, rerr := e.store.ResolveApprovalWindow(ctx, c.RunID, c.JobName, e.defaultTimeout)
				if rerr != nil {
					// Fail CLOSED for a destructive action: a definition we
					// can't read is a run we can't justify cancelling. Skip
					// and retry on a later sweep.
					e.log.Warn("approval expirer: resolve window; skipping",
						"run_id", c.RunID, "job", c.JobName, "err", rerr)
					continue
				}
				w = runWindow{d: d, freezeEnvs: freezeEnvs, expires: expires}
				windows[key] = w
			}
			if !w.expires {
				continue // `timeout: never`, or no window applies at all
			}
			// Coarse pre-filter only — a valid LOWER bound: the freeze floor
			// (#208) can only push effective_start LATER, so a gate still inside
			// awaiting_since+window is definitely not expired. The AUTHORITATIVE,
			// floor-and-freeze-aware decision is made in-tx by ExpireApprovalGate.
			if time.Since(c.AwaitingSince) < w.d {
				continue
			}
			if e.expireOne(ctx, c, w) {
				expired++
			}
		}
	}

	// Never let a bound truncate coverage silently: both ceilings leave the
	// cursor parked mid-queue, and the operator should be able to see that the
	// remainder is deferred rather than dropped.
	switch {
	case expired >= e.perSweepLimit:
		e.log.Info("approval expirer: per-sweep cancel limit reached; the rest drain from the cursor next tick",
			"limit", e.perSweepLimit, "scanned", scanned)
	case scanned >= e.scanCap:
		e.log.Info("approval expirer: per-sweep scan cap reached; the sweep resumes from the cursor next tick",
			"cap", e.scanCap, "expired", expired)
	}
	if expired > 0 {
		e.log.Info("approval expirer: swept", "expired", expired, "scanned", scanned)
	}
}

// gateKey identifies one gate for the sweep's window memo. The job name is
// part of the key because sibling gates in the same run legitimately carry
// different windows.
type gateKey struct {
	runID   uuid.UUID
	jobName string
}

// runWindow memoises one gate's resolved window across a sweep. freezeEnvs is
// the gate's GovernedFreezeEnvs (#208), carried so the in-tx freeze check does
// not re-decode the definition per candidate.
type runWindow struct {
	d          time.Duration
	freezeEnvs []string
	expires    bool
}

// expireOne terminalises a single abandoned gate's run and fires the external
// effects. Returns whether the run was actually canceled — a gate decided
// under us, or a run already terminal, counts as neither success nor failure.
func (e *ApprovalExpirer) expireOne(ctx context.Context, c store.PendingApprovalGate, w runWindow) bool {
	// The reason lands in runs.cancel_reason and is rendered in the UI. It cites
	// the window and the gate NAME only — never an approver identity, a ref, or
	// any other value that shouldn't travel into a log line.
	reason := fmt.Sprintf("approval timeout (%s) on gate %q", w.d, c.JobName)

	res, err := e.store.ExpireApprovalGate(ctx, store.ExpireApprovalGateInput{
		JobRunID:      c.JobRunID,
		RunID:         c.RunID,
		PipelineID:    c.PipelineID,
		FreezeEnvs:    w.freezeEnvs,
		Window:        w.d,
		AwaitingSince: c.AwaitingSince,
		Reason:        reason,
	})
	switch {
	case errors.Is(err, store.ErrApprovalGateDecided):
		// Someone clicked between the scan and the write. That's the outcome
		// this whole component exists to force — not a problem.
		return false
	case errors.Is(err, store.ErrRunAlreadyTerminal), errors.Is(err, store.ErrRunNotFound):
		return false
	case errors.Is(err, store.ErrApprovalGateFrozen),
		errors.Is(err, store.ErrApprovalGateContended),
		errors.Is(err, store.ErrApprovalGateWithinWindow):
		// The freeze pause (#208): the gate is deliberately held, a concurrent
		// freeze/unfreeze forced a lock back-off, or an unfreeze granted a fresh
		// window. All benign — retried next sweep, no metric, no cancel. Debug so
		// a frozen fleet doesn't spam the log every interval.
		e.log.Debug("approval expirer: paused by freeze",
			"run_id", c.RunID, "job", c.JobName, "reason", err)
		return false
	case err != nil:
		e.log.Warn("approval expirer: expire gate",
			"run_id", c.RunID, "job_run_id", c.JobRunID, "job", c.JobName, "err", err)
		return false
	}

	metrics.ApprovalsExpired.Inc()
	e.log.Info("approval expirer: gate expired",
		"run_id", c.RunID, "job", c.JobName,
		"counter", c.Counter, "window", w.d,
		"awaiting_since", c.AwaitingSince)

	// Audit is best-effort and post-commit, matching the audit package's
	// contract: losing a row must not undo a cancel that already happened.
	// Counters and the window only — no branch, ref, or approver value.
	if _, aerr := e.store.EmitAuditEvent(ctx, store.AuditEmit{
		Action:     store.AuditActionApprovalExpired,
		TargetType: "job_run",
		TargetID:   c.JobRunID.String(),
		Metadata: map[string]any{
			"run_id":         c.RunID.String(),
			"counter":        c.Counter,
			"job_name":       c.JobName,
			"window_seconds": int64(w.d / time.Second),
			"waited_seconds": int64(time.Since(c.AwaitingSince) / time.Second),
		},
	}); aerr != nil {
		e.log.Warn("approval expirer: audit emit", "run_id", c.RunID, "err", aerr)
	}

	// Close the run's GitHub check. Expiry terminalises straight to canceled,
	// skipping the JobResult path that normally reports completion — without
	// this a check opened for the run stays in_progress on the PR forever.
	// Explicitly best-effort (a fire-and-forget GitHub PATCH): a stale check is
	// cosmetic next to the durable cancel that already committed, and coupling
	// this to GitHub uptime would buy nothing. Mirrors supersede's fire point.
	if e.checks != nil {
		e.checks.ReportRunCompleted(ctx, c.RunID, string(domain.StatusCanceled))
	}

	e.fireCancelEffects(ctx, c.RunID, res)
	return true
}
