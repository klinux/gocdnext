package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// holdPriority ranks the competing explanations for why a run isn't advancing.
//
// runs.queue_reason is a SINGLE run-level column, but the things that hold a run
// are per-JOB (a frozen `production` deploy, a supersede-blocked `staging` one,
// a contended lane lock). Without a total order, whichever job the dispatch loop
// happened to visit last would win, and the column would be rewritten two or
// three times per tick — the exact churn the IS DISTINCT FROM guard exists to
// prevent, plus an explanation that flips with job naming.
//
// Highest wins. A freeze outranks the supersede states because it is the one a
// HUMAN declared and only a human can lift: telling the operator "superseded"
// while a change-freeze is the actual thing blocking production sends them to
// the wrong place entirely.
type holdPriority int

const (
	holdNone holdPriority = iota
	holdSupersedeLockBusy
	holdSupersedeBlocked
	holdFrozen
)

// runHold accumulates the winning explanation across a whole dispatch tick. It
// is written to the database ONCE, after the loop — never per job.
type runHold struct {
	priority holdPriority
	reason   string
}

// record keeps the highest-priority reason seen this tick. Ties break on the
// lexicographically smallest reason so the outcome does not depend on job
// iteration order: two supersede-blocked envs must not alternate tick to tick
// any more than two frozen ones may.
func (h *runHold) record(p holdPriority, reason string) {
	if p > h.priority || (p == h.priority && (h.reason == "" || reason < h.reason)) {
		h.priority, h.reason = p, reason
	}
}

func (h *runHold) held() bool { return h.priority != holdNone }

// freezeMemo is one tick's answer to "which of this run's ready deploy jobs are
// held by an environment change-freeze?" (#202).
//
// Built ONCE per dispatchRun from ONE pipeline decode and ONE batched query,
// then consulted per job. The alternatives — a freeze probe inside the per-job
// loop, or re-decoding the definition to find each job's environment — cost N
// queries and N JSON unmarshals per tick, forever, for a feature that is off
// almost all the time.
type freezeMemo struct {
	// frozen is the set of frozen environments among the ones scanned. nil when
	// nothing is frozen (the overwhelmingly common case), which makes
	// `holds()` a cheap map miss.
	frozen map[string]struct{}
	// winner is the first frozen env in NAME order, and it owns the run-level
	// queue_reason. Determinism matters: queue_reason is per-RUN while a freeze
	// is per-ENV, so with `prod` and `staging` both frozen in one active stage a
	// non-deterministic pick would flip the stamp between
	// `frozen-deploy:prod` and `frozen-deploy:staging` on every tick.
	winner string
	// envByJob maps job name -> declared environment (deploy: OR job-level
	// environment:, via Job.TargetEnvironment), from the single decode. The early
	// gate reads it instead of re-decoding the pipeline per candidate.
	envByJob map[string]string
}

func (m freezeMemo) any() bool { return len(m.frozen) > 0 }

func (m freezeMemo) holds(env string) bool {
	if len(m.frozen) == 0 {
		return false
	}
	_, ok := m.frozen[env]
	return ok
}

// envFor returns the declared environment of a job (deploy: or environment:), or
// "" for a job that declares none.
func (m freezeMemo) envFor(jobName string) string { return m.envByJob[jobName] }

// declaredEnvIndex decodes the run's pipeline snapshot ONCE and indexes each
// job's declared environment by job name — a `deploy:` marker OR a bare
// `environment:` (a migration), via Job.TargetEnvironment. That is what lets a
// freeze hold a non-deploy job.
//
// The per-call alternative (jobDefFromDefinition) unmarshals the ENTIRE pipeline
// definition every time, so reaching for it inside a per-job loop turns one
// decode into N. See BenchmarkFreezePreScan for the measured difference on a
// 50-job pipeline.
func declaredEnvIndex(definition []byte) (map[string]string, error) {
	if len(definition) == 0 {
		return nil, nil
	}
	var def domain.Pipeline
	if err := json.Unmarshal(definition, &def); err != nil {
		return nil, fmt.Errorf("scheduler: decode pipeline for freeze pre-scan: %w", err)
	}
	var out map[string]string
	for _, j := range def.Jobs {
		env := j.TargetEnvironment()
		if env == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, 4)
		}
		out[j.Name] = env
	}
	return out, nil
}

// scanFrozenDeployEnvs builds the memo for a tick.
//
// `ready` must be the jobs that are ACTUALLY dispatchable right now — the ones
// that already passed the needs-satisfaction gate — not the raw
// ListDispatchableJobs output. A deploy job still blocked by an unfinished
// upstream is not being held by the freeze, and letting it contribute would make
// `frozen-deploy:*` the run's queue explanation while the real reason is a
// dependency, hiding the actual blocker from the operator.
//
// Returns a zero memo without touching the database when the pipeline has no
// env-declaring job at all, so env-less pipelines keep their hot path untouched.
func (s *Scheduler) scanFrozenDeployEnvs(ctx context.Context, run store.RunForDispatch, ready []store.DispatchableJob) freezeMemo {
	envByJob, err := declaredEnvIndex(run.Definition)
	if err != nil {
		// The dispatch path below reports a bad definition on its own terms;
		// here it just means we cannot tell which jobs target an environment, so
		// the pre-scan contributes nothing and the authoritative admission check
		// still holds.
		s.log.Warn("scheduler: freeze pre-scan decode", "run_id", run.ID, "err", err)
		return freezeMemo{}
	}
	if len(envByJob) == 0 {
		return freezeMemo{} // no env-declaring jobs at all — no query, no allocation
	}

	seen := make(map[string]struct{}, 2)
	var envs []string
	for _, job := range ready {
		env, hasEnv := envByJob[job.Name]
		if !hasEnv {
			continue
		}
		if _, dup := seen[env]; dup {
			continue
		}
		seen[env] = struct{}{}
		envs = append(envs, env)
	}
	if len(envs) == 0 {
		return freezeMemo{envByJob: envByJob}
	}

	frozen, err := s.store.FrozenEnvironments(ctx, run.ProjectID, envs)
	if err != nil {
		// FAIL OPEN, deliberately, and loudly. This is the cheap early gate, not
		// the guarantee: the authoritative re-check runs inside the admitting
		// transaction under the advisory lock, so a DB blip here costs one
		// wasted dispatch attempt that is then refused at admission — never an
		// admitted deploy into a frozen environment. Failing closed instead
		// would let a transient error halt every deploy in the installation.
		s.log.Warn("scheduler: freeze pre-scan failed, deferring to admission check",
			"run_id", run.ID, "project_id", run.ProjectID, "err", err)
		return freezeMemo{envByJob: envByJob}
	}
	if len(frozen) == 0 {
		return freezeMemo{envByJob: envByJob}
	}
	// The query already returns ORDER BY name; sorting again is a no-op that
	// makes the determinism contract local and independent of the SQL.
	sort.Strings(frozen)
	set := make(map[string]struct{}, len(frozen))
	for _, f := range frozen {
		set[f] = struct{}{}
	}
	return freezeMemo{frozen: set, winner: frozen[0], envByJob: envByJob}
}

// applyRunHold writes the tick's SINGLE queue_reason decision: the winning hold,
// or a clear when nothing held the run.
//
// One statement per tick — exactly the cost this path had before the freeze
// existed. Both statements are guarded in SQL (IS DISTINCT FROM / IS NOT NULL),
// so a run whose situation did not change costs zero row versions. That is what
// makes a run parked behind a frozen `production` for a week free, rather than
// several writes per tick forever.
//
// The Info log fires only on a real TRANSITION, for the same reason: a held run
// re-evaluated every 15 seconds would otherwise emit ~5,760 identical lines per
// day, which is how an operator learns to ignore the log.
func (s *Scheduler) applyRunHold(ctx context.Context, run store.RunForDispatch, hold runHold) {
	if !hold.held() {
		if err := s.store.ClearRunQueueReason(ctx, run.ID); err != nil {
			s.log.Warn("scheduler: clear queue_reason", "run_id", run.ID, "err", err)
		}
		return
	}
	changed, err := s.store.SetActiveRunQueueReason(ctx, run.ID, hold.reason)
	if err != nil {
		// Best-effort: the operator loses the explanation for THIS tick only,
		// and the loop has already dispatched whatever it could.
		s.log.Warn("scheduler: set queue_reason", "run_id", run.ID, "reason", hold.reason, "err", err)
		return
	}
	if changed {
		s.log.Info("scheduler: run held", "run_id", run.ID, "queue_reason", hold.reason)
	}
}
