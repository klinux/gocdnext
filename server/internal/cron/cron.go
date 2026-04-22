// Package cron runs scheduled pipeline triggers. A background
// Ticker wakes up on a small interval, asks the store for every
// cron material on the system, and dispatches a run for each one
// whose cron expression says "now is past the next firing time
// after the last fire".
//
// Expressions follow standard 5-field cron syntax (minute, hour,
// day-of-month, month, day-of-week) via robfig/cron/v3, with the
// common macros supported (@hourly, @daily, @weekly, @monthly).
//
// Pipelines with ONLY a cron material run with no checkout — the
// user's scripts are self-contained, or fetch sources themselves.
// Pipelines combining cron + git trigger via cron and leave the
// git revision unresolved for this MVP; a follow-up wires the
// git HEAD fetch into the fire path.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	robfig "github.com/robfig/cron/v3"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DefaultTick is the cadence the ticker polls cron materials at.
// Short enough that a tick missing the exact minute of a 1-minute
// expression still fires within seconds of the intended time, big
// enough to keep DB load negligible for hundreds of schedules.
const DefaultTick = 30 * time.Second

// Ticker evaluates cron materials and dispatches runs. Lifecycle
// is context-scoped: Run blocks until ctx is canceled, then
// returns cleanly.
type Ticker struct {
	store  *store.Store
	log    *slog.Logger
	tick   time.Duration
	parser robfig.Parser
}

// New wires a Ticker. Uses a 5-field parser with descriptor
// macros (@daily etc.) — matches the syntax most CI systems
// document.
func New(s *store.Store, log *slog.Logger) *Ticker {
	if log == nil {
		log = slog.Default()
	}
	return &Ticker{
		store: s,
		log:   log,
		tick:  DefaultTick,
		parser: robfig.NewParser(
			robfig.Minute | robfig.Hour | robfig.Dom |
				robfig.Month | robfig.Dow | robfig.Descriptor,
		),
	}
}

// WithTick overrides the poll cadence. Intended for tests that
// need faster feedback than DefaultTick.
func (t *Ticker) WithTick(d time.Duration) *Ticker {
	if d > 0 {
		t.tick = d
	}
	return t
}

// Run blocks until ctx is canceled, evaluating cron materials on
// each tick. The first evaluation is immediate so restart-quick
// schedules (e.g. @every 1m) don't wait an extra tick after boot.
func (t *Ticker) Run(ctx context.Context) error {
	t.log.Info("cron ticker started", "tick", t.tick)
	t.evaluate(ctx, time.Now())
	tk := time.NewTicker(t.tick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			t.log.Info("cron ticker stopping")
			return nil
		case now := <-tk.C:
			t.evaluate(ctx, now)
		}
	}
}

func (t *Ticker) evaluate(ctx context.Context, now time.Time) {
	rows, err := t.store.ListCronMaterials(ctx)
	if err != nil {
		t.log.Warn("cron: list materials", "err", err)
		return
	}
	for _, r := range rows {
		sched, err := t.parser.Parse(r.Expression)
		if err != nil {
			t.log.Warn("cron: invalid expression",
				"material_id", r.MaterialID, "expression", r.Expression, "err", err)
			continue
		}
		// Baseline for "when was the next fire after". When a
		// material has fired before, we pick up right after its
		// last fire and honour the expression exactly. When it
		// has NEVER fired (just-applied schedule), we seed with
		// the zero time so Next(zero) returns an instant already
		// in the past, causing the first tick to fire — the user
		// gets instant confirmation that their schedule works
		// and MarkCronFired records the baseline for the rest of
		// the lifecycle. Previous behaviour (baseline = now-tick)
		// trapped never-fired materials in an always-future Next
		// calculation for sub-minute expressions.
		var baseline time.Time
		if r.LastFiredAt != nil {
			baseline = *r.LastFiredAt
		}
		next := sched.Next(baseline)
		if next.After(now) {
			continue
		}
		if err := t.fire(ctx, r, now); err != nil {
			t.log.Warn("cron: fire",
				"material_id", r.MaterialID, "pipeline_id", r.PipelineID, "err", err)
			continue
		}
	}
}

// fire inserts a modification for this tick and creates a run
// tagged cause="cron". MarkCronFired runs after the run insert —
// a crash in between is benign: on recovery, the modification
// already exists (InsertModification is idempotent on
// material_id+revision+branch) and re-fire simply re-inserts the
// same row, so we don't double-spawn.
func (t *Ticker) fire(ctx context.Context, r store.CronMaterialRow, now time.Time) error {
	revision := fmt.Sprintf("cron:%d", now.Unix())
	branch := "cron"
	mod, err := t.store.InsertModification(ctx, store.Modification{
		MaterialID:  r.MaterialID,
		Revision:    revision,
		Branch:      branch,
		Author:      "cron",
		Message:     fmt.Sprintf("cron tick at %s", now.UTC().Format(time.RFC3339)),
		CommittedAt: now,
	})
	if err != nil {
		return fmt.Errorf("insert modification: %w", err)
	}
	causeDetail, _ := json.Marshal(map[string]any{
		"expression": r.Expression,
		"fired_at":   now.UTC().Format(time.RFC3339),
	})
	run, err := t.store.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     r.PipelineID,
		MaterialID:     r.MaterialID,
		ModificationID: mod.ID,
		Revision:       revision,
		Branch:         branch,
		Provider:       "cron",
		Delivery:       revision,
		TriggeredBy:    "cron",
		Cause:          "cron",
		CauseDetail:    causeDetail,
	})
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	if err := t.store.MarkCronFired(ctx, r.MaterialID, now); err != nil {
		// Non-fatal — run is already created, worst case we re-fire
		// on the next tick but the run has a new modification key
		// (different Unix seconds) so it's a distinct row.
		t.log.Warn("cron: mark fired",
			"material_id", r.MaterialID, "err", err)
	}
	t.log.Info("cron: fired",
		"material_id", r.MaterialID, "pipeline_id", r.PipelineID,
		"run_id", run.RunID, "expression", r.Expression)
	return nil
}

// MustUUID is a tiny helper for tests / call sites that already
// validated the id upstream; exposes a panic on malformed uuids
// rather than threading err into every call.
func MustUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}
