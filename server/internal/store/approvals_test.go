package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedApprovalPipeline applies a 2-stage pipeline with an
// approval gate in the deploy stage. Returns the pipelineID,
// materialID, and the approval gate job's declared name (so
// tests can look up the job_run by name after a trigger).
func seedApprovalPipeline(t *testing.T, pool *pgxpool.Pool, slug string, approvers []string) (pipelineID, materialID uuid.UUID) {
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
					Approvers:   approvers,
					Description: "Ship it?",
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
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}
	return
}

// triggerApprovalRun creates a queued run against the seeded
// pipeline and returns the run id + the approval gate's
// job_run_id so tests can target it directly.
func triggerApprovalRun(t *testing.T, pool *pgxpool.Pool, pipelineID, materialID uuid.UUID) (runID, gateJobID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "deadbeef",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "t",
		TriggeredBy:    "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID = res.RunID
	for _, jr := range res.JobRuns {
		if jr.Name == "gate" {
			gateJobID = jr.ID
		}
	}
	if gateJobID == uuid.Nil {
		t.Fatal("approval gate job not in RunCreated.JobRuns")
	}
	return
}

func TestCreateRun_MarksApprovalGateAwaiting(t *testing.T) {
	// A job with Approval set must land in status='awaiting_approval'
	// with approval_gate=true, awaiting_since stamped, and the
	// declared approvers persisted — not status='queued' like a
	// regular job. The scheduler filter (status='queued') is how
	// dispatch avoids these rows; if the mark fires wrong here,
	// the gate silently dispatches to an agent that has no idea
	// what to run.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-basic", []string{"alice", "bob"})
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	var (
		status    string
		gate      bool
		approvers []string
		desc      string
		awaiting  *string // TIMESTAMPTZ scanned as string; nil means unset
	)
	err := pool.QueryRow(context.Background(), `
		SELECT status, approval_gate, approvers, COALESCE(approval_description, ''), awaiting_since::text
		FROM job_runs WHERE id = $1
	`, gateJobID).Scan(&status, &gate, &approvers, &desc, &awaiting)
	if err != nil {
		t.Fatalf("query gate row: %v", err)
	}
	if status != "awaiting_approval" {
		t.Errorf("status = %q, want awaiting_approval", status)
	}
	if !gate {
		t.Error("approval_gate = false")
	}
	if len(approvers) != 2 || approvers[0] != "alice" {
		t.Errorf("approvers = %+v", approvers)
	}
	if desc != "Ship it?" {
		t.Errorf("description = %q", desc)
	}
	if awaiting == nil {
		t.Error("awaiting_since not stamped")
	}
}

func TestApproveGate_HappyPathFlipsToSuccessAndPromotes(t *testing.T) {
	// Approval flips the gate directly to `success` (no
	// intermediate `queued`) AND cascades so the stage + run
	// progress without waiting for the scheduler to pick up a
	// queued-but-task-less row. Skipping `queued` closes the
	// window where the scheduler could try to dispatch a gate
	// to an agent.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-approve", []string{"alice"})
	parentRunID, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	// Move the test stage to a state where the deploy stage is
	// the only unfinished one — approving the gate in deploy
	// should then close both the stage and the whole run.
	if _, err := pool.Exec(context.Background(),
		`UPDATE stage_runs SET status = 'success' WHERE run_id = $1 AND name = 'test'`, parentRunID); err != nil {
		t.Fatalf("mark test stage success: %v", err)
	}

	res, err := s.ApproveGate(context.Background(), store.ApprovalDecision{
		JobRunID: gateJobID,
		User:     "alice",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if res.RunID != parentRunID {
		t.Errorf("returned run id = %s, want %s", res.RunID, parentRunID)
	}
	if !res.StageCompleted || res.StageStatus != "success" {
		t.Errorf("stage cascade = %+v, want completed+success", res)
	}
	if !res.RunCompleted || res.RunStatus != "success" {
		t.Errorf("run cascade = %+v, want completed+success", res)
	}

	var status, decision, decidedBy string
	if err := pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(decision, ''), COALESCE(decided_by, '')
		FROM job_runs WHERE id = $1
	`, gateJobID).Scan(&status, &decision, &decidedBy); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "success" {
		t.Errorf("status = %q, want success", status)
	}
	if decision != "approved" {
		t.Errorf("decision = %q", decision)
	}
	if decidedBy != "alice" {
		t.Errorf("decided_by = %q", decidedBy)
	}
}

func TestRejectGate_HappyPathFlipsToFailed(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-reject", []string{"alice"})
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	res, err := s.RejectGate(context.Background(), store.ApprovalDecision{
		JobRunID: gateJobID, User: "alice",
	})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	// Rejection is a terminal stage failure → fail-fast cascade
	// fires and the whole run lands in failed with downstream
	// queued work canceled.
	if res.RunStatus != "failed" {
		t.Errorf("run cascade status = %q, want failed", res.RunStatus)
	}
	var status, decision string
	var finished *string
	if err := pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(decision, ''), finished_at::text
		FROM job_runs WHERE id = $1
	`, gateJobID).Scan(&status, &decision, &finished); err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if decision != "rejected" {
		t.Errorf("decision = %q", decision)
	}
	if finished == nil {
		// Rejection is a terminal state; finished_at must be
		// stamped so the retention/aging queries don't treat a
		// rejected gate as forever-running.
		t.Error("finished_at not stamped on reject")
	}
}

func TestStageProgress_CountsAwaitingApprovalAsUnfinished(t *testing.T) {
	// Regression lock: a stage that has an approval gate AND a
	// regular job must NOT complete when only the regular job
	// finishes. If GetStageProgress stops counting
	// awaiting_approval as unfinished, the stage would promote
	// and the gate becomes orphaned in a dead stage. Seed a
	// fresh pipeline, complete the regular job via CompleteJob
	// (normal agent path), and assert the stage stays running.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	slug := "gate-progress"
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)
	p := &domain.Pipeline{
		Name:   "build",
		Stages: []string{"deploy"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "smoke", Stage: "deploy", Tasks: []domain.Task{{Script: "true"}}},
			{Name: "gate", Stage: "deploy", Approval: &domain.ApprovalSpec{}},
		},
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug, Pipelines: []*domain.Pipeline{p},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:     res.Pipelines[0].PipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "deadbeef",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "t",
		TriggeredBy:    "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var smokeID uuid.UUID
	for _, jr := range created.JobRuns {
		if jr.Name == "smoke" {
			smokeID = jr.ID
		}
	}
	// Simulate dispatch (queued → running) so CompleteJob can flip it.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET status='running' WHERE id = $1`, smokeID); err != nil {
		t.Fatal(err)
	}

	comp, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: smokeID, Status: "success", ExitCode: 0,
	})
	if err != nil || !ok {
		t.Fatalf("complete job: ok=%v err=%v", ok, err)
	}
	if comp.StageCompleted {
		t.Errorf("stage completed with gate still awaiting: %+v", comp)
	}

	// Sanity: the stage is still running and the gate still awaiting.
	var stageStatus, gateStatus string
	if err := pool.QueryRow(ctx,
		`SELECT s.status, g.status
		 FROM stage_runs s
		 JOIN job_runs g ON g.stage_run_id = s.id AND g.name = 'gate'
		 WHERE s.run_id = $1`, created.RunID,
	).Scan(&stageStatus, &gateStatus); err != nil {
		t.Fatal(err)
	}
	if stageStatus == "success" {
		t.Errorf("stage promoted to success with gate awaiting")
	}
	if gateStatus != "awaiting_approval" {
		t.Errorf("gate status = %q", gateStatus)
	}
}

func TestApproveGate_SecondCallReturnsNotPending(t *testing.T) {
	// Double-click on Approve, two admins racing, or a browser
	// retry on a flaky connection — the second decision must lose
	// cleanly rather than clobber the first. 409 in the HTTP layer.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-race", []string{})
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	if _, err := s.ApproveGate(context.Background(), store.ApprovalDecision{JobRunID: gateJobID}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	_, err := s.ApproveGate(context.Background(), store.ApprovalDecision{JobRunID: gateJobID}) //nolint:ineffassign
	if !errors.Is(err, store.ErrApprovalNotPending) {
		t.Fatalf("second approve err = %v, want ErrApprovalNotPending", err)
	}
}

func TestApproveGate_NotAnApprovalRowReturns404(t *testing.T) {
	// Approving a regular job_run should NOT silently flip it to
	// queued-with-decision — the endpoint caller would be
	// confused, and approver allow-lists would leak across job
	// types. Treat as not-found.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	res, err := s.CreateRunFromModification(context.Background(), baseTriggerInput(pipelineID, materialID, 0))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	regularJob := res.JobRuns[0].ID

	_, err = s.ApproveGate(context.Background(), store.ApprovalDecision{JobRunID: regularJob})
	if !errors.Is(err, store.ErrApprovalGateNotFound) {
		t.Errorf("err = %v, want ErrApprovalGateNotFound", err)
	}
}

func TestApproveGate_UnknownIDReturns404(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	_, err := s.ApproveGate(context.Background(), store.ApprovalDecision{JobRunID: uuid.New()})
	if !errors.Is(err, store.ErrApprovalGateNotFound) {
		t.Errorf("err = %v, want ErrApprovalGateNotFound", err)
	}
}

func TestApproveGate_NotInApproversListReturns403(t *testing.T) {
	// A non-empty approvers list MUST gate the decision on
	// membership. Empty approvers stays permissive (the parser
	// doc says "any authenticated user"); this test pins the
	// allow-list enforcement so a future refactor can't silently
	// drop it.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-allowlist", []string{"alice"})
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	_, err := s.ApproveGate(context.Background(), store.ApprovalDecision{
		JobRunID: gateJobID, User: "mallory",
	})
	if !errors.Is(err, store.ErrApproverNotAllowed) {
		t.Errorf("err = %v, want ErrApproverNotAllowed", err)
	}
}

func TestApproveGate_EmptyApproversAllowsAnyone(t *testing.T) {
	// Empty approvers list matches the parser's "any authenticated
	// user" semantics. The store never sees "authenticated" — that's
	// the HTTP layer's job — but it MUST NOT reject an empty User
	// either (admin scripts / system-triggered approvals).
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "gate-open", []string{})
	_, gateJobID := triggerApprovalRun(t, pool, pipelineID, materialID)

	if _, err := s.ApproveGate(context.Background(), store.ApprovalDecision{
		JobRunID: gateJobID, User: "anyone",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
}
