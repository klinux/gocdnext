package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// RequiredChecksConfig is the per-project "required pipelines for PR merge"
// config carried on projects.required_checks (JSONB). Its presence (non-nil)
// means gocdnext manages a dedicated GitHub ruleset for the repo that requires
// the listed pipelines' checks to pass before a PR can be merged. gocdnext does
// NOT perform the merge — it writes the ruleset; GitHub enforces the block.
//
// Pipelines carries the pipeline NAMES; the ruleset requires the corresponding
// commit-status contexts `ci/gocdnext/<slug>/<name>` (see checks.statusContext).
// RulesetID / SyncedAt / SyncError record the last reconcile so the UI can show
// synced / drift / a missing-permission failure without a silent half-apply.
type RequiredChecksConfig struct {
	Pipelines  []string   `json:"pipelines"`
	RulesetID  *int64     `json:"ruleset_id,omitempty"`
	SyncStatus string     `json:"sync_status,omitempty"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	SyncError  string     `json:"sync_error,omitempty"`
	// NeedsAdmin flags a sync failure caused by the App missing
	// Administration:write (so the UI can show the "re-approve the App" hint
	// rather than a generic error).
	NeedsAdmin bool `json:"needs_admin,omitempty"`
}

// RequiredChecks sync statuses (last reconcile outcome).
const (
	RequiredChecksPending = "pending" // saved, GitHub reconcile not yet run/succeeded
	RequiredChecksSynced  = "synced"  // ruleset written to match the config
	RequiredChecksFailed  = "failed"  // reconcile errored (config preserved)
	RequiredChecksSkipped = "skipped" // no App / not installed → nothing to write
)

// maxRequiredPipelines bounds the list so a project can't smuggle an unbounded
// blob into the JSONB column (GitHub also caps a ruleset's required checks).
const maxRequiredPipelines = 50

// Validate bounds the config shape (pure, no DB): names non-empty, unique, and
// within the cap. Cross-referential checks (pipeline exists, fires on PR, the
// project emits commit statuses) are DB-bound and live in
// ValidateRequiredPipelines. A nil config is valid (feature not configured).
func (c *RequiredChecksConfig) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Pipelines) > maxRequiredPipelines {
		return fmt.Errorf("store: too many required pipelines (%d > %d)", len(c.Pipelines), maxRequiredPipelines)
	}
	seen := make(map[string]struct{}, len(c.Pipelines))
	for _, name := range c.Pipelines {
		if name == "" {
			return errors.New("store: required pipeline name must not be empty")
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("store: duplicate required pipeline %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// GetProjectRequiredChecks returns the project's required-checks config, or nil
// when the feature is not configured (SQL NULL). ErrProjectNotFound when the
// slug matches no project.
func (s *Store) GetProjectRequiredChecks(ctx context.Context, slug string) (*RequiredChecksConfig, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT required_checks FROM projects WHERE slug = $1`, slug).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get project required checks: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var cfg *RequiredChecksConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("store: unmarshal required checks: %w", err)
	}
	return cfg, nil
}

// SaveProjectRequiredChecks persists the full config (pipelines + sync state) —
// the reconciler uses it to stamp the ruleset id / synced_at / sync_error after
// talking to GitHub. Bounds are re-validated (defense in depth); the DB-bound
// pipeline checks are the caller's job (ValidateRequiredPipelines). A raw Exec
// so RowsAffected distinguishes a missing project from a no-op update.
func (s *Store) SaveProjectRequiredChecks(ctx context.Context, slug string, cfg *RequiredChecksConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := marshalRequiredChecks(cfg)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET required_checks = $2 WHERE slug = $1`, slug, raw)
	if err != nil {
		return fmt.Errorf("store: save project required checks: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// ClearProjectRequiredChecks sets the column back to SQL NULL (feature not
// configured) — used once the reconciler has torn the ruleset down after the
// operator removed every required pipeline.
func (s *Store) ClearProjectRequiredChecks(ctx context.Context, slug string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE projects SET required_checks = NULL WHERE slug = $1`, slug)
	if err != nil {
		return fmt.Errorf("store: clear project required checks: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// SetProjectRequiredChecks validates the requested pipelines against the project
// (they exist, fire on PR, and the project emits commit statuses) then persists
// them, resetting the sync state to "pending" (nil SyncedAt, cleared error) while
// preserving any existing RulesetID so the reconciler upserts in place. An empty
// list clears the pipelines but keeps the RulesetID so the reconciler can tear
// the ruleset down. This is the pure-config write; the GitHub reconcile is a
// separate step (required_checks_sync.go).
func (s *Store) SetProjectRequiredChecks(ctx context.Context, slug string, pipelines []string) error {
	cfg := &RequiredChecksConfig{Pipelines: pipelines, SyncStatus: RequiredChecksPending}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := s.ValidateRequiredPipelines(ctx, slug, pipelines); err != nil {
		return err
	}
	existing, err := s.GetProjectRequiredChecks(ctx, slug)
	if err != nil {
		return err
	}
	if existing != nil {
		cfg.RulesetID = existing.RulesetID
	}
	return s.SaveProjectRequiredChecks(ctx, slug, cfg)
}

// ValidateRequiredPipelines rejects a required-checks request that would deadlock
// a merge: a pipeline that doesn't exist / doesn't fire on PR (its check never
// posts), or a project reporting only rich check runs (the required
// commit-status context `ci/gocdnext/...` is never posted). Returns
// ErrProjectNotFound for an unknown slug. An empty list always passes (it clears
// the requirement).
func (s *Store) ValidateRequiredPipelines(ctx context.Context, slug string, pipelines []string) error {
	if len(pipelines) == 0 {
		return nil
	}
	mode, err := s.GetProjectCheckReportingBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if mode == CheckReportingCheckRun {
		return fmt.Errorf(
			"%w: project reports only check runs (mode %q); required checks need the commit-status context, set reporting to %q or %q",
			ErrRequiredCheckUnreportable, mode, CheckReportingBoth, CheckReportingCommitStatus)
	}
	firing, err := s.ListPRFiringPipelineNames(ctx, slug)
	if err != nil {
		return err
	}
	eligible := make(map[string]struct{}, len(firing))
	for _, n := range firing {
		eligible[n] = struct{}{}
	}
	for _, name := range pipelines {
		if _, ok := eligible[name]; !ok {
			return fmt.Errorf("%w: pipeline %q does not fire on pull_request in project %q",
				ErrRequiredCheckUnreportable, name, slug)
		}
	}
	return nil
}

// ListPRFiringPipelineNames returns the project's pipelines SAFELY eligible to
// be required-for-merge: a git material on the SAME repo + default branch (by
// canonical fingerprint), firing on pull_request, with no path filter. A project
// with no SCM binding yields none (nothing to enforce).
func (s *Store) ListPRFiringPipelineNames(ctx context.Context, slug string) ([]string, error) {
	src, err := s.FindSCMSourceByProjectSlug(ctx, slug)
	if errors.Is(err, ErrSCMSourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: resolve scm source: %w", err)
	}
	names, err := s.q.ListPRFiringPipelineNames(ctx, db.ListPRFiringPipelineNamesParams{
		Slug:        slug,
		Fingerprint: FingerprintFor(src.URL, src.DefaultBranch),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list PR-firing pipelines: %w", err)
	}
	return names, nil
}

// ErrRequiredCheckUnreportable is returned when a requested required pipeline
// could never report a PR check (doesn't fire on PR, or the project emits no
// commit statuses) — accepting it would deadlock the merge.
var ErrRequiredCheckUnreportable = errors.New("store: required pipeline cannot report a PR check")

// ErrRequiredChecksNeedCommitStatus blocks moving a project to check_run-only
// reporting while it has required checks configured — the required contexts are
// commit statuses, which that mode stops posting, stranding the ruleset.
var ErrRequiredChecksNeedCommitStatus = errors.New("store: required checks need commit-status reporting")

func marshalRequiredChecks(c *RequiredChecksConfig) ([]byte, error) {
	if c == nil {
		return nil, nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("store: marshal required checks: %w", err)
	}
	return b, nil
}
