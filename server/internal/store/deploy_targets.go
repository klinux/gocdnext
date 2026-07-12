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

// DeployTarget is a resolved target: the registered row joined to its environment
// so it carries the owning project + environment name the provider needs.
type DeployTarget struct {
	ProjectID   uuid.UUID
	Environment string
	Provider    string
	Cluster     string
	Application  string
	Namespace   string
	SyncMode    string
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
	return DeployTarget{
		ProjectID:   fromPgUUID(row.ProjectID),
		Environment: row.Environment,
		Provider:    row.Provider,
		Cluster:     row.Cluster,
		Application:  row.Application,
		Namespace:   row.Namespace,
		SyncMode:    row.SyncMode,
	}, nil
}

// UpsertDeployTarget registers or updates the target for an environment (1:1).
func (s *Store) UpsertDeployTarget(ctx context.Context, in DeployTargetInput) error {
	if _, err := s.q.UpsertDeployTarget(ctx, db.UpsertDeployTargetParams{
		EnvironmentID: pgUUID(in.EnvironmentID),
		Provider:      in.Provider,
		Cluster:       in.Cluster,
		Application:   in.Application,
		Namespace:     in.Namespace,
		SyncMode:      in.SyncMode,
		CreatedBy:     in.CreatedBy,
	}); err != nil {
		return fmt.Errorf("store: upsert deploy target: %w", err)
	}
	return nil
}

// CountDeployTargetsForCluster backs the cluster delete-guard.
func (s *Store) CountDeployTargetsForCluster(ctx context.Context, cluster string) (int64, error) {
	n, err := s.q.CountDeployTargetsForCluster(ctx, cluster)
	if err != nil {
		return 0, fmt.Errorf("store: count deploy targets for cluster %q: %w", cluster, err)
	}
	return n, nil
}
