package store

// Read-only correlation of armed rollout gates onto the live Rollouts dashboard
// (ADR-0001, PR-C). The rollouts list is the live cluster state; a gate lives on the
// in-flight deploy_watch. This joins them by the PINNED Rollout identity so the UI can
// offer Approve/Reject on the exact Rollout a gate governs — and so the direct
// Promote/Abort endpoint can fail closed when a gate is armed.

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ArmedRolloutGate is one armed, still-undecided rollout gate correlated to its pinned
// Rollout (namespace/name). RevisionID is the deploy-watch key the Approve/Reject vote
// endpoints take; Required/ApprovalsNow render the quorum. It carries the RESOLVED
// namespace/name so the caller matches it against a live Rollout without re-discovery.
type ArmedRolloutGate struct {
	GateID       uuid.UUID
	RevisionID   uuid.UUID
	Required     int
	Namespace    string
	Name         string
	ApprovalsNow int
}

// ListArmedRolloutGatesForCluster returns the project's armed, undecided rollout gates on
// one Rollout cluster. Empty cluster returns no rows (the query's `= $2` can't match a
// NULL pinned cluster, and an unpinned/decided gate is never actionable) — a fail-safe
// for a caller that passes a blank cluster. The caller keys the result by
// (namespace, name) to attach a gate to a live Rollout.
func (s *Store) ListArmedRolloutGatesForCluster(ctx context.Context, projectID uuid.UUID, cluster string) ([]ArmedRolloutGate, error) {
	rows, err := s.q.ListArmedRolloutGatesForCluster(ctx, db.ListArmedRolloutGatesForClusterParams{
		ProjectID:          pgUUID(projectID),
		GateRolloutCluster: nullableString(cluster),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list armed rollout gates for project %s cluster %q: %w", projectID, cluster, err)
	}
	out := make([]ArmedRolloutGate, 0, len(rows))
	for _, r := range rows {
		out = append(out, ArmedRolloutGate{
			GateID:       fromPgUUID(r.GateID),
			RevisionID:   fromPgUUID(r.DeploymentRevisionID),
			Required:     int32Value(r.GateRequired),
			Namespace:    stringValue(r.GateRolloutNamespace),
			Name:         stringValue(r.GateRolloutName),
			ApprovalsNow: int(r.ApprovalsNow),
		})
	}
	return out, nil
}
