package store_test

import (
	"context"
	"encoding/json"
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
	s         *store.Store
	pool      *pgxpool.Pool
	ctx       context.Context
	revID     uuid.UUID
	projectID uuid.UUID
	gateID    uuid.UUID
	deadline  time.Time // pre-decision deadline (to assert the resume shift)
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
	gr := gatedRollout{s: s, pool: pool, ctx: ctx, revID: revID, projectID: projectID}
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
		RevisionID: gr.revID, ProjectID: gr.projectID, GateID: gr.gateID, Decision: decision,
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

// A reject after partial approvals reports the accurate approvals-so-far count (not 0).
func TestDecideRolloutGate_RejectReportsApprovals(t *testing.T) {
	gr := seedGatedRollout(t, "rg-rejcount", 2, []string{"alice@corp.com", "bob@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	bob := gr.user(t, "bob@corp.com", "Bob")

	if res, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil || !res.PendingQuorum {
		t.Fatalf("alice approve = %+v err=%v, want pending", res, err)
	}
	res, err := gr.decide(bob, "bob@corp.com", "Bob", "rejected")
	if err != nil || !res.Decided || res.Decision != "rejected" {
		t.Fatalf("bob reject = %+v err=%v, want Decided rejected", res, err)
	}
	if res.ApprovalsNow != 1 || res.ApprovalsRequired != 2 {
		t.Errorf("reject counts = %d/%d, want 1/2 (approvals so far)", res.ApprovalsNow, res.ApprovalsRequired)
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

// A revision from a DIFFERENT project (a crafted cross-project revID on the URL) doesn't
// resolve under the project-scoped lock → stale, never decidable.
func TestDecideRolloutGate_WrongProjectIsStale(t *testing.T) {
	gr := seedGatedRollout(t, "rg-xproj", 1, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	_, err := gr.s.DecideRolloutGate(gr.ctx, store.RolloutGateDecisionInput{
		RevisionID: gr.revID, ProjectID: uuid.New(), GateID: gr.gateID, Decision: "approved",
		UserID: alice, User: "Alice", UserEmail: "alice@corp.com",
	})
	if err != store.ErrGateStale {
		t.Fatalf("wrong project = %v, want ErrGateStale", err)
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

// A user allowed via approver_groups (not the direct list) can decide; a non-member
// can't — proving the ListUserGroupNames reuse.
func TestDecideRolloutGate_ApproverGroups(t *testing.T) {
	gr := seedGatedRollout(t, "rg-groups", 1, nil) // no direct approvers
	alice := gr.user(t, "alice@corp.com", "Alice")
	dave := gr.user(t, "dave@corp.com", "Dave") // not in the group

	grp, err := gr.s.InsertGroup(gr.ctx, store.GroupInput{Name: "sre"})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := gr.s.AddGroupMember(gr.ctx, grp.ID, alice, nil); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Gate lists the GROUP, not any user directly.
	if _, err := gr.pool.Exec(gr.ctx,
		`UPDATE deploy_watches SET gate_approvers = '{}', gate_approver_groups = $2 WHERE deployment_revision_id = $1`,
		gr.revID, []string{"sre"}); err != nil {
		t.Fatalf("set groups: %v", err)
	}

	if _, err := gr.decide(dave, "dave@corp.com", "Dave", "approved"); err != store.ErrApproverNotAllowed {
		t.Fatalf("non-member = %v, want ErrApproverNotAllowed", err)
	}
	res, err := gr.decide(alice, "alice@corp.com", "Alice", "approved")
	if err != nil || !res.Decided {
		t.Fatalf("group member approve = %+v err=%v, want Decided", res, err)
	}
}

// Fail-closed: a mis-shaped row (gate_required NULL = "not gated", or gate_armed_at NULL)
// is never decidable — it must be ErrGateStale, never a silent quorum-of-1.
func TestDecideRolloutGate_FailClosedOnMisshapedRow(t *testing.T) {
	for _, tt := range []struct {
		name string
		mut  string
	}{
		{"gate_required NULL", `UPDATE deploy_watches SET gate_required = NULL WHERE deployment_revision_id = $1`},
		{"gate_armed_at NULL", `UPDATE deploy_watches SET gate_armed_at = NULL WHERE deployment_revision_id = $1`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gr := seedGatedRollout(t, "rg-fc-"+uuid.NewString()[:6], 1, []string{"alice@corp.com"})
			alice := gr.user(t, "alice@corp.com", "Alice")
			if _, err := gr.pool.Exec(gr.ctx, tt.mut, gr.revID); err != nil {
				t.Fatalf("mut: %v", err)
			}
			if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != store.ErrGateStale {
				t.Fatalf("%s = %v, want ErrGateStale (no quorum-1 fallback)", tt.name, err)
			}
		})
	}
}

// The durable audit record carries each vote in full (user_id distinguishes same-label
// deciders) plus the required quorum — not just label+decision.
func TestDecideRolloutGate_AuditMetadata(t *testing.T) {
	gr := seedGatedRollout(t, "rg-auditmeta", 1, []string{"alice@corp.com"})
	alice := gr.user(t, "alice@corp.com", "Alice")
	if _, err := gr.decide(alice, "alice@corp.com", "Alice", "approved"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	var raw []byte
	if err := gr.pool.QueryRow(gr.ctx,
		`SELECT metadata FROM audit_events WHERE action = 'rollout_gate.approve' AND target_id = $1`,
		gr.revID.String()).Scan(&raw); err != nil {
		t.Fatalf("load audit: %v", err)
	}
	var m struct {
		GateID   string `json:"gate_id"`
		Decision string `json:"decision"`
		Required int    `json:"required"`
		Votes    []struct {
			UserID    string    `json:"user_id"`
			User      string    `json:"user_label"`
			Decision  string    `json:"decision"`
			DecidedAt time.Time `json:"decided_at"`
		} `json:"votes"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("metadata not JSON: %v (%s)", err, raw)
	}
	if m.GateID != gr.gateID.String() || m.Decision != "approved" || m.Required != 1 {
		t.Errorf("meta head = %+v, want gate_id/approved/required=1", m)
	}
	if len(m.Votes) != 1 {
		t.Fatalf("votes = %d, want 1", len(m.Votes))
	}
	v := m.Votes[0]
	if v.UserID != alice.String() || v.User != "Alice" || v.Decision != "approved" || v.DecidedAt.IsZero() {
		t.Errorf("vote = %+v, want full detail (user_id/label/decision/decided_at)", v)
	}
}

// DeployWatchCancelRequestedAt reads the deploy's cancel intent (job_runs.cancel_requested_at).
func TestDeployWatchCancelRequestedAt(t *testing.T) {
	gr := seedGatedRollout(t, "rg-cancelread", 1, []string{"alice@corp.com"})

	if at, err := gr.s.DeployWatchCancelRequestedAt(gr.ctx, gr.revID); err != nil || at != nil {
		t.Fatalf("pre-cancel = %v (err %v), want nil", at, err)
	}
	if _, err := gr.pool.Exec(gr.ctx,
		`UPDATE job_runs SET cancel_requested_at = NOW()
		 WHERE id = (SELECT job_run_id FROM deployment_revisions WHERE id = $1)`, gr.revID); err != nil {
		t.Fatalf("stamp cancel: %v", err)
	}
	at, err := gr.s.DeployWatchCancelRequestedAt(gr.ctx, gr.revID)
	if err != nil || at == nil {
		t.Fatalf("post-cancel = %v (err %v), want non-nil", at, err)
	}
	// Unknown revision → nil (fail-safe), not an error.
	if at, err := gr.s.DeployWatchCancelRequestedAt(gr.ctx, uuid.New()); err != nil || at != nil {
		t.Fatalf("unknown rev = %v (err %v), want nil/nil", at, err)
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
