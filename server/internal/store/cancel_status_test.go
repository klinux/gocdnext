package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// jobAgentAttempt reads the agent_id + attempt a running job carries so a test can
// call CompleteJob with the CAS snapshot the real handler would use.
func jobAgentAttempt(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) (uuid.UUID, int32) {
	t.Helper()
	var agent uuid.UUID
	var attempt int32
	if err := pool.QueryRow(context.Background(),
		`SELECT agent_id, attempt FROM job_runs WHERE id=$1`, jobID).Scan(&agent, &attempt); err != nil {
		t.Fatalf("read agent/attempt: %v", err)
	}
	return agent, attempt
}

func stampCancel(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET cancel_requested_at = NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("stamp cancel: %v", err)
	}
}

// #207: CompleteJobRun derives the terminal status from cancel_requested_at.
func TestCompleteJob_CancelRequestedRecordsCanceled(t *testing.T) {
	cases := []struct {
		name       string
		reported   string // agent-reported status
		cancel     bool   // stamp cancel_requested_at first
		outputs    map[string]string
		wantStatus string
		wantOutput bool // outputs persisted?
	}{
		{"failed + cancel ⇒ canceled", "failed", true, nil, "canceled", false},
		{"failed + no cancel ⇒ failed", "failed", false, nil, "failed", false},
		{"success + cancel ⇒ canceled, outputs dropped", "success", true, map[string]string{"k": "v"}, "canceled", false},
		{"success + no cancel ⇒ success, outputs kept", "success", false, map[string]string{"k": "v"}, "success", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			ctx := context.Background()

			_, stageBuildID, _, jobCompileID, _ := seedRunningJob(t, pool)
			agentID, attempt := jobAgentAttempt(t, pool, jobCompileID)
			if tc.cancel {
				stampCancel(t, pool, jobCompileID)
			}

			comp, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
				JobRunID: jobCompileID, Status: tc.reported, ExitCode: 0,
				ExpectedAgentID: agentID, ExpectedAttempt: attempt, Outputs: tc.outputs,
			})
			if err != nil || !ok {
				t.Fatalf("CompleteJob: ok=%v err=%v", ok, err)
			}
			if comp.JobStatus != tc.wantStatus {
				t.Errorf("effective JobStatus = %q, want %q", comp.JobStatus, tc.wantStatus)
			}
			if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobCompileID); got != tc.wantStatus {
				t.Errorf("row status = %q, want %q", got, tc.wantStatus)
			}
			// The build stage has only `compile`, so its rollup reflects the job.
			if got := scalarStr(t, pool, `SELECT status FROM stage_runs WHERE id=$1`, stageBuildID); got != tc.wantStatus {
				t.Errorf("build stage status = %q, want %q (rollup priority)", got, tc.wantStatus)
			}
			outputs := scalarStr(t, pool, `SELECT outputs::text FROM job_runs WHERE id=$1`, jobCompileID)
			if tc.wantOutput && outputs == "{}" {
				t.Errorf("outputs dropped on an effective-success job: %q", outputs)
			}
			if !tc.wantOutput && outputs != "{}" {
				t.Errorf("outputs = %q, want {} (dropped for non-effective-success)", outputs)
			}
		})
	}
}

// A success whose completion CAS commits BEFORE the cancel wins: the row is
// terminal, so the REAL cancel path (CancelJobRun) refuses it with
// ErrJobRunTerminal and its stamp — which is CAS'd on status='running' — never
// lands, so cancel_requested_at stays NULL and the row stays success. This uses
// the real terminalizer, not a raw UPDATE, so the "stamp loses" is exercised
// through the production predicate.
func TestCompleteJob_SuccessBeforeStampStaysSuccess(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, _, _, jobCompileID, _ := seedRunningJob(t, pool)
	agentID, attempt := jobAgentAttempt(t, pool, jobCompileID)

	// Success commits first (no cancel yet).
	if _, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID: jobCompileID, Status: "success", ExitCode: 0,
		ExpectedAgentID: agentID, ExpectedAttempt: attempt,
	}); err != nil || !ok {
		t.Fatalf("CompleteJob success: ok=%v err=%v", ok, err)
	}

	// The real single-job cancel now loses: the row is terminal, so it refuses
	// (ErrJobRunTerminal) and its status='running'-guarded stamp never runs.
	if _, err := s.CancelJobRun(ctx, jobCompileID); !errors.Is(err, store.ErrJobRunTerminal) {
		t.Fatalf("CancelJobRun on a terminal job = %v, want ErrJobRunTerminal", err)
	}

	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobCompileID); got != "success" {
		t.Errorf("status = %q, want success (success beat the kill)", got)
	}
	var stamp *string
	if err := pool.QueryRow(ctx, `SELECT cancel_requested_at::text FROM job_runs WHERE id=$1`, jobCompileID).Scan(&stamp); err != nil {
		t.Fatalf("read cancel_requested_at: %v", err)
	}
	if stamp != nil {
		t.Errorf("cancel_requested_at = %v, want NULL (the stamp's status='running' CAS lost)", *stamp)
	}
}
