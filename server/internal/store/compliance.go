package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/compliance"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

var (
	// ErrFrameworkNotFound / ErrPolicyNotFound map a missing row to a 404.
	ErrFrameworkNotFound = errors.New("store: compliance framework not found")
	ErrPolicyNotFound    = errors.New("store: compliance policy not found")
	// ErrFrameworkInUse blocks deleting a framework still assigned/targeted.
	ErrFrameworkInUse = errors.New("store: compliance framework in use")
	// ErrComplianceWouldDropEnforcement blocks an apply that would leave a
	// policy-governed project with zero pipelines (a compliance bypass).
	ErrComplianceWouldDropEnforcement = errors.New("store: apply would drop compliance enforcement")
)

// ComplianceFramework is an admin-defined label assigned to projects.
type ComplianceFramework struct {
	ID          string
	Name        string
	Description string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// FrameworkInput is the create/update payload for a framework.
type FrameworkInput struct {
	Name        string
	Description string
	CreatedBy   string
}

// FrameworkUsage is the delete-guard counter.
type FrameworkUsage struct {
	Projects int64
	Policies int64
}

// CompliancePolicy is an admin-defined, framework-scoped pipeline policy.
type CompliancePolicy struct {
	ID             string
	Name           string
	Description    string
	Enabled        bool
	Mode           string
	Priority       int
	AppliesToAll   bool
	PositionBefore string
	PositionAfter  string
	FrameworkIDs   []string
	ConfigYAML     string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PolicyInput is the create/update payload for a policy. ConfigYAML is the
// pipeline-schema source; it is compiled + validated (reserved-prefix names)
// before storage.
type PolicyInput struct {
	Name           string
	Description    string
	Enabled        bool
	Mode           string
	Priority       int
	AppliesToAll   bool
	PositionBefore string
	PositionAfter  string
	FrameworkIDs   []string
	ConfigYAML     string
	CreatedBy      string
}

// ProjectIDBySlug resolves a project slug to its id for the framework
// assignment endpoints. Returns ErrProjectNotFound when absent.
func (s *Store) ProjectIDBySlug(ctx context.Context, slug string) (uuid.UUID, error) {
	proj, err := s.q.GetProjectBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrProjectNotFound
		}
		return uuid.Nil, fmt.Errorf("store: project by slug: %w", err)
	}
	return fromPgUUID(proj.ID), nil
}

// ProjectHasCompliancePolicies reports whether any enabled policy applies to a
// project (global or via an assigned framework). Used by the webhook layer to
// refuse honouring `[skip ci]` on a governed project — enforced policies must
// not be bypassable.
func (s *Store) ProjectHasCompliancePolicies(ctx context.Context, projectID uuid.UUID) (bool, error) {
	rows, err := s.q.ResolvePoliciesForProject(ctx, pgUUID(projectID))
	if err != nil {
		return false, fmt.Errorf("store: project compliance policies: %w", err)
	}
	return len(rows) > 0, nil
}

// ---- frameworks ----------------------------------------------------------

func (s *Store) ListComplianceFrameworks(ctx context.Context) ([]ComplianceFramework, error) {
	rows, err := s.q.ListComplianceFrameworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list frameworks: %w", err)
	}
	out := make([]ComplianceFramework, 0, len(rows))
	for _, r := range rows {
		out = append(out, ComplianceFramework{
			ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
			CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (s *Store) InsertComplianceFramework(ctx context.Context, in FrameworkInput) (ComplianceFramework, error) {
	if in.Name == "" {
		return ComplianceFramework{}, fmt.Errorf("store: framework name is required")
	}
	r, err := s.q.InsertComplianceFramework(ctx, db.InsertComplianceFrameworkParams{
		Name: in.Name, Description: in.Description, CreatedBy: in.CreatedBy,
	})
	if err != nil {
		return ComplianceFramework{}, fmt.Errorf("store: insert framework: %w", err)
	}
	return ComplianceFramework{
		ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

// UpdateComplianceFramework changes name/description. A rename doesn't affect
// any effective definition (policies match frameworks by id), so no recompute.
func (s *Store) UpdateComplianceFramework(ctx context.Context, id string, in FrameworkInput) error {
	fwID, err := uuid.Parse(id)
	if err != nil {
		return ErrFrameworkNotFound
	}
	if in.Name == "" {
		return fmt.Errorf("store: framework name is required")
	}
	n, err := s.q.UpdateComplianceFramework(ctx, db.UpdateComplianceFrameworkParams{
		ID: pgUUID(fwID), Name: in.Name, Description: in.Description,
	})
	if err != nil {
		return fmt.Errorf("store: update framework: %w", err)
	}
	if n == 0 {
		return ErrFrameworkNotFound
	}
	return nil
}

// DeleteComplianceFramework removes a framework. Projects carrying it are
// recomputed (they lose any policy that targeted only this framework) before
// the cascade drops the assignments. Blocked while still in use unless force.
func (s *Store) DeleteComplianceFramework(ctx context.Context, id string) error {
	fwID, err := uuid.Parse(id)
	if err != nil {
		return ErrFrameworkNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: delete framework: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockComplianceExclusive(ctx, tx); err != nil {
		return err
	}
	// Capture affected projects BEFORE the cascade removes the assignments.
	affected, err := q.ListProjectIDsByFrameworks(ctx, []pgtype.UUID{pgUUID(fwID)})
	if err != nil {
		return fmt.Errorf("store: framework affected projects: %w", err)
	}
	if err := q.DeleteComplianceFramework(ctx, pgUUID(fwID)); err != nil {
		return fmt.Errorf("store: delete framework: %w", err)
	}
	if err := recomputeProjects(ctx, q, affected); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete framework: commit: %w", err)
	}
	return nil
}

func (s *Store) FrameworkUsage(ctx context.Context, id string) (FrameworkUsage, error) {
	fwID, err := uuid.Parse(id)
	if err != nil {
		return FrameworkUsage{}, ErrFrameworkNotFound
	}
	r, err := s.q.CountFrameworkUsage(ctx, pgUUID(fwID))
	if err != nil {
		return FrameworkUsage{}, fmt.Errorf("store: framework usage: %w", err)
	}
	return FrameworkUsage{Projects: r.ProjectCount, Policies: r.PolicyCount}, nil
}

// ---- project ↔ framework assignment --------------------------------------

func (s *Store) ListProjectFrameworks(ctx context.Context, projectID uuid.UUID) ([]ComplianceFramework, error) {
	rows, err := s.q.ListFrameworksByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list project frameworks: %w", err)
	}
	out := make([]ComplianceFramework, 0, len(rows))
	for _, r := range rows {
		out = append(out, ComplianceFramework{
			ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
			CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// SetProjectFrameworks replaces a project's framework set and recomputes its
// effective pipeline definitions in the same tx.
func (s *Store) SetProjectFrameworks(ctx context.Context, projectID uuid.UUID, frameworkIDs []string) error {
	fwIDs, err := parseUUIDs(frameworkIDs)
	if err != nil {
		return fmt.Errorf("store: set project frameworks: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: set project frameworks: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockComplianceExclusive(ctx, tx); err != nil {
		return err
	}
	if err := q.DeleteProjectFrameworks(ctx, pgUUID(projectID)); err != nil {
		return fmt.Errorf("store: clear project frameworks: %w", err)
	}
	for _, fw := range fwIDs {
		if err := q.InsertProjectFramework(ctx, db.InsertProjectFrameworkParams{
			ProjectID: pgUUID(projectID), FrameworkID: fw,
		}); err != nil {
			return fmt.Errorf("store: assign framework: %w", err)
		}
	}
	if err := recomputeProjectEffective(ctx, q, pgUUID(projectID)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set project frameworks: commit: %w", err)
	}
	return nil
}

// ---- policies ------------------------------------------------------------

func (s *Store) ListCompliancePolicies(ctx context.Context) ([]CompliancePolicy, error) {
	rows, err := s.q.ListCompliancePolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list policies: %w", err)
	}
	out := make([]CompliancePolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, CompliancePolicy{
			ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
			Enabled: r.Enabled, Mode: r.Mode, Priority: int(r.Priority),
			AppliesToAll: r.AppliesToAll, PositionBefore: r.PositionBefore,
			PositionAfter: r.PositionAfter, CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (s *Store) GetCompliancePolicy(ctx context.Context, id string) (CompliancePolicy, error) {
	pID, err := uuid.Parse(id)
	if err != nil {
		return CompliancePolicy{}, ErrPolicyNotFound
	}
	r, err := s.q.GetCompliancePolicy(ctx, pgUUID(pID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CompliancePolicy{}, ErrPolicyNotFound
		}
		return CompliancePolicy{}, fmt.Errorf("store: get policy: %w", err)
	}
	fwIDs, err := s.q.ListFrameworkIDsByPolicy(ctx, pgUUID(pID))
	if err != nil {
		return CompliancePolicy{}, fmt.Errorf("store: policy frameworks: %w", err)
	}
	return CompliancePolicy{
		ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
		Enabled: r.Enabled, Mode: r.Mode, Priority: int(r.Priority),
		AppliesToAll: r.AppliesToAll, PositionBefore: r.PositionBefore,
		PositionAfter: r.PositionAfter, FrameworkIDs: uuidStrings(fwIDs),
		ConfigYAML: r.ConfigYaml, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

// validatePolicyInput normalises and validates a policy payload, returning the
// compiled pipeline and its config JSON (the pipeline-schema source compiled to
// domain form).
func validatePolicyInput(in *PolicyInput) (domain.Pipeline, []byte, error) {
	if in.Name == "" {
		return domain.Pipeline{}, nil, fmt.Errorf("store: policy name is required")
	}
	if in.Mode == "" {
		in.Mode = compliance.ModeInject
	}
	if in.Mode != compliance.ModeInject && in.Mode != compliance.ModeOverride {
		return domain.Pipeline{}, nil, fmt.Errorf("store: policy mode %q invalid (want inject|override)", in.Mode)
	}
	if in.PositionBefore != "" && in.PositionAfter != "" {
		return domain.Pipeline{}, nil, fmt.Errorf("store: policy position_before and position_after are mutually exclusive")
	}
	compiled, err := compliance.CompilePolicy(in.ConfigYAML)
	if err != nil {
		return domain.Pipeline{}, nil, err
	}
	cfg, err := json.Marshal(compiled)
	if err != nil {
		return domain.Pipeline{}, nil, fmt.Errorf("store: marshal policy config: %w", err)
	}
	return compiled, cfg, nil
}

// checkPolicyNameCollisions rejects a policy whose job/stage names overlap with
// any OTHER policy's. Reserved-prefixed names are a flat global namespace; two
// policies sharing one would materialise duplicate job_runs and the scheduler
// would pick whichever it encountered first. excludeID is the policy being
// updated (uuid.Nil on create).
func checkPolicyNameCollisions(ctx context.Context, q *db.Queries, excludeID pgtype.UUID, compiled domain.Pipeline) error {
	rows, err := q.ListPolicyConfigsExcept(ctx, excludeID)
	if err != nil {
		return fmt.Errorf("store: load policy configs: %w", err)
	}
	usedStage := map[string]string{}
	usedJob := map[string]string{}
	for _, r := range rows {
		var cfg domain.Pipeline
		if err := json.Unmarshal(r.Config, &cfg); err != nil {
			return fmt.Errorf("store: decode policy %q config: %w", r.Name, err)
		}
		for _, s := range cfg.Stages {
			usedStage[s] = r.Name
		}
		for _, j := range cfg.Jobs {
			usedJob[j.Name] = r.Name
		}
	}
	for _, s := range compiled.Stages {
		if other, ok := usedStage[s]; ok {
			return fmt.Errorf("store: stage %q already defined by policy %q", s, other)
		}
	}
	for _, j := range compiled.Jobs {
		if other, ok := usedJob[j.Name]; ok {
			return fmt.Errorf("store: job %q already defined by policy %q", j.Name, other)
		}
	}
	return nil
}

func (s *Store) InsertCompliancePolicy(ctx context.Context, in PolicyInput) (CompliancePolicy, error) {
	compiled, cfg, err := validatePolicyInput(&in)
	if err != nil {
		return CompliancePolicy{}, err
	}
	fwIDs, err := parseUUIDs(in.FrameworkIDs)
	if err != nil {
		return CompliancePolicy{}, fmt.Errorf("store: insert policy: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CompliancePolicy{}, fmt.Errorf("store: insert policy: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockComplianceExclusive(ctx, tx); err != nil {
		return CompliancePolicy{}, err
	}
	if err := checkPolicyNameCollisions(ctx, q, pgUUID(uuid.Nil), compiled); err != nil {
		return CompliancePolicy{}, err
	}

	r, err := q.InsertCompliancePolicy(ctx, db.InsertCompliancePolicyParams{
		Name: in.Name, Description: in.Description, Enabled: in.Enabled, Mode: in.Mode,
		Priority: int32(in.Priority), AppliesToAll: in.AppliesToAll,
		PositionBefore: in.PositionBefore, PositionAfter: in.PositionAfter,
		ConfigYaml: in.ConfigYAML, Config: cfg, CreatedBy: in.CreatedBy,
	})
	if err != nil {
		return CompliancePolicy{}, fmt.Errorf("store: insert policy: %w", err)
	}
	for _, fw := range fwIDs {
		if err := q.InsertPolicyFramework(ctx, db.InsertPolicyFrameworkParams{
			PolicyID: r.ID, FrameworkID: fw,
		}); err != nil {
			return CompliancePolicy{}, fmt.Errorf("store: link policy framework: %w", err)
		}
	}
	if in.Enabled {
		affected, err := affectedProjectIDs(ctx, q, in.AppliesToAll, fwIDs)
		if err != nil {
			return CompliancePolicy{}, err
		}
		if err := recomputeProjects(ctx, q, affected); err != nil {
			return CompliancePolicy{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CompliancePolicy{}, fmt.Errorf("store: insert policy: commit: %w", err)
	}
	return CompliancePolicy{
		ID: fromPgUUID(r.ID).String(), Name: r.Name, Description: r.Description,
		Enabled: r.Enabled, Mode: r.Mode, Priority: int(r.Priority),
		AppliesToAll: r.AppliesToAll, PositionBefore: r.PositionBefore,
		PositionAfter: r.PositionAfter, FrameworkIDs: in.FrameworkIDs,
		ConfigYAML: in.ConfigYAML, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func (s *Store) UpdateCompliancePolicy(ctx context.Context, id string, in PolicyInput) error {
	pID, err := uuid.Parse(id)
	if err != nil {
		return ErrPolicyNotFound
	}
	compiled, cfg, err := validatePolicyInput(&in)
	if err != nil {
		return err
	}
	newFW, err := parseUUIDs(in.FrameworkIDs)
	if err != nil {
		return fmt.Errorf("store: update policy: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: update policy: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockComplianceExclusive(ctx, tx); err != nil {
		return err
	}
	if err := checkPolicyNameCollisions(ctx, q, pgUUID(pID), compiled); err != nil {
		return err
	}

	// Old targeting, captured before the change, so projects that STOP being
	// covered get recomputed too.
	old, err := q.GetCompliancePolicy(ctx, pgUUID(pID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPolicyNotFound
		}
		return fmt.Errorf("store: update policy: load: %w", err)
	}
	oldFW, err := q.ListFrameworkIDsByPolicy(ctx, pgUUID(pID))
	if err != nil {
		return fmt.Errorf("store: update policy: old frameworks: %w", err)
	}

	n, err := q.UpdateCompliancePolicy(ctx, db.UpdateCompliancePolicyParams{
		ID: pgUUID(pID), Name: in.Name, Description: in.Description, Enabled: in.Enabled,
		Mode: in.Mode, Priority: int32(in.Priority), AppliesToAll: in.AppliesToAll,
		PositionBefore: in.PositionBefore, PositionAfter: in.PositionAfter,
		ConfigYaml: in.ConfigYAML, Config: cfg,
	})
	if err != nil {
		return fmt.Errorf("store: update policy: %w", err)
	}
	if n == 0 {
		return ErrPolicyNotFound
	}
	if err := q.DeletePolicyFrameworks(ctx, pgUUID(pID)); err != nil {
		return fmt.Errorf("store: update policy: clear frameworks: %w", err)
	}
	for _, fw := range newFW {
		if err := q.InsertPolicyFramework(ctx, db.InsertPolicyFrameworkParams{
			PolicyID: pgUUID(pID), FrameworkID: fw,
		}); err != nil {
			return fmt.Errorf("store: update policy: link framework: %w", err)
		}
	}
	// Recompute the union of old and new coverage so both newly-covered and
	// no-longer-covered projects refresh.
	affectedAll := old.AppliesToAll || in.AppliesToAll
	affected, err := affectedProjectIDs(ctx, q, affectedAll, mergeUUIDs(oldFW, newFW))
	if err != nil {
		return err
	}
	if err := recomputeProjects(ctx, q, affected); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: update policy: commit: %w", err)
	}
	return nil
}

func (s *Store) DeleteCompliancePolicy(ctx context.Context, id string) error {
	pID, err := uuid.Parse(id)
	if err != nil {
		return ErrPolicyNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: delete policy: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if err := lockComplianceExclusive(ctx, tx); err != nil {
		return err
	}
	old, err := q.GetCompliancePolicy(ctx, pgUUID(pID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPolicyNotFound
		}
		return fmt.Errorf("store: delete policy: load: %w", err)
	}
	oldFW, err := q.ListFrameworkIDsByPolicy(ctx, pgUUID(pID))
	if err != nil {
		return fmt.Errorf("store: delete policy: frameworks: %w", err)
	}
	affected, err := affectedProjectIDs(ctx, q, old.AppliesToAll, oldFW)
	if err != nil {
		return err
	}
	if err := q.DeleteCompliancePolicy(ctx, pgUUID(pID)); err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	if err := recomputeProjects(ctx, q, affected); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete policy: commit: %w", err)
	}
	return nil
}

// ---- advisory locking ----------------------------------------------------

// complianceLockKey is a fixed application-defined key for the global compliance
// advisory lock. ApplyProject takes it SHARED; every policy/framework/assignment
// mutation takes it EXCLUSIVE. This serialises the apply-vs-mutation race: an
// apply that resolves policies and writes the effective definition either fully
// precedes or fully follows a policy change, so it can never persist a stale
// effective definition that drops a just-added policy. The lock is held for the
// transaction (pg_advisory_xact_lock*) and released on commit/rollback. It only
// gates compliance writes + project applies — run dispatch never takes it, so CI
// keeps flowing during a recompute.
const complianceLockKey int64 = 0x636f6d706c69 // "compli"

func lockComplianceShared(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, complianceLockKey); err != nil {
		return fmt.Errorf("store: compliance shared lock: %w", err)
	}
	return nil
}

func lockComplianceExclusive(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, complianceLockKey); err != nil {
		return fmt.Errorf("store: compliance exclusive lock: %w", err)
	}
	return nil
}

// ---- helpers -------------------------------------------------------------

func parseUUIDs(ids []string) ([]pgtype.UUID, error) {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, s := range ids {
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("invalid uuid %q", s)
		}
		out = append(out, pgUUID(u))
	}
	return out, nil
}

func uuidStrings(ids []pgtype.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fromPgUUID(id).String())
	}
	return out
}

// mergeUUIDs unions two pgtype.UUID slices (dedup by string form).
func mergeUUIDs(a, b []pgtype.UUID) []pgtype.UUID {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]pgtype.UUID, 0, len(a)+len(b))
	for _, set := range [][]pgtype.UUID{a, b} {
		for _, id := range set {
			k := fromPgUUID(id).String()
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
