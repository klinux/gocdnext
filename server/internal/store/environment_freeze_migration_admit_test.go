package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// AssignJobIfEnvNotFrozen (#206) is the admission boundary for a NON-deploy
// migration that declares `environment:`. It shares the deploy path's guarantee:
// once a freeze on the env has COMMITTED, no admission of a job targeting it can
// commit afterwards — and when the env is NOT frozen, the admission wins.
func TestFreezeSerialisesAgainstMigrationAdmission(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1,'h') RETURNING id`,
		"freeze-mig-admit-agent").Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	requeue := func() {
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET status='queued', agent_id=NULL, started_at=NULL WHERE id=$1`, jobID); err != nil {
			t.Fatalf("requeue: %v", err)
		}
	}

	// (b) admission wins first when NOT frozen: the job is admitted and running.
	requeue()
	assigned, outcome, err := s.AssignJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "prod")
	if err != nil {
		t.Fatalf("admission (unfrozen): %v", err)
	}
	if outcome != store.DeployAdmitted {
		t.Fatalf("outcome = %q, want admitted when the env is not frozen", outcome)
	}
	if assigned.ID != jobID {
		t.Fatalf("assigned wrong job: %v", assigned.ID)
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); got != "running" {
		t.Fatalf("job status = %q, want running after admission", got)
	}

	// Now freeze commits, then every subsequent admission is refused and the job
	// stays queued — the advisory lock + under-lock re-check close the race.
	requeue()
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	for i := range 5 {
		_, outcome, err := s.AssignJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "prod")
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		if outcome != store.DeployAdmissionFrozen {
			t.Fatalf("admission %d = %q, want frozen after the freeze committed", i, outcome)
		}
	}
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, jobID); got != "queued" {
		t.Fatalf("job status = %q, want queued (nothing admitted under the freeze)", got)
	}
	var agent *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT agent_id FROM job_runs WHERE id=$1`, jobID).Scan(&agent); err != nil {
		t.Fatalf("agent_id: %v", err)
	}
	if agent != nil {
		t.Fatalf("agent_id = %v, want NULL (job never claimed)", agent)
	}
}
