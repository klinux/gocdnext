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
	"time"

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

	// Rollout awareness (ADR-0001 Phase 2). When RolloutAware, Observe additionally
	// resolves + reads the Argo Rollouts Rollout CR the Application manages and reports
	// it in DeployState.Rollout. The Rollout lives on the WORKLOAD's destination
	// cluster/namespace (not the argocd hub ns) — reached via RolloutCluster (a
	// registered cluster; empty → same as Cluster). RolloutNamespace/RolloutName empty
	// → auto-discover the single Rollout from the Application's `.status.resources[]`.
	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
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
	// OperationStartedAt is `.status.operationState.startedAt` — when the LAST sync
	// operation began. Zero when absent or unparseable. It is the correlation anchor:
	// the watch loop trusts the operationState (phase, syncResult) as THIS deploy's
	// only if OperationStartedAt is at/after the deploy's own Sync trigger
	// (sync_requested_at). A zero value therefore fails closed — a pre-Sync snapshot
	// is never mistaken for this deploy's result. Parsed from RFC3339, so equal
	// timestamps compare equal under `==`.
	OperationStartedAt time.Time
	// SyncResultRevision is `.status.operationState.syncResult.revision` — the
	// revision the last operation actually synced to (which can differ from the live
	// `.status.sync.revision`). Paired with OperationStartedAt, the loop trusts the
	// operationState only if this equals the deploy's expected revision, so a stale
	// operation for a different revision can neither fast-fail nor false-succeed it.
	SyncResultRevision string

	// Rollout is the Argo Rollouts snapshot when the target is RolloutAware and the
	// Rollout was resolved+read this tick; RolloutObserved says whether it's populated.
	// A value (not a pointer) so DeployState stays comparable-by-`==`.
	Rollout         RolloutState
	RolloutObserved bool
	// RolloutError is a short, sanitized reason the Rollout couldn't be resolved/read
	// this tick (RolloutObserved=false) — surfaced to the UI and, in control mode,
	// the fail-closed signal. Empty when observed OK or not rollout-aware.
	RolloutError string
}

// RolloutPhase mirrors an Argo Rollouts Rollout's aggregate `.status.phase`.
type RolloutPhase string

const (
	RolloutProgressing RolloutPhase = "Progressing"
	RolloutPaused      RolloutPhase = "Paused"
	RolloutDegraded    RolloutPhase = "Degraded"
	RolloutHealthy     RolloutPhase = "Healthy"
)

// Pause reasons Argo Rollouts records in `.status.pauseConditions[].reason`.
const (
	PauseReasonCanaryStep   = "CanaryPauseStep"
	PauseReasonBlueGreen    = "BlueGreenPause"
	PauseReasonInconclusive = "AnalysisRunInconclusive"
)

// RolloutState is the comparable-by-`==` slice of an Argo Rollouts Rollout the
// provider reads (`.status` + `.spec.strategy.canary.steps`). All fields are
// scalars so DeployState remains comparable.
type RolloutState struct {
	Phase       RolloutPhase
	PauseReason string // first `.status.pauseConditions[].reason`, "" if not paused
	// CurrentStepIndex is `.status.currentStepIndex`; CurrentStepKnown says the
	// controller actually reported it. An ABSENT index unmarshals to 0, which must
	// NOT be trusted as "at step 0" — deriving PausedIndefinitely (arm a gate) from an
	// assumed step 0 would arm on incomplete state. So the index is nullable upstream
	// and only trusted when known.
	CurrentStepIndex int
	CurrentStepKnown bool
	StepCount        int    // len(`.spec.strategy.canary.steps`)
	Aborted          bool   // `.status.abort`
	Message          string // `.status.message` (bounded on persist)
	StableHash       string // `.status.stableRS`
	PodHash          string // `.status.currentPodHash`

	// PausedIndefinitely: the current pause is a canary `pause: {}` with no duration
	// (the human-gate step) — as opposed to a timed pause / analysis / blue-green.
	PausedIndefinitely bool
	// FullyPromoted: the canary has advanced through all steps and the new version is
	// the stable one — the "no early finalize" signal (a healthy App is not enough).
	FullyPromoted bool

	// Resolved identity of the Rollout Observe actually read this tick — used by the
	// Phase-2 watcher to Abort a non-gated rollout without a fragile re-discovery.
	ResolvedCluster   string
	ResolvedNamespace string
	ResolvedName      string

	// Active metric-analysis run (Argo Rollouts AnalysisRun), when the canary is running
	// one — observe-only (nothing in the gate machine keys on it), surfaced so an operator
	// sees WHY a canary is paused/degraded. AnalysisActive says whether it's populated;
	// AnalysisKind is "step" (gates the current step) or "background" (runs across the
	// rollout).
	AnalysisActive  bool
	AnalysisKind    string
	AnalysisName    string
	AnalysisPhase   AnalysisPhase
	AnalysisMessage string
}

// AnalysisPhase mirrors an Argo Rollouts AnalysisRun's `.status.phase`.
type AnalysisPhase string

const (
	AnalysisPending      AnalysisPhase = "Pending"
	AnalysisRunning      AnalysisPhase = "Running"
	AnalysisSuccessful   AnalysisPhase = "Successful"
	AnalysisFailed       AnalysisPhase = "Failed"
	AnalysisError        AnalysisPhase = "Error"
	AnalysisInconclusive AnalysisPhase = "Inconclusive"
)

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
// later. Promote/Abort drive an Argo Rollouts canary (Phase 2 gate control); they act
// on the target's RESOLVED/pinned Rollout identity, never a re-discovery.
type DeploymentProvider interface {
	// Sync actuates the target toward revision (no-op for observe mode).
	Sync(ctx context.Context, target DeploymentTarget, revision string) error
	// Observe returns one convergence snapshot.
	Observe(ctx context.Context, target DeploymentTarget) (DeployState, error)
	// Promote advances a step-paused canary one step (approve). Idempotent.
	Promote(ctx context.Context, target DeploymentTarget) error
	// Abort reverts a canary's traffic to stable (reject). Idempotent; not a Git revert.
	Abort(ctx context.Context, target DeploymentTarget) error
}
