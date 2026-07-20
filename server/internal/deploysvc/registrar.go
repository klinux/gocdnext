// Package deploysvc is the deploy-target registration use case: it composes the
// deployment provider (Application inspection over the cluster transport) with the
// store (environment + target persistence). The orchestration lives here — not in
// the admin handler — because it has ordered external effects (validate → fetch →
// write), an authorization check implicit in the fetch, and a small transactional-
// ish write to the registry.
package deploysvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// Provider inspects an ArgoCD Application (existence, reachability, single-source),
// with the cluster->project authorization enforced inside the fetch. Satisfied by
// *deploy.ArgoProvider.
type Provider interface {
	ValidateSingleSource(context.Context, deploy.DeploymentTarget) error
}

// Registry persists environments + deploy targets. Satisfied by *store.Store.
type Registry interface {
	EnsureEnvironment(context.Context, uuid.UUID, string) (uuid.UUID, error)
	UpsertDeployTarget(context.Context, store.DeployTargetInput) error
}

// Registrar registers deploy targets.
type Registrar struct {
	provider Provider
	registry Registry
}

// New builds a Registrar over a provider + registry.
func New(p Provider, r Registry) *Registrar {
	return &Registrar{provider: p, registry: r}
}

// RegisterInput is the register-target request (from the admin API).
type RegisterInput struct {
	ProjectID   uuid.UUID
	Environment string
	Provider    string
	Cluster     string
	Application string
	Namespace   string
	SyncMode    string
	CreatedBy   string

	// Rollout awareness (Phase 2). Routing empty = defaults (App's cluster /
	// auto-discover). No register-time rollout validation in observe-only PR1: the
	// observe path resolves the Rollout each tick and degrades/fails-closed there.
	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
}

// Register validates and persists a deploy target. Order is load-bearing: field
// validation and the Application fetch (which also authorizes cluster->project and
// rejects multi-source) run BEFORE any DB write, so a bad/unauthorized/multi-source
// request never touches the registry. All string fields are trimmed here so the
// stored + fetched values are canonical (ValidateTargetFields only checks
// non-empty; it doesn't normalize).
func (r *Registrar) Register(ctx context.Context, in RegisterInput) (store.DeployTarget, error) {
	provider := strings.TrimSpace(in.Provider)
	cluster := strings.TrimSpace(in.Cluster)
	application := strings.TrimSpace(in.Application)
	environment := strings.TrimSpace(in.Environment)
	syncMode := strings.TrimSpace(in.SyncMode)
	namespace := deploy.NormalizeNamespace(in.Namespace)
	rolloutCluster := strings.TrimSpace(in.RolloutCluster)
	rolloutNamespace := strings.TrimSpace(in.RolloutNamespace)
	rolloutName := strings.TrimSpace(in.RolloutName)

	if in.ProjectID == uuid.Nil {
		return store.DeployTarget{}, &Fault{Kind: FaultInvalid, Err: errors.New("deploysvc: project is required")}
	}
	// Same bound the pipeline parser enforces on deploy.environment — so the API
	// can't register a target for a name no pipeline could reference, and a name
	// with '/' can't break the DELETE route.
	if !domain.ValidEnvironmentName(environment) {
		return store.DeployTarget{}, &Fault{Kind: FaultInvalid, Err: fmt.Errorf(
			"deploysvc: environment %q is invalid — start alphanumeric, then alphanumeric + . _ - (max 64)", in.Environment)}
	}
	if err := deploy.ValidateTargetFields(provider, cluster, application, syncMode); err != nil {
		return store.DeployTarget{}, &Fault{Kind: FaultInvalid, Err: err}
	}

	target := deploy.DeploymentTarget{
		ProjectID:        in.ProjectID,
		Environment:      environment,
		Provider:         provider,
		Cluster:          cluster,
		Application:      application,
		Namespace:        namespace,
		SyncMode:         deploy.SyncMode(syncMode),
		RolloutAware:     in.RolloutAware,
		RolloutCluster:   rolloutCluster,
		RolloutNamespace: rolloutNamespace,
		RolloutName:      rolloutName,
	}
	// Fetch + authorize + single-source check BEFORE any DB write. (Validates the
	// Application; the Rollout is resolved lazily at observe time.)
	if err := r.provider.ValidateSingleSource(ctx, target); err != nil {
		return store.DeployTarget{}, classifyValidateErr(err)
	}

	envID, err := r.registry.EnsureEnvironment(ctx, in.ProjectID, environment)
	if err != nil {
		return store.DeployTarget{}, &Fault{Kind: FaultInternal, Err: fmt.Errorf("deploysvc: ensure environment: %w", err)}
	}
	if err := r.registry.UpsertDeployTarget(ctx, store.DeployTargetInput{
		EnvironmentID:    envID,
		Provider:         provider,
		Cluster:          cluster,
		Application:      application,
		Namespace:        namespace,
		SyncMode:         syncMode,
		CreatedBy:        in.CreatedBy,
		RolloutAware:     in.RolloutAware,
		RolloutCluster:   rolloutCluster,
		RolloutNamespace: rolloutNamespace,
		RolloutName:      rolloutName,
	}); err != nil {
		return store.DeployTarget{}, &Fault{Kind: FaultInternal, Err: fmt.Errorf("deploysvc: upsert deploy target: %w", err)}
	}
	// Return the canonical (normalized) target so the caller needn't re-read it.
	return store.DeployTarget{
		ProjectID:        in.ProjectID,
		Environment:      environment,
		Provider:         provider,
		Cluster:          cluster,
		Application:      application,
		Namespace:        namespace,
		SyncMode:         syncMode,
		RolloutAware:     in.RolloutAware,
		RolloutCluster:   rolloutCluster,
		RolloutNamespace: rolloutNamespace,
		RolloutName:      rolloutName,
	}, nil
}
