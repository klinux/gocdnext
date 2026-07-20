package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// gateState is the raw gate snapshot for assertions (the store read model gains these
// fields in the watcher chunk; here we read them directly).
type gateState struct {
	gateID      *uuid.UUID
	armedAt     *time.Time
	pausedStep  *int
	roCluster   *string
	roNamespace *string
	roName      *string
	decision    *string
	actionedAt  *time.Time
	deadlineAt  time.Time
}

func readGate(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revID uuid.UUID) gateState {
	t.Helper()
	var g gateState
	err := pool.QueryRow(ctx, `SELECT gate_id, gate_armed_at, gate_paused_step,
		gate_rollout_cluster, gate_rollout_namespace, gate_rollout_name,
		gate_decision, gate_actioned_at, deadline_at
		FROM deploy_watches WHERE deployment_revision_id = $1`, revID).Scan(
		&g.gateID, &g.armedAt, &g.pausedStep, &g.roCluster, &g.roNamespace, &g.roName,
		&g.decision, &g.actionedAt, &g.deadlineAt)
	if err != nil {
		t.Fatalf("read gate: %v", err)
	}
	return g
}

func armedGateStore(t *testing.T) (*store.Store, *pgxpool.Pool, context.Context, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()
	projectID, revID := seedWatchable(t, s, ctx, "gate-"+uuid.NewString()[:8])
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	return s, pool, ctx, revID, claimed[0].ClaimID
}

func armInput() store.ArmRolloutGateInput {
	return store.ArmRolloutGateInput{PausedStep: 1, RolloutCluster: "dest", RolloutNamespace: "ns", RolloutName: "ro"}
}

func TestArmRolloutGate(t *testing.T) {
	s, pool, ctx, revID, claimID := armedGateStore(t)

	ok, err := s.ArmRolloutGate(ctx, revID, claimID, armInput())
	if err != nil || !ok {
		t.Fatalf("arm: ok=%v err=%v", ok, err)
	}
	g := readGate(t, ctx, pool, revID)
	if g.gateID == nil || *g.gateID == uuid.Nil {
		t.Errorf("gate_id not stamped: %+v", g)
	}
	if g.pausedStep == nil || *g.pausedStep != 1 {
		t.Errorf("gate_paused_step = %v, want 1", g.pausedStep)
	}
	if g.roCluster == nil || *g.roCluster != "dest" || g.roNamespace == nil || *g.roNamespace != "ns" || g.roName == nil || *g.roName != "ro" {
		t.Errorf("pinned identity incomplete: %v/%v/%v", g.roCluster, g.roNamespace, g.roName)
	}
	first := *g.gateID

	// Re-arm is a no-op (gate_id IS NULL guard) — never re-mints a token under a vote.
	if ok, err := s.ArmRolloutGate(ctx, revID, claimID, armInput()); err != nil || ok {
		t.Fatalf("re-arm = ok:%v err:%v, want no-op", ok, err)
	}
	if g2 := readGate(t, ctx, pool, revID); g2.gateID == nil || *g2.gateID != first {
		t.Errorf("re-arm changed the gate_id: %v -> %v", first, g2.gateID)
	}

	// A stale watcher (wrong claim_id) can't arm.
	s2, pool2, ctx2, revID2, _ := armedGateStore(t)
	if ok, err := s2.ArmRolloutGate(ctx2, revID2, uuid.New(), armInput()); err != nil || ok {
		t.Fatalf("arm with a wrong claim = ok:%v err:%v, want fenced-out", ok, err)
	}
	if g := readGate(t, ctx2, pool2, revID2); g.gateID != nil {
		t.Errorf("fenced arm still stamped a gate: %+v", g)
	}
}

func TestMarkGateActioned(t *testing.T) {
	s, pool, ctx, revID, claimID := armedGateStore(t)
	if _, err := s.ArmRolloutGate(ctx, revID, claimID, armInput()); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if ok, err := s.MarkGateActioned(ctx, revID, claimID); err != nil || !ok {
		t.Fatalf("mark actioned: ok=%v err=%v", ok, err)
	}
	if g := readGate(t, ctx, pool, revID); g.actionedAt == nil {
		t.Errorf("gate_actioned_at not stamped")
	}
	// Idempotent: a re-tick is a no-op.
	if ok, err := s.MarkGateActioned(ctx, revID, claimID); err != nil || ok {
		t.Errorf("re-mark = ok:%v err:%v, want no-op", ok, err)
	}
	// Fenced.
	if ok, _ := s.MarkGateActioned(ctx, revID, uuid.New()); ok {
		t.Errorf("mark actioned with a wrong claim succeeded")
	}
}

func TestClearRolloutGate_ResumesDeadlineWhenUndecided(t *testing.T) {
	s, pool, ctx, revID, claimID := armedGateStore(t)
	if _, err := s.ArmRolloutGate(ctx, revID, claimID, armInput()); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Backdate the arm 5m so the resumed-deadline shift is observable.
	if _, err := pool.Exec(ctx, `UPDATE deploy_watches SET gate_armed_at = NOW() - interval '5 minutes' WHERE deployment_revision_id = $1`, revID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	before := readGate(t, ctx, pool, revID).deadlineAt

	ok, err := s.ClearRolloutGate(ctx, revID, claimID)
	if err != nil || !ok {
		t.Fatalf("clear: ok=%v err=%v", ok, err)
	}
	g := readGate(t, ctx, pool, revID)
	// Per-arm columns nulled.
	if g.gateID != nil || g.armedAt != nil || g.pausedStep != nil || g.roName != nil {
		t.Errorf("gate not fully disarmed: %+v", g)
	}
	// Deadline shifted forward by ~5m (undecided clear resumes the suspended clock).
	shift := g.deadlineAt.Sub(before)
	if shift < 4*time.Minute+50*time.Second || shift > 5*time.Minute+10*time.Second {
		t.Errorf("deadline shift = %s, want ~5m", shift)
	}
}

func TestClearRolloutGate_NoShiftWhenDecided(t *testing.T) {
	s, pool, ctx, revID, claimID := armedGateStore(t)
	if _, err := s.ArmRolloutGate(ctx, revID, claimID, armInput()); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// A DECIDED gate already had its deadline resumed by DecideRolloutGate — clearing it
	// must NOT shift again.
	if _, err := pool.Exec(ctx, `UPDATE deploy_watches
		SET gate_armed_at = NOW() - interval '5 minutes', gate_decision = 'approved'
		WHERE deployment_revision_id = $1`, revID); err != nil {
		t.Fatalf("decide: %v", err)
	}
	before := readGate(t, ctx, pool, revID).deadlineAt
	if _, err := s.ClearRolloutGate(ctx, revID, claimID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	g := readGate(t, ctx, pool, revID)
	if g.decision != nil || g.gateID != nil {
		t.Errorf("gate not disarmed: %+v", g)
	}
	if shift := g.deadlineAt.Sub(before); shift > time.Second {
		t.Errorf("deadline shifted %s on a DECIDED clear, want ~0 (no double-resume)", shift)
	}
}

func TestClearRolloutGate_Fenced(t *testing.T) {
	s, pool, ctx, revID, claimID := armedGateStore(t)
	if _, err := s.ArmRolloutGate(ctx, revID, claimID, armInput()); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if ok, err := s.ClearRolloutGate(ctx, revID, uuid.New()); err != nil || ok {
		t.Fatalf("clear with a wrong claim = ok:%v err:%v, want fenced-out", ok, err)
	}
	if g := readGate(t, ctx, pool, revID); g.gateID == nil {
		t.Errorf("fenced clear still disarmed the gate: %+v", g)
	}
}
