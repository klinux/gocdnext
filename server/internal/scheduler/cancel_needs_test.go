package scheduler_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// #207 Part 4: a CANCELED upstream skips (not fails) its `needs:` dependents, and
// the skip propagates down the chain. A stage of only-skipped jobs derives success
// (the jobs didn't run; a cancel's fallout must not hit the failure metric), while
// the run stays 'canceled' from the earlier canceled job.
func TestDispatchRun_CanceledUpstreamSkipsDependentsChain(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, jobA, jobB, jobC := seedNeedsChain(t, pool, s, "needs-cancel")

	// Cancel the queued upstream A (user single-job cancel).
	if _, err := s.CancelJobRun(ctx, jobA); err != nil {
		t.Fatalf("cancel A: %v", err)
	}

	// A ready agent isn't needed (B/C are skipped, never dispatched), but provide
	// one so the dispatch loop runs its full path. Several ticks let the skip
	// propagate stage by stage.
	agentID := seedAgentRow(t, pool, "needs-cancel-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)
	for range 5 {
		sched.DispatchRun(ctx, runID)
	}

	if got := jobStatusOf(t, pool, jobA); got != "canceled" {
		t.Errorf("A status = %q, want canceled", got)
	}
	if got := jobStatusOf(t, pool, jobB); got != "skipped" {
		t.Errorf("B status = %q, want skipped (needs a canceled upstream)", got)
	}
	if got := jobStatusOf(t, pool, jobC); got != "skipped" {
		t.Errorf("C status = %q, want skipped (needs a skipped upstream — propagation)", got)
	}
	// The run derives canceled (the canceled A counts), not failed/success.
	var runStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("run status: %v", err)
	}
	if runStatus != "canceled" {
		t.Errorf("run status = %q, want canceled", runStatus)
	}
}

// seedNeedsChain builds a → b(needs a) → c(needs b), one job per stage, and returns
// the run + the three job ids (all queued).
func seedNeedsChain(t *testing.T, pool *pgxpool.Pool, s *store.Store, slug string) (runID, a, b, c uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	fp := domain.GitFingerprint("https://github.com/org/"+slug, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "chain",
			Stages: []string{"s1", "s2", "s3"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/" + slug, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "a", Stage: "s1", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
				{Name: "b", Stage: "s2", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}, Needs: []string{"a"}},
				{Name: "c", Stage: "s3", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}, Needs: []string{"b"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID,
		Revision: "aaa0123456789aaa0123456789aaa0123456789a", Branch: "main",
		Provider: "github", Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, j := range run.JobRuns {
		switch j.Name {
		case "a":
			a = j.ID
		case "b":
			b = j.ID
		case "c":
			c = j.ID
		}
	}
	return run.RunID, a, b, c
}
