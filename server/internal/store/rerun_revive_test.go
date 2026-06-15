package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestRerunJob_RevivesFailFastCanceledGate is the regression for the
// stuck-canceled approval gate: when a stage fails, the cascade
// fail-fast-cancels every downstream stage/job — including the
// awaiting approval gate (CancelQueuedJobsInRun). Re-running the
// failed upstream job must REVIVE that gate (back to
// awaiting_approval) and reopen its stage + the run. Without the
// revive, the rerun finalizes the run 'success' with the gate stuck
// 'canceled' and production silently skipped — exactly what happened
// to a live release whose deploy failed on a missing secret, was
// fixed, and rerun.
func TestRerunJob_RevivesFailFastCanceledGate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// seedApprovalPipeline: stages [test, deploy]; job `unit` in test,
	// approval gate `gate` in deploy. Failing `unit` fail-fast-cancels
	// the downstream gate — the minimal shape of the production bug.
	pipelineID, materialID := seedApprovalPipeline(t, pool, "rerun-revive", []string{"alice"})
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: "main", Provider: "github",
		Delivery: "t", TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var unitJobID, gateJobID uuid.UUID
	for _, jr := range res.JobRuns {
		switch jr.Name {
		case "unit":
			unitJobID = jr.ID
		case "gate":
			gateJobID = jr.ID
		}
	}
	if unitJobID == uuid.Nil || gateJobID == uuid.Nil {
		t.Fatalf("unit (%v) or gate (%v) missing from RunCreated", unitJobID, gateJobID)
	}

	// Fail the upstream at dispatch (mirrors failJobWithError on a
	// missing secret). This fail-fast-cancels the downstream gate.
	if _, ok, err := s.FailJobWithReason(ctx, unitJobID, "secrets: not set"); err != nil || !ok {
		t.Fatalf("FailJobWithReason: ok=%v err=%v", ok, err)
	}
	// Precondition: the cascade canceled the gate — this is the state
	// production was stuck in. If this assert fails the test is no
	// longer exercising the bug.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, gateJobID); got != "canceled" {
		t.Fatalf("precondition: gate status = %q, want canceled (fail-fast)", got)
	}

	// Re-run the failed upstream job.
	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: unitJobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}

	// Gate revived to awaiting_approval (NOT queued — the dispatch query
	// only sees queued rows and would try to run a task-less gate), with
	// awaiting_since re-stamped and the declared approvers intact.
	var (
		status    string
		gate      bool
		approvers []string
		awaiting  *string // TIMESTAMPTZ as text; nil = unset
	)
	if err := pool.QueryRow(ctx, `
		SELECT status, approval_gate, approvers, awaiting_since::text
		FROM job_runs WHERE id = $1`, gateJobID,
	).Scan(&status, &gate, &approvers, &awaiting); err != nil {
		t.Fatalf("query gate: %v", err)
	}
	if status != "awaiting_approval" {
		t.Errorf("gate status = %q, want awaiting_approval (revived)", status)
	}
	if !gate {
		t.Error("approval_gate flag lost on revive")
	}
	if len(approvers) != 1 || approvers[0] != "alice" {
		t.Errorf("approvers = %+v, want [alice] (preserved across revive)", approvers)
	}
	if awaiting == nil {
		t.Error("awaiting_since not re-stamped on revive")
	}

	// The gate's stage + the run reopened so the gate isn't a ghost in
	// a terminal 'success' run.
	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, res.RunID); got != "running" {
		t.Errorf("run status = %q, want running", got)
	}
	stageStatus := scalarStr(t, pool, `
		SELECT s.status FROM stage_runs s
		JOIN job_runs j ON j.stage_run_id = s.id
		WHERE j.id = $1`, gateJobID)
	if stageStatus != "queued" && stageStatus != "running" {
		t.Errorf("gate stage status = %q, want queued/running", stageStatus)
	}
}

// scalarStr runs a single-column, single-row query and returns the
// string result — small read helper for the asserts above.
func scalarStr(t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("scalarStr %q: %v", query, err)
	}
	return out
}
