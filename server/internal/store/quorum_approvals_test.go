package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedQuorumPipeline applies a pipeline with an approval gate
// carrying an approver_groups list + a quorum requirement.
func seedQuorumPipeline(
	t *testing.T, pool *pgxpool.Pool, slug string,
	approvers, approverGroups []string, required int,
) (pipelineID, materialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)
	p := &domain.Pipeline{
		Name:   "build",
		Stages: []string{"test", "deploy"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "unit", Stage: "test", Tasks: []domain.Task{{Script: "true"}}},
			{
				Name:  "gate",
				Stage: "deploy",
				Approval: &domain.ApprovalSpec{
					Approvers:      approvers,
					ApproverGroups: approverGroups,
					Required:       required,
					Description:    "Ship it?",
				},
			},
		},
	}
	ctx := context.Background()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug, Pipelines: []*domain.Pipeline{p},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID = res.Pipelines[0].PipelineID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM materials WHERE fingerprint = $1`, fp,
	).Scan(&materialID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}
	return
}

// seedUser creates a minimal user row so the quorum tests have
// someone to vote. Returns the user's uuid.
func seedUser(t *testing.T, pool *pgxpool.Pool, email, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (provider, external_id, email, name, role)
		VALUES ('local', $1, $1, $2, 'maintainer')
		RETURNING id
	`, email, name).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// TestApproveGate_QuorumOfTwo: two approvers, Required=2. First
// approve leaves the gate pending; second approve passes it.
func TestApproveGate_QuorumOfTwo(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	alice := seedUser(t, pool, "alice@example.com", "alice")
	bob := seedUser(t, pool, "bob@example.com", "bob")

	pipelineID, materialID := seedQuorumPipeline(t, pool, "quorum2",
		[]string{"alice", "bob"}, nil, 2)
	parentRunID, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)
	_, _ = parentRunID, gateJobID
	// Other stage must be success so deploy stage transitions on quorum hit.
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status = 'success' WHERE run_id = $1 AND name = 'test'`, parentRunID); err != nil {
		t.Fatalf("mark test stage: %v", err)
	}

	// First vote — should leave gate pending.
	res, err := s.ApproveGate(ctx, store.ApprovalDecision{
		JobRunID: gateJobID, UserID: alice, User: "alice",
	})
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if !res.PendingQuorum {
		t.Fatalf("first vote should leave gate pending; got %+v", res)
	}
	if res.ApprovalsNow != 1 || res.ApprovalsRequired != 2 {
		t.Errorf("quorum progress = %d/%d, want 1/2", res.ApprovalsNow, res.ApprovalsRequired)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id = $1`, gateJobID).Scan(&status); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "awaiting_approval" {
		t.Errorf("status after first vote = %q, want awaiting_approval", status)
	}

	// Second vote — quorum hit, gate passes.
	res2, err := s.ApproveGate(ctx, store.ApprovalDecision{
		JobRunID: gateJobID, UserID: bob, User: "bob",
	})
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if res2.PendingQuorum {
		t.Errorf("second vote should close the gate; still pending %+v", res2)
	}
	if !res2.StageCompleted || res2.StageStatus != "success" {
		t.Errorf("cascade = %+v, want stage completed+success", res2)
	}
}

// TestApproveGate_RejectInQuorum: one reject ends the gate
// immediately even when required=3 and only 1 vote landed.
func TestApproveGate_RejectEndsQuorumGate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	alice := seedUser(t, pool, "alice@example.com", "alice")

	pipelineID, materialID := seedQuorumPipeline(t, pool, "quorumreject",
		[]string{"alice", "bob", "carol"}, nil, 3)
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	res, err := s.RejectGate(ctx, store.ApprovalDecision{
		JobRunID: gateJobID, UserID: alice, User: "alice",
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if res.PendingQuorum {
		t.Errorf("reject shouldn't pend quorum; got %+v", res)
	}

	var status, decision string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(decision,'') FROM job_runs WHERE id = $1
	`, gateJobID).Scan(&status, &decision); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "failed" || decision != "rejected" {
		t.Errorf("status=%q decision=%q, want failed+rejected", status, decision)
	}
}

// TestApproveGate_ApproverGroupGrants — user not in the
// approvers list BUT in a group listed in approver_groups can
// still approve.
func TestApproveGate_ApproverGroupGrants(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	carol := seedUser(t, pool, "carol@example.com", "carol")

	// Create an "sre" group and put carol in it.
	g, err := s.InsertGroup(ctx, store.GroupInput{
		Name: "sre", Description: "SRE team",
	})
	if err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := s.AddGroupMember(ctx, g.ID, carol, nil); err != nil {
		t.Fatalf("add member: %v", err)
	}

	pipelineID, materialID := seedQuorumPipeline(t, pool, "groupgate",
		[]string{"alice"}, []string{"sre"}, 1)
	parentRunID, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)
	if _, err := pool.Exec(ctx,
		`UPDATE stage_runs SET status = 'success' WHERE run_id = $1 AND name = 'test'`, parentRunID); err != nil {
		t.Fatalf("mark test stage: %v", err)
	}

	// carol isn't in approvers but IS in the sre group — should pass.
	res, err := s.ApproveGate(ctx, store.ApprovalDecision{
		JobRunID: gateJobID, UserID: carol, User: "carol",
	})
	if err != nil {
		t.Fatalf("approve via group: %v", err)
	}
	if res.PendingQuorum {
		t.Errorf("single required should close gate on first vote; got %+v", res)
	}
}

// TestApproveGate_NonMemberRejected — user neither in approvers
// nor in any approver_group gets 403.
func TestApproveGate_NonMemberRejected(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	dave := seedUser(t, pool, "dave@example.com", "dave")

	pipelineID, materialID := seedQuorumPipeline(t, pool, "groupnomember",
		[]string{"alice"}, []string{"sre"}, 1)
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	_, err := s.ApproveGate(ctx, store.ApprovalDecision{
		JobRunID: gateJobID, UserID: dave, User: "dave",
	})
	if err == nil {
		t.Fatalf("expected ErrApproverNotAllowed; got nil")
	}
	if err != store.ErrApproverNotAllowed {
		t.Errorf("expected ErrApproverNotAllowed; got %v", err)
	}
}
