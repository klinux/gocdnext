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

// seedMigrationRun applies a project whose sole job is a NON-deploy migration
// that declares `environment: prod` (no `deploy:` marker), then creates one run.
// This is the #206 shape: a job a freeze must hold even though it deploys nothing.
func seedMigrationRun(t *testing.T, pool *pgxpool.Pool, slug string) store.RunCreated {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	fp := domain.GitFingerprint("https://github.com/org/"+slug, "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "migrate",
			Stages: []string{"migration"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/" + slug, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "migrate-prod", Stage: "migration", Image: "goose",
				Tasks:       []domain.Task{{Script: "goose up"}},
				Environment: "prod", // acts on prod, but is not a deploy
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply migration project: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID,
		Revision: "aaa0123456789aaa0123456789aaa0123456789a", Branch: "main",
		Provider: "github", Delivery: slug + "-run", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create migration run: %v", err)
	}
	return run
}

// A frozen environment holds a NON-deploy migration job that declares
// `environment:` — the core #206 guarantee. The job never reaches an agent and
// the run carries the reason.
func TestDispatchRun_FrozenEnvironmentHoldsMigration(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	run := seedMigrationRun(t, pool, "freeze-mig")
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-mig")

	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "month-end close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	agentID := seedAgentRow(t, pool, "freeze-mig-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)

	assertNoAssignment(t, sess)
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("migration job status = %q, want queued (a freeze holds, never fails)", got)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want frozen-deploy:prod", got)
	}
}

// A single-job RERUN of a migration is held by a freeze too: the rerun re-queues
// the job and it flows back through the same dispatch admission.
func TestDispatchRun_FrozenEnvironmentHoldsMigrationRerun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	run := seedMigrationRun(t, pool, "freeze-mig-rerun")
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-mig-rerun")

	// Terminalise then rerun — re-queues the job (attempt+1). A non-deploy rerun
	// passes no rerun-time freeze guard; the hold must land at dispatch admission.
	if _, ok, err := s.FailJobWithReason(ctx, jobID, "boom"); err != nil || !ok {
		t.Fatalf("FailJobWithReason: ok=%v err=%v", ok, err)
	}
	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: jobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("after rerun, job status = %q, want queued", got)
	}

	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	agentID := seedAgentRow(t, pool, "freeze-mig-rerun-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)

	assertNoAssignment(t, sess)
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("reran migration job status = %q, want queued (freeze holds the rerun)", got)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want frozen-deploy:prod", got)
	}
}

// A NON-env, non-deploy job is untouched by the freeze machinery: it takes the
// plain AssignJob path (no freeze lock, no probe) and is dispatched normally even
// while another environment is frozen.
func TestDispatchRun_PlainJobUnaffectedByFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	run := seedPlainRun(t, pool, "freeze-plain")
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-plain")

	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	agentID := seedAgentRow(t, pool, "freeze-plain-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)

	if got := jobStatusOf(t, pool, jobID); got != "running" {
		t.Fatalf("plain job status = %q, want running (freeze must not touch a no-env job)", got)
	}
}

// seedPlainRun applies a project whose sole job declares neither deploy nor
// environment.
func seedPlainRun(t *testing.T, pool *pgxpool.Pool, slug string) store.RunCreated {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	fp := domain.GitFingerprint("https://github.com/org/"+slug, "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name:   "build",
			Stages: []string{"test"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/" + slug, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "unit", Stage: "test", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo test"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply plain project: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID,
		Revision: "aaa0123456789aaa0123456789aaa0123456789a", Branch: "main",
		Provider: "github", Delivery: slug + "-run", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create plain run: %v", err)
	}
	return run
}
