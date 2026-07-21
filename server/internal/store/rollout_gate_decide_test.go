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

// gatedRollout assembles a gated, armed deploy: a real job_run (for the votes FK), a
// deploy_watch with a denormalized gate config, and an armed gate (backdated 5m so the
// deadline resume is observable). Returns everything a decision test needs.
type gatedRollout struct {
	s        *store.Store
	pool     *pgxpool.Pool
	ctx      context.Context
	revID    uuid.UUID
	gateID   uuid.UUID
	deadline time.Time // pre-decision deadline (to assert the resume shift)
}

func seedGatedRollout(t *testing.T, slug string, required int32, approvers []string) gatedRollout {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	// A real run → a real job_run to key votes on.
	pipelineID, materialID := seedApprovalPipeline(t, pool, slug, nil)
	_, jobRunID := triggerApprovalRun(t, pool, pipelineID, materialID)
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1`, slug).Scan(&projectID); err != nil {
		t.Fatalf("project lookup: %v", err)
	}

	if _, err := s.InsertCluster(ctx, newAuthCipher(t), store.ClusterInput{
		Name: "prod-gke", AuthType: store.ClusterAuthKubeconfig, Credential: sampleKubeconfig,
	}); err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, JobRunID: jobRunID, Attempt: 0, Version: "v1", DeployedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("create revision: %v", err)
	}
	if _, err := s.CreateDeployWatch(ctx, newWatchInput(projectID, revID)); err != nil {
		t.Fatalf("create watch: %v", err)
	}
	// Denormalize the gate config onto the watch (StartNativeDeploy does this in prod).
	if _, err := pool.Exec(ctx,
		`UPDATE deploy_watches SET gate_required = $2, gate_approvers = $3 WHERE deployment_revision_id = $1`,
		revID, required, approvers); err != nil {
		t.Fatalf("set gate config: %v", err)
	}
	claimed, err := s.ClaimDeployWatches(ctx, "w", 3600, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	if _, err := s.ArmRolloutGate(ctx, revID, claimed[0].ClaimID, store.ArmRolloutGateInput{
		PausedStep: 1, RolloutCluster: "dest", RolloutNamespace: "ns", RolloutName: "ro",
	}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Backdate the arm 5m so the resumed deadline shift is observable on a terminal decision.
	if _, err := pool.Exec(ctx,
		`UPDATE deploy_watches SET gate_armed_at = NOW() - interval '5 minutes' WHERE deployment_revision_id = $1`, revID); err != nil {
		t.Fatalf("backdate arm: %v", err)
	}
	gr := gatedRollout{s: s, pool: pool, ctx: ctx, revID: revID}
	g := readGate(t, ctx, pool, revID)
	gr.gateID, gr.deadline = *g.gateID, g.deadlineAt
	return gr
}

func (gr gatedRollout) user(t *testing.T, email, name string) uuid.UUID {
	t.Helper()
	u, err := gr.s.CreateOrUpdateLocalUser(gr.ctx, email, name, store.RoleMaintainer, "hunter2pass")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func (gr gatedRollout) decide(uid uuid.UUID, email, name, decision string) (store.RolloutGateResult, error) {
	return gr.s.DecideRolloutGate(gr.ctx, store.RolloutGateDecisionInput{
		RevisionID: gr.revID, GateID: gr.gateID, Decision: decision,
		UserID: uid, User: name, UserEmail: email,
	})
}

func TestDecideRolloutGate_ApproveQuorumAndDeadlineResume(t *testing.T) {
	gr := seedGatedRollout(t, "rg-approve", 2, []string{"alice@corp.com", "bob@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	bob := gr.user(t, "bob@corp.com", "Bob")

	// First approve: recorded, quorum (2) not met → window stays open, no deadline shift.
	res, err := gr.decide(alice, "alice@corp.com", "Alice", "approved")
	if err != nil || !res.PendingQuorum || res.Decided {
		t.Fatalf("first approve = %+v err=%v, want PendingQuorum", res, err)
	}
	if g := readGate(t, gr.ctx, gr.pool, gr.revID); g.decision != nil || !g.deadlineAt.Equal(gr.deadline) {
		t.Fatalf("partial approve changed the gate/deadline: %+v", g)
	}

	// Second, distinct approver: quorum met → terminal + deadline resumed (~5m).
	res, err = gr.decide(bob, "bob@corp.com", "Bob", "approved")
	if err != nil || !res.Decided || res.Decision != "approved" {
		t.Fatalf("quorum approve = %+v err=%v, want Decided approved", res, err)
	}
	g := readGate(t, gr.ctx, gr.pool, gr.revID)
	if g.decision == nil || *g.decision != "approved" {
		t.Fatalf("gate_decision = %v, want approved", g.decision)
	}
	if shift := g.deadlineAt.Sub(gr.deadline); shift < 4*time.Minute+50*time.Second || shift > 5*time.Minute+10*time.Second {
		t.Errorf("deadline resume shift = %s, want ~5m", shift)
	}
}

func TestDecideRolloutGate_FreshRejectIsTerminal(t *testing.T) {
	gr := seedGatedRollout(t, "rg-reject", 2, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")

	res, err := gr.decide(alice, "alice@corp.com", "Alice", "rejected")
	if err != nil || !res.Decided || res.Decision != "rejected" {
		t.Fatalf("reject = %+v err=%v, want Decided rejected", res, err)
	}
	if g := readGate(t, gr.ctx, gr.pool, gr.revID); g.decision == nil || *g.decision != "rejected" {
		t.Fatalf("gate_decision = %v, want rejected", g.decision)
	}
}

func TestDecideRolloutGate_VoteSemantics(t *testing.T) {
	gr := seedGatedRollout(t, "rg-votes", 2, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")

	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// Same user, same decision → 409 (no idempotent re-count).
	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != store.ErrAlreadyVoted {
		t.Fatalf("duplicate approve = %v, want ErrAlreadyVoted", err)
	}
	// Same user, CHANGED decision → 409 (approve-then-reject can't terminalize).
	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "rejected"); err != store.ErrAlreadyVoted {
		t.Fatalf("changed vote = %v, want ErrAlreadyVoted", err)
	}
	if g := readGate(t, gr.ctx, gr.pool, gr.revID); g.decision != nil {
		t.Errorf("a changed vote terminalized the gate: %+v", g)
	}
}

func TestDecideRolloutGate_StaleToken(t *testing.T) {
	gr := seedGatedRollout(t, "rg-stale", 1, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	gr.gateID = uuid.New() // pretend the UI holds a superseded token

	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != store.ErrGateStale {
		t.Fatalf("stale token = %v, want ErrGateStale", err)
	}
}

func TestDecideRolloutGate_NotAllowed(t *testing.T) {
	gr := seedGatedRollout(t, "rg-deny", 1, []string{"alice@corp.com"})
	carol := gr.user(t, "carol@corp.com", "Carol") // not on the allow-list

	if _, err := gr.decide(carol, "carol@corp.com", "Carol", "approved"); err != store.ErrApproverNotAllowed {
		t.Fatalf("non-approver = %v, want ErrApproverNotAllowed", err)
	}
}

// ClearRolloutGate deletes the step's real votes (proving the 3b vote-delete against a
// live job_run_id — the audit event is the durable record that survives).
func TestDecideRolloutGate_ClearDeletesVotes(t *testing.T) {
	gr := seedGatedRollout(t, "rg-clearvotes", 2, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A vote exists (partial quorum, gate still open).
	var votes int
	countVotes := func() int {
		if err := gr.pool.QueryRow(gr.ctx,
			`SELECT COUNT(*) FROM job_run_approvals a
			 JOIN deployment_revisions dr ON dr.job_run_id = a.job_run_id
			 WHERE dr.id = $1`, gr.revID).Scan(&votes); err != nil {
			t.Fatalf("count votes: %v", err)
		}
		return votes
	}
	if countVotes() != 1 {
		t.Fatalf("want 1 recorded vote before clear, got %d", votes)
	}
	// Re-claim to clear under a valid lease, then clear the gate.
	claimed, _ := gr.s.ClaimDeployWatches(gr.ctx, "w2", -1, 10)
	if len(claimed) != 1 {
		t.Fatalf("re-claim: n=%d", len(claimed))
	}
	if ok, err := gr.s.ClearRolloutGate(gr.ctx, gr.revID, claimed[0].ClaimID); err != nil || !ok {
		t.Fatalf("clear: ok=%v err=%v", ok, err)
	}
	if countVotes() != 0 {
		t.Errorf("votes not deleted on clear: %d remain", votes)
	}
}

func TestDecideRolloutGate_AuditOnTerminal(t *testing.T) {
	gr := seedGatedRollout(t, "rg-audit", 1, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")

	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var n int
	if err := gr.pool.QueryRow(gr.ctx,
		`SELECT COUNT(*) FROM audit_events WHERE action = 'rollout_gate.approve' AND target_id = $1`,
		gr.revID.String()).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 1 {
		t.Errorf("audit events = %d, want 1 durable rollout_gate.approve", n)
	}
}
