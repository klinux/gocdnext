package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// PollableGitMaterial is the decoded view the poll worker iterates
// on each tick: one row per git material in the system plus the
// parent project's scm_source context and the last poll outcome.
//
// Two fields drive poll decisions:
//   - MaterialPollInterval: duration declared in the material's
//     YAML (poll_interval:). Zero = not set at material level.
//   - ProjectPollInterval: scm_source fallback (set at project
//     level via /settings). Zero = not set at project level.
//
// Effective interval is MaterialPollInterval when non-zero, else
// ProjectPollInterval. The worker skips materials where both are
// zero, and also skips when the scm_source binding is missing
// (can't ask a provider about a detached repo).
//
// SCMProvider / SCMURL / SCMAuthRef / DefaultBranch may be empty
// when the project has no scm_source bound yet.
type PollableGitMaterial struct {
	MaterialID uuid.UUID
	PipelineID uuid.UUID
	ProjectID  uuid.UUID

	URL                  string
	Branch               string
	MaterialPollInterval time.Duration

	LastPolledAt  *time.Time
	LastHeadSHA   string
	LastPollError string

	SCMProvider         string
	SCMURL              string
	SCMAuthRef          string
	SCMDefaultBranch    string
	ProjectPollInterval time.Duration
}

// EffectiveInterval applies the material > project fallback and
// returns zero when no polling is configured.
func (p PollableGitMaterial) EffectiveInterval() time.Duration {
	if p.MaterialPollInterval > 0 {
		return p.MaterialPollInterval
	}
	return p.ProjectPollInterval
}

// IsDue returns true when this material is owed a poll according
// to its effective interval and last-polled stamp. A material
// that's never been polled (LastPolledAt == nil) is always due.
func (p PollableGitMaterial) IsDue(now time.Time) bool {
	interval := p.EffectiveInterval()
	if interval <= 0 {
		return false
	}
	if p.LastPolledAt == nil {
		return true
	}
	return !p.LastPolledAt.Add(interval).After(now)
}

// ListPollableGitMaterials loads every git material + its poll
// state + scm_source context. Filtering to "materials with any
// polling configured" happens in the caller because the effective
// interval straddles two data sources; see EffectiveInterval.
func (s *Store) ListPollableGitMaterials(ctx context.Context) ([]PollableGitMaterial, error) {
	rows, err := s.q.ListPollableGitMaterials(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list pollable git materials: %w", err)
	}
	out := make([]PollableGitMaterial, 0, len(rows))
	for _, r := range rows {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			return nil, fmt.Errorf("store: decode git material %s: %w",
				fromPgUUID(r.ID), err)
		}
		item := PollableGitMaterial{
			MaterialID:           fromPgUUID(r.ID),
			PipelineID:           fromPgUUID(r.PipelineID),
			ProjectID:            fromPgUUID(r.ProjectID),
			URL:                  cfg.URL,
			Branch:               cfg.Branch,
			MaterialPollInterval: cfg.PollInterval,
			LastPolledAt:         pgTimePtr(r.LastPolledAt),
			LastHeadSHA:          derefStr(r.LastHeadSha),
			LastPollError:        derefStr(r.LastPollError),
			SCMProvider:          derefStr(r.Provider),
			SCMURL:               derefStr(r.Url),
			SCMAuthRef:           derefStr(r.AuthRef),
			SCMDefaultBranch:     derefStr(r.DefaultBranch),
		}
		if r.ProjectPollIntervalNs != nil {
			item.ProjectPollInterval = time.Duration(*r.ProjectPollIntervalNs)
		}
		out = append(out, item)
	}
	return out, nil
}

// PollOutcome is what UpsertMaterialPollState records for one
// poll attempt. HeadSHA is the last-known-good value whether
// the poll succeeded or errored; ErrorMsg is non-empty only on
// failure (success callers pass "").
type PollOutcome struct {
	MaterialID uuid.UUID
	PolledAt   time.Time
	HeadSHA    string
	ErrorMsg   string
}

// RecordPollOutcome writes the outcome of a poll attempt. The
// caller holds the "last known good SHA" contract: pass the new
// SHA on success, or repeat the previous SHA on failure so the
// UI keeps showing the last-known-good.
func (s *Store) RecordPollOutcome(ctx context.Context, o PollOutcome) error {
	var sha, errMsg *string
	if o.HeadSHA != "" {
		s := o.HeadSHA
		sha = &s
	}
	if o.ErrorMsg != "" {
		e := o.ErrorMsg
		errMsg = &e
	}
	err := s.q.UpsertMaterialPollState(ctx, db.UpsertMaterialPollStateParams{
		MaterialID:    pgUUID(o.MaterialID),
		LastPolledAt:  pgtype.Timestamptz{Time: o.PolledAt, Valid: true},
		LastHeadSha:   sha,
		LastPollError: errMsg,
	})
	if err != nil {
		return fmt.Errorf("store: record poll outcome: %w", err)
	}
	return nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
