package deploysvc

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// ReconcileDecision is the outcome taxonomy the scheduler acts on. Keeping these apart
// is the contract: collapsing them turns a benign CAS race into a failed job, or a real
// misconfiguration into an infinite retry.
type ReconcileDecision string

const (
	// ReconcileNoChange: the registered target already matches the declaration.
	ReconcileNoChange ReconcileDecision = "no_change"
	// ReconcileChanged: the target was created or updated.
	ReconcileChanged ReconcileDecision = "changed"
	// ReconcileConflictRetry: someone changed the row under us (lost CAS), or the row
	// moved between read and write. Do not dispatch; the next tick settles it.
	ReconcileConflictRetry ReconcileDecision = "conflict_retry"
	// ReconcileTerminalFault: the declaration cannot be applied and a retry would fail
	// identically — gated target, unauthorized cluster, invalid/missing Application.
	// Fail the job loud.
	ReconcileTerminalFault ReconcileDecision = "terminal_fault"
)

// DeclarativeReconcileInput is one job's declared target for one environment.
type DeclarativeReconcileInput struct {
	ProjectID   uuid.UUID
	Environment string
	Cluster     string
	Application string
	Namespace   string
	SyncMode    string
	// Actor labels the audit row for the write (see audit.EmitAs). A dispatch has no
	// user, so without it the change would be indistinguishable from any system event.
	Actor string
}

// DeclarativeResult carries the decision plus the message to surface on a fault.
type DeclarativeResult struct {
	Decision ReconcileDecision
	// Public is the caller-safe reason for a TerminalFault. Cluster problems collapse
	// to store.ClusterUnavailableMessage so the existence oracle stays closed.
	Public string
}

// declarativeAuthorizer is the narrow, credential-free cluster check for this path. It
// is deliberately NOT AuthorizeClusterForProject: that one serves the imperative
// registration path, and applying the self-service opt-in rule there would start
// refusing legitimate admin edits.
type declarativeAuthorizer interface {
	AuthorizeDeclarativeTargetClusterForProject(context.Context, uuid.UUID, string) error
}

// ReconcileDeclarativeTarget makes the registered target match a pipeline's declaration,
// deciding and writing against ONE snapshot.
//
// It does not reuse Register: that performs its own ResolveDeployTarget, and for an
// UNGATED target enforceGateSoD returns early without comparing routing — so the
// decision would be taken against snapshot A while the guard is derived from a fresh
// snapshot B, and the rollout fields preserved from A would clobber whatever landed in
// between. Here the gate check, the drift compare, the stale-pin refusal and the CAS all
// derive from the same read.
func (r *Registrar) ReconcileDeclarativeTarget(ctx context.Context, in DeclarativeReconcileInput) (DeclarativeResult, error) {
	auth, ok := r.registry.(declarativeAuthorizer)
	if !ok {
		return DeclarativeResult{}, errors.New("deploysvc: registry does not support declarative authorization")
	}

	// 1. ONE read. Everything below decides from this.
	var snapshot *store.DeployTarget
	existing, err := r.registry.ResolveDeployTarget(ctx, in.ProjectID, in.Environment)
	switch {
	case err == nil:
		snapshot = &existing
	case errors.Is(err, store.ErrDeployTargetNotFound):
		// absent — a create
	default:
		return DeclarativeResult{}, fmt.Errorf("deploysvc: resolve declared target: %w", err)
	}

	// 2. A gated target refuses the declaration ALWAYS — even when the fields match.
	// A "no drift" short-circuit here would silently ignore the declaration and quietly
	// bypass the fail-loud intent.
	if snapshot != nil && snapshot.GoverningGate != nil {
		return DeclarativeResult{
			Decision: ReconcileTerminalFault,
			Public: fmt.Sprintf(
				"environment %q has an admin-managed approval gate — remove `deploy.target` from the pipeline (the registered target governs), or ask an admin to ungate it",
				in.Environment),
		}, nil
	}

	// 3. Narrow authorization BEFORE any network call: existence + allowed_projects +
	// the self-service opt-in, credential-free. Also means an unauthorized declaration
	// never costs an ArgoCD fetch.
	if err := auth.AuthorizeDeclarativeTargetClusterForProject(ctx, in.ProjectID, in.Cluster); err != nil {
		if store.IsClusterUnavailable(err) {
			return DeclarativeResult{
				Decision: ReconcileTerminalFault,
				Public:   store.ClusterUnavailableMessage,
			}, nil
		}
		return DeclarativeResult{}, fmt.Errorf("deploysvc: authorize declared cluster: %w", err)
	}

	namespace := deploy.NormalizeNamespace(in.Namespace)

	// 4. No drift => done. This is what keeps the steady state cheap: it skips the live
	// validation fetch and the write. (Not a single-read path overall — the takeover
	// resolves the target again under its own lock.)
	if snapshot != nil &&
		snapshot.Cluster == in.Cluster &&
		snapshot.Application == in.Application &&
		snapshot.Namespace == namespace &&
		snapshot.SyncMode == in.SyncMode {
		return DeclarativeResult{Decision: ReconcileNoChange}, nil
	}

	// 5. A preserved PIN can go stale — but only when the Application CHANGES.
	//
	// An Application's identity is cluster+namespace+application. Drift in any of those
	// points at a different Application, and the watcher would sync the NEW one while
	// still observing and promoting/aborting the OLD Rollout. Drift in `sync_mode`
	// alone does NOT: it changes who issues the sync, not which object is deployed, so
	// the pin stays valid and refusing it would be a false positive. Auto-discovered
	// routing (all three empty) is exempt either way — discovery re-resolves against
	// whatever Application the target now names.
	identityChanged := snapshot != nil &&
		(snapshot.Cluster != in.Cluster ||
			snapshot.Application != in.Application ||
			snapshot.Namespace != namespace)
	if identityChanged && snapshot.RolloutAware &&
		(snapshot.RolloutCluster != "" || snapshot.RolloutNamespace != "" || snapshot.RolloutName != "") {
		return DeclarativeResult{
			Decision: ReconcileTerminalFault,
			Public: fmt.Sprintf(
				"environment %q pins a rollout (%s) and the declared target changes the Application — change it via the UI/API, or the deploy would sync one Application while acting on another's rollout",
				in.Environment, snapshot.RolloutName),
		}, nil
	}

	// 6. Validate against the real Application (existence, reachability, single-source).
	target := deploy.DeploymentTarget{
		ProjectID:   in.ProjectID,
		Cluster:     in.Cluster,
		Application: in.Application,
		Namespace:   namespace,
	}
	if err := r.provider.ValidateSingleSource(ctx, target); err != nil {
		return classifyDeclarativeValidateErr(err), nil
	}

	envID, err := r.registry.EnsureEnvironment(ctx, in.ProjectID, in.Environment)
	if err != nil {
		return DeclarativeResult{}, fmt.Errorf("deploysvc: ensure environment: %w", err)
	}

	// 7. Guarded upsert against the SAME snapshot. A concurrent change (a gate added,
	// routing pinned) fails the CAS instead of clobbering. The rollout fields are
	// ADOPTED from the snapshot, never zeroed: the file does not express them, so a
	// YAML zero-value must not clear config it cannot describe.
	upsert := store.DeployTargetInput{
		EnvironmentID: envID,
		// The only provider today; the imperative handler hardcodes it the same way,
		// and it is never settable from YAML.
		Provider:    "argocd",
		Cluster:     in.Cluster,
		Application: in.Application,
		Namespace:   namespace,
		SyncMode:    in.SyncMode,
		CreatedBy:   in.Actor,
	}
	if snapshot != nil {
		upsert.RolloutAware = snapshot.RolloutAware
		upsert.RolloutCluster = snapshot.RolloutCluster
		upsert.RolloutNamespace = snapshot.RolloutNamespace
		upsert.RolloutName = snapshot.RolloutName
	}
	if err := r.registry.UpsertDeployTargetGuarded(ctx, upsert, guardFrom(snapshot)); err != nil {
		if errors.Is(err, store.ErrDeployTargetConflict) {
			return DeclarativeResult{Decision: ReconcileConflictRetry}, nil
		}
		return DeclarativeResult{}, fmt.Errorf("deploysvc: upsert declared target: %w", err)
	}
	return DeclarativeResult{Decision: ReconcileChanged}, nil
}

// classifyDeclarativeValidateErr splits CONFIGURATION from INFRASTRUCTURE.
//
// classifyValidateErr (the imperative one) maps transport errors and any 5xx to
// FaultUnprocessable, which is right for a human reading a 422 and retrying — and wrong
// for a dispatcher, where treating it as terminal makes a transient ArgoCD outage
// permanently fail the job.
//
// The two defaults pull in opposite directions on purpose: an unrecognised HTTP 4xx is
// treated as configuration (terminal), while an unrecognised error VALUE is treated as
// infrastructure (retry) — never terminalise on something we could not identify.
func classifyDeclarativeValidateErr(err error) DeclarativeResult {
	retry := DeclarativeResult{Decision: ReconcileConflictRetry}

	if errors.Is(err, deploy.ErrMultiSource) {
		return DeclarativeResult{
			Decision: ReconcileTerminalFault,
			Public:   "the declared Application is multi-source — register a single-source Application",
		}
	}
	if store.IsClusterUnavailable(err) {
		return DeclarativeResult{Decision: ReconcileTerminalFault, Public: store.ClusterUnavailableMessage}
	}
	var apiErr *store.ClusterAPIStatusError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusNotFound:
			return DeclarativeResult{
				Decision: ReconcileTerminalFault,
				Public:   "the declared ArgoCD Application does not exist on that cluster",
			}
		case apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden:
			// A credential that lacks rights will not gain them by retrying; loud and
			// actionable beats an endless loop.
			return DeclarativeResult{
				Decision: ReconcileTerminalFault,
				Public:   "the cluster credential is not allowed to read the declared Application",
			}
		case apiErr.Status == http.StatusRequestTimeout || apiErr.Status == http.StatusTooManyRequests:
			return retry
		case apiErr.Status >= 500:
			return retry
		case apiErr.Status >= 400:
			return DeclarativeResult{
				Decision: ReconcileTerminalFault,
				Public:   "the declared ArgoCD Application could not be validated",
			}
		}
	}
	return retry
}
