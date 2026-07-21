package store

// deploy_targets — the platform-registered "how does this environment deploy?"
// descriptor for the native deployment provider (ADR-0001). Persistence + resolve
// live here; the write-time validations (cluster->project authz, multi-source
// rejection) and the admin API land in sibling code that calls these.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ErrDeployTargetNotFound is returned when a project/environment has no target.
var ErrDeployTargetNotFound = errors.New("store: deploy target not found")

// ErrDeployTargetConflict is returned by UpsertDeployTargetGuarded when the row's
// gate/routing changed between the caller's separation-of-duties read and this write
// (the TOCTOU backstop) — the write is refused so a stale non-admin request can't
// clobber a concurrent admin gate change. The caller maps it to 409.
var ErrDeployTargetConflict = errors.New("store: deploy target changed since authorization")

// DeployTargetSoDGuard is the prior (SoD-authorized) gate + routing that a non-admin
// upsert must still match at write time. Captured at the SoD read; if the row no longer
// matches, UpsertDeployTargetGuarded refuses the write (ErrDeployTargetConflict).
type DeployTargetSoDGuard struct {
	Gate             *GoverningGate
	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
}

// DeleteTargetOutcome is the atomic result of DeleteUngatedDeployTargetByEnvironment.
type DeleteTargetOutcome string

const (
	DeleteTargetDeleted DeleteTargetOutcome = "deleted" // was ungated, removed
	DeleteTargetGated   DeleteTargetOutcome = "gated"   // exists + gated → admin-only (403)
	DeleteTargetAbsent  DeleteTargetOutcome = "absent"  // no such target (404)
)

// DeployTarget is a resolved target: the registered row joined to its environment
// so it carries the owning project + environment name the provider needs.
type DeployTarget struct {
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID // populated by ResolveDeployTarget (for the revision FK)
	Environment   string
	Provider      string
	Cluster       string
	Application   string
	Namespace     string
	SyncMode      string
	// Rollout awareness (Phase 2). RolloutCluster/Namespace/Name empty = defaults
	// (App's cluster / auto-discover).
	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
	// GoverningGate is the approval-gate config (nil = no gate = observe-only). Its
	// presence puts the target in control mode.
	GoverningGate *GoverningGate
}

// DeployTargetInput is the write shape for registering/updating a target. The
// caller resolves EnvironmentID (via EnsureEnvironment) first.
type DeployTargetInput struct {
	EnvironmentID uuid.UUID
	Provider      string
	Cluster       string
	Application   string
	Namespace     string
	SyncMode      string
	CreatedBy     string

	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
	GoverningGate    *GoverningGate
}

// ResolveDeployTarget looks up the target for a project's environment by name.
func (s *Store) ResolveDeployTarget(ctx context.Context, projectID uuid.UUID, envName string) (DeployTarget, error) {
	row, err := s.q.ResolveDeployTarget(ctx, db.ResolveDeployTargetParams{
		ProjectID: pgUUID(projectID),
		Name:      envName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployTarget{}, ErrDeployTargetNotFound
	}
	if err != nil {
		return DeployTarget{}, fmt.Errorf("store: resolve deploy target %s/%s: %w", projectID, envName, err)
	}
	gate, err := unmarshalGoverningGate(row.GoverningGate)
	if err != nil {
		return DeployTarget{}, fmt.Errorf("store: resolve deploy target %s/%s: %w", projectID, envName, err)
	}
	return DeployTarget{
		ProjectID:        fromPgUUID(row.ProjectID),
		EnvironmentID:    fromPgUUID(row.EnvironmentID),
		Environment:      row.Environment,
		Provider:         row.Provider,
		Cluster:          row.Cluster,
		Application:      row.Application,
		Namespace:        row.Namespace,
		SyncMode:         row.SyncMode,
		RolloutAware:     row.RolloutAware,
		RolloutCluster:   stringValue(row.RolloutCluster),
		RolloutNamespace: stringValue(row.RolloutNamespace),
		RolloutName:      stringValue(row.RolloutName),
		GoverningGate:    gate,
	}, nil
}

// UpsertDeployTarget registers or updates the target for an environment (1:1). The
// caller's separation-of-duties check (a gate/routing change on a gated target is
// admin-only) runs BEFORE this write — the store just persists.
func (s *Store) UpsertDeployTarget(ctx context.Context, in DeployTargetInput) error {
	gate, err := marshalGoverningGate(in.GoverningGate)
	if err != nil {
		return err
	}
	if _, err := s.q.UpsertDeployTarget(ctx, db.UpsertDeployTargetParams{
		EnvironmentID:    pgUUID(in.EnvironmentID),
		Provider:         in.Provider,
		Cluster:          in.Cluster,
		Application:      in.Application,
		Namespace:        in.Namespace,
		SyncMode:         in.SyncMode,
		CreatedBy:        in.CreatedBy,
		RolloutAware:     in.RolloutAware,
		RolloutCluster:   nullableString(in.RolloutCluster),
		RolloutNamespace: nullableString(in.RolloutNamespace),
		RolloutName:      nullableString(in.RolloutName),
		GoverningGate:    gate,
	}); err != nil {
		return fmt.Errorf("store: upsert deploy target: %w", err)
	}
	return nil
}

// UpsertDeployTargetGuarded is the non-admin upsert: the update applies only if the
// row's gate + routing still equal the SoD-authorized snapshot (guard). A 0-row result
// (the guard failed → a concurrent admin gate/routing change) maps to
// ErrDeployTargetConflict. A fresh create (no conflict) is unguarded and succeeds.
func (s *Store) UpsertDeployTargetGuarded(ctx context.Context, in DeployTargetInput, guard DeployTargetSoDGuard) error {
	gate, err := marshalGoverningGate(in.GoverningGate)
	if err != nil {
		return err
	}
	expected, err := marshalGoverningGate(guard.Gate)
	if err != nil {
		return err
	}
	_, err = s.q.UpsertDeployTargetGuarded(ctx, db.UpsertDeployTargetGuardedParams{
		EnvironmentID:            pgUUID(in.EnvironmentID),
		Provider:                 in.Provider,
		Cluster:                  in.Cluster,
		Application:              in.Application,
		Namespace:                in.Namespace,
		SyncMode:                 in.SyncMode,
		CreatedBy:                in.CreatedBy,
		RolloutAware:             in.RolloutAware,
		RolloutCluster:           nullableString(in.RolloutCluster),
		RolloutNamespace:         nullableString(in.RolloutNamespace),
		RolloutName:              nullableString(in.RolloutName),
		GoverningGate:            gate,
		ExpectedGate:             expected,
		ExpectedRolloutAware:     guard.RolloutAware,
		ExpectedRolloutCluster:   nullableString(guard.RolloutCluster),
		ExpectedRolloutNamespace: nullableString(guard.RolloutNamespace),
		ExpectedRolloutName:      nullableString(guard.RolloutName),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeployTargetConflict
	}
	if err != nil {
		return fmt.Errorf("store: upsert deploy target (guarded): %w", err)
	}
	return nil
}

// DeployTargetListItem is one row for the per-project deploy-targets listing.
type DeployTargetListItem struct {
	ID          uuid.UUID
	Environment string
	Provider    string
	Cluster     string
	Application string
	Namespace   string
	SyncMode    string

	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
	GoverningGate    *GoverningGate
}

// ListDeployTargets returns a project's registered targets, ordered by environment.
func (s *Store) ListDeployTargets(ctx context.Context, projectID uuid.UUID) ([]DeployTargetListItem, error) {
	rows, err := s.q.ListDeployTargetsForProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list deploy targets: %w", err)
	}
	out := make([]DeployTargetListItem, 0, len(rows))
	for _, r := range rows {
		gate, err := unmarshalGoverningGate(r.GoverningGate)
		if err != nil {
			return nil, fmt.Errorf("store: list deploy targets: %w", err)
		}
		out = append(out, DeployTargetListItem{
			ID:               fromPgUUID(r.ID),
			Environment:      r.Environment,
			Provider:         r.Provider,
			Cluster:          r.Cluster,
			Application:      r.Application,
			Namespace:        r.Namespace,
			SyncMode:         r.SyncMode,
			RolloutAware:     r.RolloutAware,
			RolloutCluster:   stringValue(r.RolloutCluster),
			RolloutNamespace: stringValue(r.RolloutNamespace),
			RolloutName:      stringValue(r.RolloutName),
			GoverningGate:    gate,
		})
	}
	return out, nil
}

// DeleteDeployTargetByEnvironment removes a project environment's target. Returns
// whether a row was deleted (false → nothing to delete → the handler 404s).
func (s *Store) DeleteDeployTargetByEnvironment(ctx context.Context, projectID uuid.UUID, envName string) (bool, error) {
	n, err := s.q.DeleteDeployTargetByEnvironment(ctx, db.DeleteDeployTargetByEnvironmentParams{
		ProjectID: pgUUID(projectID),
		Name:      envName,
	})
	if err != nil {
		return false, fmt.Errorf("store: delete deploy target %s/%s: %w", projectID, envName, err)
	}
	return n > 0, nil
}

// DeleteUngatedDeployTargetByEnvironment is the non-admin delete: it removes the target
// only if it is UNGATED and reports the outcome (deleted | gated | absent), so the
// handler needn't do a separate racy read to distinguish 403 (gated) from 404. The
// ungated-check runs on the row LOCKED FOR UPDATE in the SAME tx as the delete, so a
// concurrent admin gate-add can't slip in between the check and the delete (it blocks on
// the lock, or is seen post-commit as gated) — no TOCTOU.
func (s *Store) DeleteUngatedDeployTargetByEnvironment(ctx context.Context, projectID uuid.UUID, envName string) (DeleteTargetOutcome, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("store: delete ungated deploy target begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.LockDeployTargetForDelete(ctx, db.LockDeployTargetForDeleteParams{
		ProjectID: pgUUID(projectID),
		Name:      envName,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeleteTargetAbsent, nil // nothing to delete (tx rolls back — no writes)
	}
	if err != nil {
		return "", fmt.Errorf("store: lock deploy target %s/%s: %w", projectID, envName, err)
	}
	// Gate check on the LOCKED row. len(row.GoverningGate) > 0 means non-NULL jsonb.
	if len(row.GoverningGate) > 0 {
		return DeleteTargetGated, nil // deleting a gated target is admin-only
	}
	if _, err := q.DeleteDeployTargetByID(ctx, row.ID); err != nil {
		return "", fmt.Errorf("store: delete deploy target %s/%s: %w", projectID, envName, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: delete ungated deploy target commit: %w", err)
	}
	return DeleteTargetDeleted, nil
}

// CountDeployTargetsForCluster backs the cluster delete-guard.
func (s *Store) CountDeployTargetsForCluster(ctx context.Context, cluster string) (int64, error) {
	n, err := s.q.CountDeployTargetsForCluster(ctx, cluster)
	if err != nil {
		return 0, fmt.Errorf("store: count deploy targets for cluster %q: %w", cluster, err)
	}
	return n, nil
}
