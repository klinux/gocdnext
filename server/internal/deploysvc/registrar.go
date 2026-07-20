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
	// AuthorizeClusterForProject validates a rollout_cluster reference (existence +
	// allowed_projects) oracle-safely before the write.
	AuthorizeClusterForProject(context.Context, uuid.UUID, string) error
	// ResolveDeployTarget reads the current target (for the separation-of-duties check
	// — has it a gate? is the routing changing?). Returns store.ErrDeployTargetNotFound
	// when the environment has no target yet (a create).
	ResolveDeployTarget(context.Context, uuid.UUID, string) (store.DeployTarget, error)
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
	// GoverningGate is the approval-gate config (nil = no gate). Setting/changing it,
	// and changing the rollout routing on a gated target, is ADMIN-only (separation of
	// duties — see Register). Requires RolloutAware (a gate with nothing to observe is
	// rejected).
	GoverningGate *store.GoverningGate
	// CallerIsAdmin is the caller's admin status, resolved by the handler from the auth
	// context (true when auth is disabled). Gates the separation-of-duties check.
	CallerIsAdmin bool
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
	// Rollout routing is meaningless when observation is off — drop it so a disabled
	// route can't authorize/persist a cluster reference (which the FK would then let
	// block that cluster's delete).
	if !in.RolloutAware {
		rolloutCluster, rolloutNamespace, rolloutName = "", "", ""
	}

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
	// A gate with nothing to observe is meaningless: governing_gate requires
	// rollout_aware. (Disabling rollout_aware on a gated target is itself caught by the
	// routing SoD check below, so a maintainer can't strip the gate that way either.)
	if in.GoverningGate != nil && !in.RolloutAware {
		return store.DeployTarget{}, &Fault{Kind: FaultInvalid, Err: errors.New(
			"deploysvc: governing_gate requires rollout_aware")}
	}
	if err := in.GoverningGate.Validate(); err != nil {
		return store.DeployTarget{}, &Fault{Kind: FaultInvalid, Err: err}
	}

	// Separation of duties (admin-only edits on a gated target). A maintainer must not
	// be able to add/remove/change a gate, nor reroute the rollout of a target that has
	// (or would have) a gate — either would defeat the gate they can't drop directly.
	// Non-gate, non-routing fields (application, cluster, sync_mode) stay
	// maintainer-editable. Skipped for admins (and auth-disabled). Runs AFTER routing
	// normalization so the comparison is against canonical values, and BEFORE any write.
	if err := r.enforceGateSoD(ctx, in, rolloutCluster, rolloutNamespace, rolloutName); err != nil {
		return store.DeployTarget{}, err
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
	// If a rollout cluster is pinned, authorize it too (oracle-safe) — the write
	// mustn't persist a reference to a cluster the project isn't allowed. Same
	// collapsed missing-vs-unauthorized message as the Application cluster.
	if rolloutCluster != "" {
		if err := r.registry.AuthorizeClusterForProject(ctx, in.ProjectID, rolloutCluster); err != nil {
			return store.DeployTarget{}, classifyValidateErr(err)
		}
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
		GoverningGate:    in.GoverningGate,
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
		GoverningGate:    in.GoverningGate,
	}, nil
}

// enforceGateSoD applies the separation-of-duties rule: on a target that has (or would
// have) a governing_gate, changing the gate OR the rollout routing is admin-only. The
// routing args are the already-normalized values (post rollout_aware clearing). A
// non-admin request that would change either → 403; everything else (admins,
// auth-disabled, non-gated routing edits, non-gate field edits on a gated target) is
// allowed. Reads the current target once (only for non-admins).
func (r *Registrar) enforceGateSoD(ctx context.Context, in RegisterInput, rolloutCluster, rolloutNamespace, rolloutName string) error {
	if in.CallerIsAdmin {
		return nil
	}
	existing, err := r.registry.ResolveDeployTarget(ctx, in.ProjectID, strings.TrimSpace(in.Environment))
	haveExisting := err == nil
	if err != nil && !errors.Is(err, store.ErrDeployTargetNotFound) {
		return &Fault{Kind: FaultInternal, Err: fmt.Errorf("deploysvc: resolve target for authz: %w", err)}
	}

	var existingGate *store.GoverningGate
	if haveExisting {
		existingGate = existing.GoverningGate
	}
	// (1) The gate itself. A change includes create (nil->set), remove (set->nil), and
	// any field edit. Order-sensitive compare; a maintainer's UI round-trips the stored
	// gate verbatim, so an unchanged resubmit compares equal.
	if !store.GoverningGateEqual(existingGate, in.GoverningGate) {
		return &Fault{Kind: FaultForbidden, Err: errors.New(
			"deploysvc: changing the approval gate requires admin")}
	}
	// (2) The rollout routing, but only when a gate is in play (existing or new — which,
	// past check (1), are the same gate). An ungated target's routing stays
	// maintainer-editable, exactly as in PR1.
	if existingGate == nil && in.GoverningGate == nil {
		return nil
	}
	routingChanged := !haveExisting ||
		existing.RolloutAware != in.RolloutAware ||
		existing.RolloutCluster != rolloutCluster ||
		existing.RolloutNamespace != rolloutNamespace ||
		existing.RolloutName != rolloutName
	if routingChanged {
		return &Fault{Kind: FaultForbidden, Err: errors.New(
			"deploysvc: changing rollout routing on a gated target requires admin")}
	}
	return nil
}
