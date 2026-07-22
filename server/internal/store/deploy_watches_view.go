package store

// Read-only reporting over deploy_watches: the per-project live-status view (backs the
// /deploy-watches endpoint) and the cluster delete-guard count. Kept apart from the
// watcher-lifecycle mutations in deploy_watches.go.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DeployWatchView is one in-flight native deploy, joined to its environment + display
// version, for the read-only live-status endpoint. Cluster/Application/SyncMode are
// config (sanitised by role at the HTTP layer); the rest is live state.
type DeployWatchView struct {
	DeploymentRevisionID uuid.UUID // the endpoint's per-deploy key (Phase 2 approve/reject)
	Environment          string
	Version              string
	ExpectedRevision     string
	SyncMode             string
	Cluster              string
	Application          string
	WatchStartedAt       time.Time
	SyncRequestedAt      *time.Time
	DeadlineAt           time.Time
	DegradedSince        *time.Time

	// Observed rollout snapshot (Phase 2). RolloutObservedAt nil = not yet observed;
	// RolloutStepKnown=false = the controller step index was absent.
	RolloutAware       bool
	RolloutPhase       string
	RolloutMessage     string
	RolloutPauseReason string
	RolloutCurrentStep int
	RolloutStepKnown   bool
	RolloutStepCount   int
	RolloutAborted     bool
	RolloutError       string
	RolloutObservedAt  *time.Time

	// Active AnalysisRun (observe-only, PR3). AnalysisPhase empty = no active analysis.
	RolloutAnalysisKind    string
	RolloutAnalysisName    string
	RolloutAnalysisPhase   string
	RolloutAnalysisMessage string

	// Gate live-state (viewer-readable). GateID is Nil when no step is armed — the UI
	// echoes it on approve/reject (the anti-stale token). GatePausedStepKnown/GateRequired
	// nil = absent. GateApprovalsNow is how many approvals are in for the current round.
	GateID              uuid.UUID
	GatePausedStep      int
	GatePausedStepKnown bool
	GateRequired        int
	GateRequiredKnown   bool
	GateDecision        string
	GateApprovalsNow    int

	// The Rollout identity PINNED when the gate armed — what Promote/Abort act on, and
	// what lets the UI deep-link to the exact Rollout a gate governs (a namespace can
	// hold several). Empty when no gate is armed. Config-class: it names a cluster and
	// namespace, so the HTTP layer sanitises it by role like Cluster/Application.
	GateRolloutCluster   string
	GateRolloutNamespace string
	GateRolloutName      string
}

// ListDeployWatchesForProject returns the project's in-flight native deploys.
func (s *Store) ListDeployWatchesForProject(ctx context.Context, projectID uuid.UUID) ([]DeployWatchView, error) {
	rows, err := s.q.ListDeployWatchesForProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list deploy watches for project %s: %w", projectID, err)
	}
	out := make([]DeployWatchView, 0, len(rows))
	for _, r := range rows {
		out = append(out, DeployWatchView{
			DeploymentRevisionID:   fromPgUUID(r.DeploymentRevisionID),
			Environment:            r.Environment,
			Version:                r.Version,
			ExpectedRevision:       r.ExpectedRevision,
			SyncMode:               r.SyncMode,
			Cluster:                r.Cluster,
			Application:            r.Application,
			WatchStartedAt:         r.WatchStartedAt.Time,
			SyncRequestedAt:        pgTimePtr(r.SyncRequestedAt),
			DeadlineAt:             r.DeadlineAt.Time,
			DegradedSince:          pgTimePtr(r.DegradedSince),
			RolloutAware:           r.RolloutAware,
			RolloutPhase:           stringValue(r.RolloutPhase),
			RolloutMessage:         stringValue(r.RolloutMessage),
			RolloutPauseReason:     stringValue(r.RolloutPauseReason),
			RolloutCurrentStep:     int32Value(r.RolloutCurrentStep),
			RolloutStepKnown:       r.RolloutCurrentStep != nil,
			RolloutStepCount:       int32Value(r.RolloutStepCount),
			RolloutAborted:         r.RolloutAborted != nil && *r.RolloutAborted,
			RolloutError:           stringValue(r.RolloutError),
			RolloutObservedAt:      pgTimePtr(r.RolloutObservedAt),
			RolloutAnalysisKind:    stringValue(r.RolloutAnalysisKind),
			RolloutAnalysisName:    stringValue(r.RolloutAnalysisName),
			RolloutAnalysisPhase:   stringValue(r.RolloutAnalysisPhase),
			RolloutAnalysisMessage: stringValue(r.RolloutAnalysisMessage),
			GateID:                 fromPgUUID(r.GateID),
			GatePausedStep:         int32Value(r.GatePausedStep),
			GatePausedStepKnown:    r.GatePausedStep != nil,
			GateRequired:           int32Value(r.GateRequired),
			GateRequiredKnown:      r.GateRequired != nil,
			GateDecision:           stringValue(r.GateDecision),
			GateApprovalsNow:       int(r.GateApprovalsNow),
			GateRolloutCluster:     stringValue(r.GateRolloutCluster),
			GateRolloutNamespace:   stringValue(r.GateRolloutNamespace),
			GateRolloutName:        stringValue(r.GateRolloutName),
		})
	}
	return out, nil
}

// CountActiveWatchesForCluster backs the cluster delete-guard (an in-flight watch
// also RESTRICTs the cluster FK).
func (s *Store) CountActiveWatchesForCluster(ctx context.Context, cluster string) (int64, error) {
	n, err := s.q.CountActiveWatchesForCluster(ctx, cluster)
	if err != nil {
		return 0, fmt.Errorf("store: count active watches for cluster %q: %w", cluster, err)
	}
	return n, nil
}
