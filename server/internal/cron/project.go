package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	robfig "github.com/robfig/cron/v3"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// ProjectTicker fires project-level schedules. Unlike the
// material-level Ticker (which operates on `cron:` materials
// inside a single pipeline's YAML), this one walks the
// project_crons table: each enabled schedule, when due, queues a
// run for every pipeline in its target list (or every pipeline in
// the project when the list is empty).
//
// Both tickers share the robfig parser config + tick cadence so
// the two flavours of scheduled fire stay observationally
// identical (same lateness tolerance, same crash-safety contract).
type ProjectTicker struct {
	store  *store.Store
	log    *slog.Logger
	tick   time.Duration
	parser robfig.Parser

	// serial guards against overlapping evaluates when a tick
	// runs long — the scheduled workload may be "fire 20
	// pipelines at 2am" and we don't want two parallel fires.
	serial sync.Mutex
}

// NewProject wires the project-level ticker. Uses the same parser
// config as the material-level Ticker — 5-field cron + descriptor
// macros — so operators who learn one syntax can carry it over.
func NewProject(s *store.Store, log *slog.Logger) *ProjectTicker {
	if log == nil {
		log = slog.Default()
	}
	return &ProjectTicker{
		store: s,
		log:   log,
		tick:  DefaultTick,
		parser: robfig.NewParser(
			robfig.Minute | robfig.Hour | robfig.Dom |
				robfig.Month | robfig.Dow | robfig.Descriptor,
		),
	}
}

// WithTick overrides the poll cadence. Tests use it for faster
// feedback than DefaultTick.
func (t *ProjectTicker) WithTick(d time.Duration) *ProjectTicker {
	if d > 0 {
		t.tick = d
	}
	return t
}

// Run blocks until ctx is canceled, evaluating enabled project
// schedules on each tick. First evaluation is immediate so
// never-fired schedules don't wait an extra tick after boot.
func (t *ProjectTicker) Run(ctx context.Context) error {
	t.log.Info("project cron ticker started", "tick", t.tick)
	t.evaluate(ctx, time.Now())
	tk := time.NewTicker(t.tick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			t.log.Info("project cron ticker stopping")
			return nil
		case now := <-tk.C:
			t.evaluate(ctx, now)
		}
	}
}

func (t *ProjectTicker) evaluate(ctx context.Context, now time.Time) {
	if !t.serial.TryLock() {
		t.log.Warn("project cron: previous tick still running, skipping")
		return
	}
	defer t.serial.Unlock()

	rows, err := t.store.ListEnabledProjectCrons(ctx)
	if err != nil {
		t.log.Warn("project cron: list schedules", "err", err)
		return
	}
	for _, r := range rows {
		sched, err := t.parser.Parse(r.Expression)
		if err != nil {
			t.log.Warn("project cron: invalid expression",
				"schedule_id", r.ID, "expression", r.Expression, "err", err)
			continue
		}
		// Baseline + next calc mirrors the material-level Ticker:
		// never-fired rows get a zero baseline so Next(zero) lands
		// already in the past and the first tick after enable
		// triggers, giving the operator instant confirmation.
		var baseline time.Time
		if r.LastFiredAt != nil {
			baseline = *r.LastFiredAt
		}
		next := sched.Next(baseline)
		if next.After(now) {
			continue
		}
		if err := t.fire(ctx, r, now); err != nil {
			t.log.Warn("project cron: fire",
				"schedule_id", r.ID, "project_id", r.ProjectID, "err", err)
			continue
		}
	}
}

// fire resolves the target pipelines (pinned list or all project
// pipelines at fire time) and queues a scheduled run for each.
// Errors on individual pipelines don't abort the fire — one
// missing or never-pushed pipeline shouldn't starve the others.
// MarkProjectCronFired runs after all attempts so the baseline
// advances even when some targets 422.
func (t *ProjectTicker) fire(ctx context.Context, r store.EnabledProjectCron, now time.Time) error {
	pipelines := r.PipelineIDs
	if len(pipelines) == 0 {
		all, err := t.store.ListPipelineIDsByProject(ctx, r.ProjectID)
		if err != nil {
			return fmt.Errorf("resolve all pipelines: %w", err)
		}
		pipelines = all
	}
	if len(pipelines) == 0 {
		// Empty project — nothing to fire, still advance the
		// baseline so the schedule doesn't spin on every tick.
		return t.store.MarkProjectCronFired(ctx, r.ID, now)
	}

	causeDetail, _ := json.Marshal(map[string]any{
		"schedule_id":   r.ID.String(),
		"schedule_name": r.Name,
		"expression":    r.Expression,
		"fired_at":      now.UTC().Format(time.RFC3339),
	})

	fired := 0
	for _, pipelineID := range pipelines {
		_, err := t.store.TriggerManualRun(ctx, store.TriggerManualRunInput{
			PipelineID:  pipelineID,
			TriggeredBy: "cron:" + r.Name,
			Cause:       string(domain.CauseSchedule),
			CauseDetail: causeDetail,
		})
		if err != nil {
			if errors.Is(err, store.ErrNoModificationForPipeline) {
				t.log.Info("project cron: skipped pipeline (no modifications yet)",
					"schedule_id", r.ID, "pipeline_id", pipelineID)
				continue
			}
			t.log.Warn("project cron: per-pipeline fire",
				"schedule_id", r.ID, "pipeline_id", pipelineID, "err", err)
			continue
		}
		fired++
	}

	if err := t.store.MarkProjectCronFired(ctx, r.ID, now); err != nil {
		// Non-fatal: the fire already happened. Worst case a
		// replay after restart re-triggers; downstream modification
		// unique keys absorb the duplicate.
		t.log.Warn("project cron: mark fired",
			"schedule_id", r.ID, "err", err)
	}
	t.log.Info("project cron: fired",
		"schedule_id", r.ID,
		"project_id", r.ProjectID,
		"name", r.Name,
		"fired", fired,
		"total_targets", len(pipelines))
	return nil
}

// RunAll is the manual "Run all pipelines" operation for a
// project. Same fire mechanics the ticker uses, synchronous here
// so the HTTP handler can return the list of queued runs to the
// UI. Errors per-pipeline are collected in the result, never
// aborting the loop — one 422 shouldn't stop the others.
type RunAllResult struct {
	PipelineID uuid.UUID
	RunID      *uuid.UUID
	Error      string
}

// RunAll triggers a "manual" (cause=manual) run for every
// pipeline in the given project. Called by the POST
// /projects/{slug}/run-all HTTP handler — not the ticker.
// Skipping the scheduled cause keeps the audit trail accurate:
// this is an operator decision, not a timed one.
func RunAll(
	ctx context.Context, s *store.Store, projectID uuid.UUID,
	triggeredBy string,
) ([]RunAllResult, error) {
	pipelines, err := s.ListPipelineIDsByProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project pipelines: %w", err)
	}
	if len(pipelines) == 0 {
		return []RunAllResult{}, nil
	}
	causeDetail, _ := json.Marshal(map[string]any{
		"run_all":  true,
		"fired_at": time.Now().UTC().Format(time.RFC3339),
	})
	out := make([]RunAllResult, 0, len(pipelines))
	for _, pid := range pipelines {
		res, err := s.TriggerManualRun(ctx, store.TriggerManualRunInput{
			PipelineID:  pid,
			TriggeredBy: triggeredBy,
			Cause:       "manual",
			CauseDetail: causeDetail,
		})
		entry := RunAllResult{PipelineID: pid}
		if err != nil {
			entry.Error = err.Error()
		} else {
			rid := res.RunID
			entry.RunID = &rid
		}
		out = append(out, entry)
	}
	return out, nil
}
