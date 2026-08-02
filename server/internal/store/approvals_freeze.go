package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// lockApprovalEnvs takes, in the mandatory global order, every advisory lock an
// APPROVE of `gateName` needs, and returns the concrete deploy environments the
// gate governs (sorted, deduped — domain.GovernedEnvs' contract).
//
// Order within the function mirrors the global order exactly:
//
//	(1) LaneEnvLockKey per governed env, in name order
//	(2) ProjectEnvFreezeLockKey per governed env, in name order
//
// The lane key is computed with the SAME helper writeGatePassMarkers uses, from
// the same inputs (pipeline id, supersede mode, run ref, env). That identity is
// load-bearing, not cosmetic: writeGatePassMarkers runs at the END of the
// approve tx and re-takes the lane lock. If the key computed here differed by
// so much as the mode prefix, that call would take a DIFFERENT, not-yet-held
// lane lock AFTER the freeze lock — re-introducing the freeze→lane inversion
// this whole ordering exists to prevent. Because advisory xact locks are
// reentrant, taking the identical key twice is a free no-op.
//
// Returns an empty slice for a pure-approval gate (governs no deploy) and for a
// supersede=off pipeline's lane locks — the freeze locks are still taken, since
// freezing is independent of supersede.
func lockApprovalEnvs(ctx context.Context, tx pgx.Tx, projectID, pipelineID uuid.UUID, runRef, gateName string, definition []byte) ([]string, error) {
	if len(definition) == 0 {
		return nil, nil
	}
	var def domain.Pipeline
	if err := json.Unmarshal(definition, &def); err != nil {
		return nil, fmt.Errorf("store: approval freeze: decode run definition: %w", err)
	}
	// Two DISTINCT env sets (#206). Lane locks are a supersede/deploy concern, so
	// they use the deploy-only GovernedEnvs (widening them would let a migration
	// contend the lane / imply a supersede backstop it never has). Freeze locks +
	// the freeze CHECK use GovernedFreezeEnvs, which also counts bare
	// `environment:` jobs (a migration) — so a gate that governs ONLY a migration
	// is still freeze-aware. Return the freeze set: the caller's approve-time
	// check runs against it.
	laneEnvs := def.GovernedEnvs(gateName)        // sorted, concrete, deduped
	freezeEnvs := def.GovernedFreezeEnvs(gateName) // superset (deploy + environment:)
	if len(laneEnvs) == 0 && len(freezeEnvs) == 0 {
		return nil, nil
	}

	// (1) lane locks, name order. Only supersede pipelines have a lane backstop;
	// for supersede=off writeGatePassMarkers is a no-op, so there is no lane
	// lock to order against. Deploy-only — a migration takes no lane lock.
	if def.Supersede == domain.SupersedeBranch || def.Supersede == domain.SupersedePipeline {
		for _, env := range laneEnvs {
			key := LaneEnvLockKey(pipelineID, def.Supersede, runRef, env)
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
				return nil, fmt.Errorf("store: approval lane lock: %w", err)
			}
		}
	}

	// (2) freeze locks, name order — over the freeze set (incl. migration envs).
	for _, env := range freezeEnvs {
		if err := lockProjectEnvFreeze(ctx, tx, projectID, env); err != nil {
			return nil, err
		}
	}
	return freezeEnvs, nil
}

// frozenGovernedEnvsTx is FrozenGovernedEnvs bound to a caller-owned tx.
//
// The tx binding is the point: the approval path holds advisory locks when it
// asks, and going out to s.q (a second pooled connection) while holding them
// deadlocks deterministically at MaxConns=1 and wastes a connection at any size.
func frozenGovernedEnvsTx(ctx context.Context, q *db.Queries, projectID uuid.UUID, envs []string) ([]string, error) {
	if len(envs) == 0 {
		return nil, nil
	}
	frozen, err := q.FrozenEnvironments(ctx, db.FrozenEnvironmentsParams{
		ProjectID: pgUUID(projectID),
		Names:     envs,
	})
	if err != nil {
		return nil, fmt.Errorf("store: approval freeze check: %w", err)
	}
	return frozen, nil
}
