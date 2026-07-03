package store

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// LaneEnvLockKey is the canonical Postgres advisory-lock key for a (lane, env),
// taken by BOTH the approve-time gate-pass marker write and (Phase 2) the dispatch
// backstop guard so they mutually exclude — the TOCTOU serialization. It MUST be
// stable across processes and identical between the two paths; hash collisions only
// over-serialize (harmless). The lane is (pipeline, ref) for `branch` and (pipeline)
// for `pipeline`, so pipeline mode drops ref, and the mode prefix keeps branch:""
// (tag/manual) distinct from pipeline:"".
func LaneEnvLockKey(pipelineID uuid.UUID, laneMode, laneRef, env string) int64 {
	ref := laneRef
	if laneMode != domain.SupersedeBranch {
		ref = "" // the pipeline lane ignores ref
	}
	h := fnv.New64a()
	writeField := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	writeField(laneMode)
	_, _ = h.Write(pipelineID[:])
	_, _ = h.Write([]byte{0})
	writeField(ref)
	writeField(env)
	return int64(h.Sum64())
}

// clearRevivedGatePassMarkers deletes a rerun-revived run's gate-pass markers whose
// env is governed by a gate that is now awaiting_approval again (the rerun re-armed
// it). A marker asserts "all gates governing this env passed"; a re-armed gate makes
// that false, so the marker must go and the run re-establishes clearance through the
// normal approve path. Markers for envs governed ONLY by still-passed (upstream)
// gates are left intact — deleting those would drop a legitimate block and let an
// older run's stale deploy for that env through. No-op for supersede=off (no
// markers) and when nothing is awaiting.
func (s *Store) clearRevivedGatePassMarkers(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	q := s.q.WithTx(tx)
	gateNames, err := q.ListAwaitingGateNamesForRun(ctx, pgUUID(runID))
	if err != nil {
		return fmt.Errorf("store: revived gate-pass gates: %w", err)
	}
	if len(gateNames) == 0 {
		return nil
	}
	rc, err := q.GetRunSupersedeContext(ctx, pgUUID(runID))
	if err != nil {
		return fmt.Errorf("store: revived gate-pass context: %w", err)
	}
	var def domain.Pipeline
	if err := json.Unmarshal(rc.Definition, &def); err != nil {
		return fmt.Errorf("store: revived gate-pass decode: %w", err)
	}
	if def.Supersede != domain.SupersedeBranch && def.Supersede != domain.SupersedePipeline {
		return nil
	}
	seen := map[string]struct{}{}
	var envs []string
	for _, gn := range gateNames {
		for _, e := range def.GovernedEnvs(gn) {
			if _, dup := seen[e]; !dup {
				seen[e] = struct{}{}
				envs = append(envs, e)
			}
		}
	}
	if len(envs) == 0 {
		return nil
	}
	if err := q.DeleteRunGatePassForEnvs(ctx, db.DeleteRunGatePassForEnvsParams{
		RunID: pgUUID(runID), Column2: envs,
	}); err != nil {
		return fmt.Errorf("store: delete revived gate-pass: %w", err)
	}
	return nil
}

// writeGatePassMarkers records, for each concrete deploy env the just-approved gate
// governs, that the run cleared it — but only once ALL gates governing that env have
// passed (a multi-gate env stays unmarked until the last approval). Runs in the
// approve tx and is FAIL-CLOSED: any error aborts the approval, so a supersede
// pipeline never approves a deploy without its backstop marker. No-op for
// supersede=off and for a gate that governs no deploy.
//
// Serialization: each env's marker write happens under pg_advisory_xact_lock on the
// lane-env key, in sorted env order. Concurrent approvals of DIFFERENT gates
// governing the SAME env therefore serialize on that env's lock, so the "all
// governing gates passed" read is race-free and exactly one marker is written — with
// NO runs-row lock, which would deadlock against the job→runs result/cascade order.
func (s *Store) writeGatePassMarkers(ctx context.Context, tx pgx.Tx, gateName string, runID uuid.UUID) error {
	q := s.q.WithTx(tx)
	rc, err := q.GetRunSupersedeContext(ctx, pgUUID(runID))
	if err != nil {
		return fmt.Errorf("store: gate-pass context: %w", err)
	}
	if len(rc.Definition) == 0 {
		return nil
	}
	var def domain.Pipeline
	if err := json.Unmarshal(rc.Definition, &def); err != nil {
		return fmt.Errorf("store: gate-pass decode: %w", err)
	}
	if def.Supersede != domain.SupersedeBranch && def.Supersede != domain.SupersedePipeline {
		return nil // the backstop only guards supersede pipelines
	}
	envs := def.GovernedEnvs(gateName) // sorted, concrete, deduped
	if len(envs) == 0 {
		return nil // pure-approval / shadowed gate — no deploy to protect
	}
	pipelineID := fromPgUUID(rc.PipelineID)
	for _, env := range envs {
		key := LaneEnvLockKey(pipelineID, def.Supersede, rc.Ref, env)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return fmt.Errorf("store: gate-pass lock: %w", err)
		}
		governing := def.GoverningGates(env)
		if len(governing) == 0 {
			continue // defensive — env came from GovernedEnvs, so this shouldn't hit
		}
		passed, err := q.CountPassedGates(ctx, db.CountPassedGatesParams{
			RunID: pgUUID(runID), Column2: governing,
		})
		if err != nil {
			return fmt.Errorf("store: gate-pass count: %w", err)
		}
		if int(passed) < len(governing) {
			continue // not all governing gates passed yet — hold the marker
		}
		if err := q.InsertRunGatePass(ctx, db.InsertRunGatePassParams{
			RunID:       pgUUID(runID),
			PipelineID:  rc.PipelineID,
			Ref:         rc.Ref,
			Counter:     rc.Counter,
			Environment: env,
		}); err != nil {
			return fmt.Errorf("store: gate-pass insert: %w", err)
		}
	}
	return nil
}
