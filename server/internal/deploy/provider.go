// Package deploy is the server-side deployment-provider subsystem (ADR-0001):
// gocdnext observes and controls an external GitOps controller (ArgoCD first)
// through a thin client, and NEVER reconciles desired state itself — the
// controller renders + reconciles, gocdnext triggers a sync, watches
// convergence, and (later) drives progressive rollout via approval gates.
//
// This file holds the provider-facing domain: the target descriptor, the
// convergence snapshot, and the provider interface. Persistence (the target
// registry) and the concrete ArgoCD client land in sibling files.
package deploy

import (
	"context"

	"github.com/google/uuid"
)

// SyncMode selects how gocdnext actuates a target.
//
//   - trigger: manual-sync Applications — gocdnext issues the sync (after any
//     gate), then watches to convergence.
//   - observe: auto-sync Applications — gocdnext issues NO sync; it only watches
//     convergence (and, in a later slice, controls the rollout).
type SyncMode string

const (
	SyncModeTrigger SyncMode = "trigger"
	SyncModeObserve SyncMode = "observe"
)

// DeploymentTarget is the platform-registered descriptor of *how* an environment
// deploys — resolved from the registry by a pipeline's `deploy: { to: <env> }`.
// It carries no credentials: the ArgoCD Application CR is reached through the
// cluster registry (Cluster names a registered cluster whose k8s API hosts the
// Application), so the same credential serves the Application (this slice) and
// the Rollout CR (a later slice).
type DeploymentTarget struct {
	ProjectID   uuid.UUID // owning project — gates cluster access (allowed_projects)
	Environment string    // "prod" — the gocdnext environment this target deploys
	Provider    string    // "argocd" (the only provider today)
	Cluster     string    // cluster-registry name whose k8s API hosts the Application CR
	Application string    // ArgoCD Application name
	Namespace   string    // Application namespace (typically "argocd")
	SyncMode    SyncMode  // trigger | observe
}

// DeployState is one convergence snapshot the provider reports, mirroring the
// fields of an ArgoCD Application's `.status`. It is intentionally comparable
// (no slice/map fields) so tests and Evaluate can use `==`.
type DeployState struct {
	Sync   SyncStatus
	Health HealthStatus
	// ObservedRev is the single-source git revision the controller reports live
	// (`.status.sync.revision`). Empty for a multi-source Application
	// (`.status.sync.revisions`, out of scope for this slice — the target registry
	// rejects multi-source): an empty ObservedRev makes the revision check below
	// fail-closed (Pending, never a false success) rather than matching.
	ObservedRev string
	// OperationPhase is `.status.operationState.phase` — the LAST sync operation's
	// state. It persists across syncs and can be STALE/unrelated to this deploy, so
	// it is NOT consulted by the pure Evaluate (a stale Failed op must not fail a
	// Synced+Healthy app on the right revision). The watch loop reads it, correlated
	// with this deploy's Sync (post-Sync + matching revision), to fast-fail a
	// genuinely-failed sync and to avoid trusting a pre-Sync snapshot.
	OperationPhase OpPhase
}

// OpPhase mirrors an ArgoCD Application's `.status.operationState.phase`. Empty
// means no operation has been recorded.
type OpPhase string

const (
	OpRunning     OpPhase = "Running"
	OpSucceeded   OpPhase = "Succeeded"
	OpFailed      OpPhase = "Failed"
	OpError       OpPhase = "Error"
	OpTerminating OpPhase = "Terminating"
)

// SyncStatus mirrors an ArgoCD Application's `.status.sync.status`.
type SyncStatus string

const (
	SyncSynced    SyncStatus = "Synced"
	SyncOutOfSync SyncStatus = "OutOfSync"
	SyncUnknown   SyncStatus = "Unknown"
)

// HealthStatus mirrors an ArgoCD Application's `.status.health.status`.
type HealthStatus string

const (
	HealthHealthy     HealthStatus = "Healthy"
	HealthProgressing HealthStatus = "Progressing"
	HealthDegraded    HealthStatus = "Degraded"
	HealthSuspended   HealthStatus = "Suspended"
	HealthMissing     HealthStatus = "Missing"
	HealthUnknown     HealthStatus = "Unknown"
)

// DeployOutcome is the watch loop's per-snapshot classification (see Evaluate).
type DeployOutcome string

const (
	OutcomePending   DeployOutcome = "pending"   // not converged yet — keep watching
	OutcomeSucceeded DeployOutcome = "succeeded" // Synced + Healthy
	OutcomeFailed    DeployOutcome = "failed"    // Degraded — the desired state is unhealthy
)

// DeploymentProvider is the seam over an external GitOps controller. ArgoCD is
// the first implementation; the interface hedges toward argo-rollouts / git-only
// later. Promote/Abort (rollout runtime control) arrive with the rollout slice.
type DeploymentProvider interface {
	// Sync actuates the target toward revision (no-op for observe mode).
	Sync(ctx context.Context, target DeploymentTarget, revision string) error
	// Observe returns one convergence snapshot.
	Observe(ctx context.Context, target DeploymentTarget) (DeployState, error)
}
