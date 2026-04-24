// Package polling runs periodic HEAD checks against git materials
// that opted into poll_interval (per-material or project-level).
// A Ticker wakes up on a small cadence, asks the store for every
// pollable git material, and for each one whose effective interval
// has elapsed, resolves branch HEAD via the configsync Fetcher and
// inserts a modification + run when HEAD advanced.
//
// The trigger path (InsertModification + CreateRunFromModification)
// is the exact same as the webhook handler, so a polled material
// drives runs the same way a pushed one does — the `cause` just
// differs.
//
// Covers the firewall-bound-repo use case where the server can't
// receive webhook deliveries but CAN outbound-fetch the provider
// API. One-way network policies like that are common in corp
// envs; polling is the escape hatch.
package polling

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// DefaultTick is how often the poller evaluates the pollable-
// material list. 30s is short enough that a 1-minute poll_interval
// fires within 60-90s of its schedule, big enough to keep DB load
// negligible at any realistic scale (N projects × a handful of
// materials each).
const DefaultTick = 30 * time.Second

// HeadResolver is the narrow interface this package needs from the
// configsync Fetcher. Keeping it local lets tests stub behaviour
// without pulling the full provider machinery into unit coverage.
type HeadResolver interface {
	HeadSHA(ctx context.Context, scm store.SCMSource, branch string) (string, error)
}

// Ticker evaluates pollable git materials and inserts modifications
// when HEAD advances. Lifecycle is context-scoped: Run blocks until
// ctx is canceled, then returns cleanly.
type Ticker struct {
	store    *store.Store
	resolver HeadResolver
	log      *slog.Logger
	tick     time.Duration

	// serial protects against two overlapping evaluate() runs if a
	// slow tick collides with the next one. Tick is 30s and poll
	// ops are short, but a transient provider slowdown can push a
	// single tick past its window.
	serial sync.Mutex
}

// New wires a Ticker. When resolver is nil, Run returns an error
// rather than silently no-op'ing — polling requires a provider
// adapter, and a nil one is a wiring bug we want loud.
func New(s *store.Store, resolver HeadResolver, log *slog.Logger) *Ticker {
	if log == nil {
		log = slog.Default()
	}
	return &Ticker{
		store:    s,
		resolver: resolver,
		log:      log,
		tick:     DefaultTick,
	}
}

// WithTick overrides the poll cadence. Tests use it for faster
// feedback than DefaultTick.
func (t *Ticker) WithTick(d time.Duration) *Ticker {
	if d > 0 {
		t.tick = d
	}
	return t
}

// Run blocks until ctx is canceled, evaluating pollable materials
// on each tick. First evaluation is immediate so never-polled
// materials don't wait an extra tick after boot.
func (t *Ticker) Run(ctx context.Context) error {
	if t.resolver == nil {
		return fmt.Errorf("polling: HeadResolver is nil — wire a configsync.Fetcher")
	}
	t.log.Info("polling ticker started", "tick", t.tick)
	t.evaluate(ctx, time.Now())
	tk := time.NewTicker(t.tick)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			t.log.Info("polling ticker stopping")
			return nil
		case now := <-tk.C:
			t.evaluate(ctx, now)
		}
	}
}

func (t *Ticker) evaluate(ctx context.Context, now time.Time) {
	if !t.serial.TryLock() {
		t.log.Warn("polling: previous tick still running, skipping")
		return
	}
	defer t.serial.Unlock()

	rows, err := t.store.ListPollableGitMaterials(ctx)
	if err != nil {
		t.log.Warn("polling: list materials", "err", err)
		return
	}
	for _, r := range rows {
		if r.EffectiveInterval() <= 0 {
			continue
		}
		if !r.IsDue(now) {
			continue
		}
		if err := t.pollOne(ctx, r, now); err != nil {
			t.log.Warn("polling: tick",
				"material_id", r.MaterialID,
				"pipeline_id", r.PipelineID,
				"url", r.URL,
				"err", err)
			continue
		}
	}
}

// pollOne resolves HEAD, compares against last_head_sha, and when
// different inserts a modification + creates a run. Always records
// a poll outcome (success or failure) so the UI can show "last
// polled at" independent of whether HEAD moved.
func (t *Ticker) pollOne(ctx context.Context, m store.PollableGitMaterial, now time.Time) error {
	scm, branch, ok := materialToSCM(m)
	if !ok {
		// Missing scm_source binding OR missing branch: record the
		// skip as an error so operators see why polling isn't
		// firing for this material.
		return t.recordOutcome(ctx, m, now, m.LastHeadSHA,
			"material has no scm_source binding or branch")
	}

	sha, err := t.resolver.HeadSHA(ctx, scm, branch)
	if err != nil {
		// Provider failure — remember last good SHA so UI stays
		// stable and surface the error. Non-fatal to the tick.
		return t.recordOutcome(ctx, m, now, m.LastHeadSHA,
			fmt.Sprintf("resolve HEAD: %v", err))
	}
	if sha == "" {
		return t.recordOutcome(ctx, m, now, m.LastHeadSHA,
			"provider returned empty HEAD sha")
	}

	// No advance: first poll (LastHeadSHA empty) still records the
	// baseline; subsequent polls with the same sha are idempotent
	// no-ops on the modification side (ON CONFLICT DO NOTHING).
	if sha == m.LastHeadSHA {
		return t.recordOutcome(ctx, m, now, sha, "")
	}

	// HEAD advanced — insert modification, create run. Both steps
	// are idempotent on their respective unique keys, so a crash
	// between them converges on the next tick.
	mod, err := t.store.InsertModification(ctx, store.Modification{
		MaterialID:  m.MaterialID,
		Revision:    sha,
		Branch:      branch,
		Author:      "poll",
		Message:     fmt.Sprintf("detected %s@%s via poll", branch, shortSHA(sha)),
		CommittedAt: now,
	})
	if err != nil {
		return fmt.Errorf("insert modification: %w", err)
	}

	causeDetail, _ := json.Marshal(map[string]any{
		"poll_interval_ns": int64(m.EffectiveInterval()),
		"polled_at":        now.UTC().Format(time.RFC3339),
		"prev_sha":         m.LastHeadSHA,
	})
	run, err := t.store.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     m.PipelineID,
		MaterialID:     m.MaterialID,
		ModificationID: mod.ID,
		Revision:       sha,
		Branch:         branch,
		Provider:       providerForCause(scm.Provider),
		Delivery:       fmt.Sprintf("poll:%d", now.Unix()),
		TriggeredBy:    "poll",
		Cause:          string(domain.CausePoll),
		CauseDetail:    causeDetail,
	})
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	if err := t.recordOutcome(ctx, m, now, sha, ""); err != nil {
		// Outcome-write failure is non-fatal: the run was created,
		// worst case we repoll next tick and InsertModification
		// short-circuits via ON CONFLICT.
		t.log.Warn("polling: record outcome after fire",
			"material_id", m.MaterialID, "err", err)
	}
	t.log.Info("polling: fired",
		"material_id", m.MaterialID,
		"pipeline_id", m.PipelineID,
		"run_id", run.RunID,
		"sha", shortSHA(sha),
		"prev_sha", shortSHA(m.LastHeadSHA))
	return nil
}

func (t *Ticker) recordOutcome(
	ctx context.Context, m store.PollableGitMaterial,
	now time.Time, sha, errMsg string,
) error {
	return t.store.RecordPollOutcome(ctx, store.PollOutcome{
		MaterialID: m.MaterialID,
		PolledAt:   now,
		HeadSHA:    sha,
		ErrorMsg:   errMsg,
	})
}

// materialToSCM synthesizes the store.SCMSource the Fetcher expects
// from what ListPollableGitMaterials returned. Returns ok=false
// when the project has no scm_source binding (detached) OR the
// material has no branch — we can't ask a provider about "some
// unspecified branch on a detached repo".
func materialToSCM(m store.PollableGitMaterial) (store.SCMSource, string, bool) {
	branch := m.Branch
	if branch == "" {
		branch = m.SCMDefaultBranch
	}
	if branch == "" {
		return store.SCMSource{}, "", false
	}
	if m.SCMProvider == "" || m.SCMURL == "" {
		return store.SCMSource{}, "", false
	}
	return store.SCMSource{
		ID:            uuid.Nil, // Fetcher uses URL+AuthRef, not ID
		ProjectID:     m.ProjectID,
		Provider:      m.SCMProvider,
		URL:           m.SCMURL,
		DefaultBranch: m.SCMDefaultBranch,
		AuthRef:       m.SCMAuthRef,
	}, branch, true
}

// providerForCause maps the scm_source provider name to the value
// stored in runs.provider. Empty falls back to "poll" so the run
// is still attributable to this subsystem.
func providerForCause(p string) string {
	if p == "" {
		return "poll"
	}
	return p
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
