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

// projectIDForRun resolves the owning project of a seeded run so the
// deployment tests can lazy-create environments under a real project.
func projectIDForRun(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var pid uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT p.project_id FROM runs r JOIN pipelines p ON p.id = r.pipeline_id WHERE r.id = $1`,
		runID,
	).Scan(&pid)
	if err != nil {
		t.Fatalf("resolve project for run: %v", err)
	}
	return pid
}

func TestEnsureEnvironment_LazyCreateIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "env-lazy")

	id1, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("EnsureEnvironment: %v", err)
	}
	// Second reference to the same name returns the SAME row — lazy
	// create must not duplicate.
	id2, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("EnsureEnvironment (2nd): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("lazy-create produced two ids: %s vs %s", id1, id2)
	}
	envs, err := s.ListEnvironments(ctx, projectID)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 1 || envs[0].Name != "production" {
		t.Fatalf("environments = %+v, want exactly [production]", envs)
	}
}

func TestDeleteEnvironment(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// An IDLE env (only finalized history) deletes and cascades its revisions.
	t.Run("cascades finalized history when idle", func(t *testing.T) {
		runID, _, _, jobID, _ := seedRunningJob(t, pool)
		projectID := projectIDForRun(t, pool, runID)
		envID, err := s.EnsureEnvironment(ctx, projectID, "prod-idle")
		if err != nil {
			t.Fatalf("EnsureEnvironment: %v", err)
		}
		revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
			EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "1.0.abc",
		})
		if err != nil {
			t.Fatalf("CreateDeploymentRevision: %v", err)
		}
		// CreateDeploymentRevision records it in_progress; finalize it so the env is
		// idle (an in-flight revision would block the delete — covered below).
		if _, err := pool.Exec(ctx,
			`UPDATE deployment_revisions SET status='success', finished_at=NOW() WHERE id=$1`, revID,
		); err != nil {
			t.Fatalf("finalize revision: %v", err)
		}

		outcome, err := s.DeleteEnvironment(ctx, projectID, envID)
		if err != nil {
			t.Fatalf("DeleteEnvironment: %v", err)
		}
		if outcome != store.EnvDeleteDeleted {
			t.Fatalf("outcome = %v, want EnvDeleteDeleted", outcome)
		}
		if envs, _ := s.ListEnvironments(ctx, projectID); len(envs) != 0 {
			t.Fatalf("environment survived delete: %+v", envs)
		}
		// ON DELETE CASCADE dropped the finalized revision too.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM deployment_revisions WHERE environment_id = $1`, envID,
		).Scan(&n); err != nil {
			t.Fatalf("count revisions: %v", err)
		}
		if n != 0 {
			t.Fatalf("cascade left %d deployment_revisions", n)
		}
	})

	// An in-flight deploy (in_progress revision) blocks the delete — otherwise the
	// cascade would yank the revision out from under a still-running job.
	t.Run("refuses while a deploy is in progress", func(t *testing.T) {
		runID, _, _, jobID, _ := seedRunningJob(t, pool)
		projectID := projectIDForRun(t, pool, runID)
		envID, err := s.EnsureEnvironment(ctx, projectID, "prod-active")
		if err != nil {
			t.Fatalf("EnsureEnvironment: %v", err)
		}
		if _, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
			EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "2.0.def",
		}); err != nil {
			t.Fatalf("CreateDeploymentRevision: %v", err)
		}

		outcome, err := s.DeleteEnvironment(ctx, projectID, envID)
		if err != nil {
			t.Fatalf("DeleteEnvironment: %v", err)
		}
		if outcome != store.EnvDeleteActive {
			t.Fatalf("outcome = %v, want EnvDeleteActive", outcome)
		}
		if envs, _ := s.ListEnvironments(ctx, projectID); len(envs) != 1 {
			t.Fatalf("active-deploy env was removed: %+v", envs)
		}
	})

	// The delete is scoped to its project: naming it through another project → absent.
	t.Run("foreign project cannot delete", func(t *testing.T) {
		projectA := seedProject(t, s, "env-del-a")
		projectB := seedProject(t, s, "env-del-b")
		envID, err := s.EnsureEnvironment(ctx, projectA, "staging")
		if err != nil {
			t.Fatalf("EnsureEnvironment: %v", err)
		}
		outcome, err := s.DeleteEnvironment(ctx, projectB, envID)
		if err != nil {
			t.Fatalf("DeleteEnvironment: %v", err)
		}
		if outcome != store.EnvDeleteAbsent {
			t.Fatalf("outcome = %v, want EnvDeleteAbsent (wrong project)", outcome)
		}
		if envs, _ := s.ListEnvironments(ctx, projectA); len(envs) != 1 {
			t.Fatalf("scope guard removed the env: %+v", envs)
		}
	})

	// An absent id → EnvDeleteAbsent (→ 404 at the API), not an error.
	t.Run("absent environment", func(t *testing.T) {
		projectID := seedProject(t, s, "env-del-absent")
		outcome, err := s.DeleteEnvironment(ctx, projectID, uuid.New())
		if err != nil {
			t.Fatalf("DeleteEnvironment: %v", err)
		}
		if outcome != store.EnvDeleteAbsent {
			t.Fatalf("outcome = %v, want EnvDeleteAbsent", outcome)
		}
	})
}

func TestDeploymentRevision_LifecycleInProgressToSuccess(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("EnsureEnvironment: %v", err)
	}

	_, err = s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID,
		RunID:         runID,
		JobRunID:      jobID,
		Attempt:       0,
		Version:       "1.42.abc",
		DeployedBy:    "alice",
	})
	if err != nil {
		t.Fatalf("CreateDeploymentRevision: %v", err)
	}

	// While in_progress, there is no "current" deployment yet.
	if _, found, err := s.CurrentDeployment(ctx, envID); err != nil || found {
		t.Fatalf("CurrentDeployment while in_progress = found %v, err %v; want not-found", found, err)
	}

	// Terminal result finalises the in_progress revision of this
	// (job_run, attempt).
	n, err := s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusSuccess)
	if err != nil {
		t.Fatalf("FinalizeDeploymentRevision: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalize affected %d rows, want 1", n)
	}

	cur, found, err := s.CurrentDeployment(ctx, envID)
	if err != nil || !found {
		t.Fatalf("CurrentDeployment after success = found %v, err %v", found, err)
	}
	if cur.Version != "1.42.abc" || cur.Status != store.DeployStatusSuccess {
		t.Fatalf("current = %+v", cur)
	}
	if cur.FinishedAt == nil {
		t.Fatal("finished_at not stamped on success")
	}
	if cur.RunID == nil || *cur.RunID != runID {
		t.Fatalf("run link lost: %+v", cur.RunID)
	}

	// Re-delivered terminal result is a no-op (status guard).
	n, err = s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusSuccess)
	if err != nil {
		t.Fatalf("FinalizeDeploymentRevision (replay): %v", err)
	}
	if n != 0 {
		t.Fatalf("replay finalize affected %d rows, want 0 (already terminal)", n)
	}
}

func TestFinalizeDeploymentRevision_NoDeployBlockIsNoop(t *testing.T) {
	// A job with no deploy: block has no in_progress revision — the
	// result-path finalize must affect zero rows, not error.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, _, _, jobID, _ := seedRunningJob(t, pool)
	n, err := s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusSuccess)
	if err != nil {
		t.Fatalf("FinalizeDeploymentRevision: %v", err)
	}
	if n != 0 {
		t.Fatalf("affected %d rows for a non-deploy job, want 0", n)
	}
}

// TestDeploymentRevision_RetryAttemptsDontCrossFinalize is the
// regression for the retry/reaper corruption (review HIGH): attempt 0
// dies leaving an in_progress revision, attempt 1 is redispatched and
// succeeds. Finalising attempt 1 must NOT also flip the stale
// attempt-0 row to success — keying on (job_run, attempt) is what
// prevents two successes for one deploy.
func TestDeploymentRevision_RetryAttemptsDontCrossFinalize(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	// attempt 0 dispatched (and then the agent died — never finalised).
	revA, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "1.0.dead",
	})
	// reaper-requeue finalises the dead attempt as failed.
	if n, err := s.FinalizeDeploymentRevision(ctx, jobID, 0, store.DeployStatusFailed); err != nil || n != 1 {
		t.Fatalf("finalize dead attempt 0 = %d rows, %v; want 1", n, err)
	}
	// attempt 1 redispatched (same job_run id) and succeeds.
	revB, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 1, Version: "1.0.good",
	})
	if n, err := s.FinalizeDeploymentRevision(ctx, jobID, 1, store.DeployStatusSuccess); err != nil || n != 1 {
		t.Fatalf("finalize attempt 1 = %d rows, %v; want 1", n, err)
	}

	// Exactly ONE success, and it's attempt 1 (1.0.good). attempt 0
	// stays failed — never flipped to success by the attempt-1 finalize.
	hist, _ := s.ListDeploymentHistory(ctx, envID, 10)
	var successes int
	statusByID := map[uuid.UUID]string{}
	for _, h := range hist {
		statusByID[h.ID] = h.Status
		if h.Status == store.DeployStatusSuccess {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("got %d successes, want exactly 1 (one deploy, despite the retry)", successes)
	}
	if statusByID[revA] != store.DeployStatusFailed {
		t.Errorf("attempt-0 revision = %q, want failed", statusByID[revA])
	}
	if statusByID[revB] != store.DeployStatusSuccess {
		t.Errorf("attempt-1 revision = %q, want success", statusByID[revB])
	}
	cur, found, _ := s.CurrentDeployment(ctx, envID)
	if !found || cur.Version != "1.0.good" {
		t.Fatalf("current = %+v, want version 1.0.good", cur)
	}
}

func TestDeleteDeploymentRevision_OnlyInProgress(t *testing.T) {
	// The dispatch-failed cleanup path deletes the revision it just
	// created — but the SQL is scoped to in_progress so it can NEVER
	// erase a finalized audit row (a stale id collision, a re-delivery).
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	// in_progress revision is deletable (dispatch-failure cleanup).
	rev, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "1.0",
	})
	if err := s.DeleteDeploymentRevision(ctx, rev); err != nil {
		t.Fatalf("DeleteDeploymentRevision (in_progress): %v", err)
	}
	if hist, _ := s.ListDeploymentHistory(ctx, envID, 10); len(hist) != 0 {
		t.Fatalf("in_progress revision not deleted: %+v", hist)
	}

	// A finalized (success) revision must survive a delete attempt.
	rev2, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 1, Version: "2.0",
	})
	if _, err := s.FinalizeDeploymentRevision(ctx, jobID, 1, store.DeployStatusSuccess); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := s.DeleteDeploymentRevision(ctx, rev2); err != nil {
		t.Fatalf("DeleteDeploymentRevision (success, should no-op): %v", err)
	}
	if _, found, _ := s.CurrentDeployment(ctx, envID); !found {
		t.Fatal("delete erased a finalized audit row — must be scoped to in_progress")
	}
}

func TestCurrentDeployment_LatestSuccessWins(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobA, jobB := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	// Two successful deploys; control finished_at explicitly so the
	// "newest finished_at wins" rule is deterministic (NOW() on two
	// fast finalizes could collide).
	revOld, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobA, Attempt: 0, Version: "1.40.old", DeployedBy: "alice",
	})
	revNew, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobB, Attempt: 0, Version: "1.42.new", DeployedBy: "bob",
	})
	mustFinalizeAt(t, pool, revOld, "success", "2026-06-13T10:00:00Z")
	mustFinalizeAt(t, pool, revNew, "success", "2026-06-13T11:00:00Z")

	cur, found, err := s.CurrentDeployment(ctx, envID)
	if err != nil || !found {
		t.Fatalf("CurrentDeployment = found %v, err %v", found, err)
	}
	if cur.Version != "1.42.new" {
		t.Fatalf("current version = %q, want 1.42.new (newest finished_at)", cur.Version)
	}

	// A later FAILED deploy must NOT become current.
	revFail, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobA, Attempt: 1, Version: "1.43.broken", DeployedBy: "carol",
	})
	mustFinalizeAt(t, pool, revFail, "failed", "2026-06-13T12:00:00Z")
	cur, _, _ = s.CurrentDeployment(ctx, envID)
	if cur.Version != "1.42.new" {
		t.Fatalf("a failed deploy moved current to %q — must stay 1.42.new", cur.Version)
	}

	// History carries all three, newest first.
	hist, err := s.ListDeploymentHistory(ctx, envID, 10)
	if err != nil {
		t.Fatalf("ListDeploymentHistory: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history len = %d, want 3", len(hist))
	}
}

func TestListEnvironmentsWithCurrent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobA, jobB := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)

	// prod: a successful deploy → has a current.
	prod, _ := s.EnsureEnvironment(ctx, projectID, "production")
	rev, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: prod, RunID: runID, JobRunID: jobA, Attempt: 0, Version: "1.42.abc", DeployedBy: "alice",
	})
	mustFinalizeAt(t, pool, rev, "success", "2026-06-13T10:00:00Z")

	// staging: only an in_progress deploy → NO current (in_progress
	// never reads as "current"; only success does).
	staging, _ := s.EnsureEnvironment(ctx, projectID, "staging")
	_, _ = s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: staging, RunID: runID, JobRunID: jobB, Attempt: 0, Version: "1.43.wip",
	})

	envs, err := s.ListEnvironmentsWithCurrent(ctx, projectID)
	if err != nil {
		t.Fatalf("ListEnvironmentsWithCurrent: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("got %d environments, want 2", len(envs))
	}
	byName := map[string]store.EnvironmentWithCurrent{}
	for _, e := range envs {
		byName[e.Name] = e
	}
	if c := byName["production"].Current; c == nil || c.Version != "1.42.abc" || c.DeployedBy != "alice" {
		t.Fatalf("production current = %+v, want version 1.42.abc by alice", byName["production"].Current)
	}
	if byName["staging"].Current != nil {
		t.Fatalf("staging has only an in_progress deploy — current must be nil, got %+v", byName["staging"].Current)
	}
}

func TestEnvironmentBelongsToProject(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	p1 := seedProject(t, s, "proj-one")
	p2 := seedProject(t, s, "proj-two")
	env1, _ := s.EnsureEnvironment(ctx, p1, "production")

	ok, err := s.EnvironmentBelongsToProject(ctx, p1, env1)
	if err != nil || !ok {
		t.Fatalf("env1 should belong to p1: ok=%v err=%v", ok, err)
	}
	// Cross-project read must be rejected (the scope guard).
	ok, err = s.EnvironmentBelongsToProject(ctx, p2, env1)
	if err != nil {
		t.Fatalf("scope check err: %v", err)
	}
	if ok {
		t.Fatal("env1 must NOT belong to p2 — scope guard broken")
	}
}

func TestRollbackToRevision_HappyPath(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")

	// A past successful deploy whose run still exists.
	rev, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "1.40.old",
	})
	mustFinalizeAt(t, pool, rev, "success", "2026-06-13T10:00:00Z")
	mustMarkJobTerminal(t, pool, jobID) // RerunJob requires a terminal row

	res, err := s.RollbackToRevision(ctx, store.RollbackInput{
		ProjectID: projectID, EnvironmentID: envID, RevisionID: rev, TriggeredBy: "user:alice",
	})
	if err != nil {
		t.Fatalf("RollbackToRevision: %v", err)
	}
	if res.JobRunID != jobID {
		t.Fatalf("re-ran job %s, want the deploy job %s of the target run", res.JobRunID, jobID)
	}
	// The deploy job is back to queued, flagged so the next dispatch
	// records is_rollback=true.
	if !deployRollbackOf(t, s, runID, jobID) {
		t.Fatal("deploy_rollback not set after rollback")
	}
}

func TestRollbackToRevision_Rejections(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, jobB := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	otherProject := seedProject(t, s, "other")
	envID, _ := s.EnsureEnvironment(ctx, projectID, "production")
	otherEnv, _ := s.EnsureEnvironment(ctx, projectID, "staging")

	// in_progress revision (never finalised) → not successful.
	inProgress, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Attempt: 0, Version: "wip",
	})
	// success revision but its run was garbage-collected (job_run NULL).
	runGone, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, Version: "orphan", // RunID/JobRunID left nil → NULL
	})
	mustFinalizeAt(t, pool, runGone, "success", "2026-06-13T09:00:00Z")
	// success revision in a DIFFERENT environment.
	wrongEnv, _ := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: otherEnv, RunID: runID, JobRunID: jobB, Attempt: 0, Version: "2.0",
	})
	mustFinalizeAt(t, pool, wrongEnv, "success", "2026-06-13T09:30:00Z")

	tests := []struct {
		name      string
		projectID uuid.UUID
		envID     uuid.UUID
		revID     uuid.UUID
		want      error
	}{
		{"env not in project", otherProject, envID, inProgress, store.ErrEnvironmentNotFound},
		{"revision not found", projectID, envID, uuid.New(), store.ErrRevisionNotFound},
		{"revision wrong environment", projectID, envID, wrongEnv, store.ErrRevisionWrongEnvironment},
		{"revision not successful", projectID, envID, inProgress, store.ErrRollbackNotSuccessful},
		{"run garbage-collected", projectID, envID, runGone, store.ErrRollbackRunGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.RollbackToRevision(ctx, store.RollbackInput{
				ProjectID: tt.projectID, EnvironmentID: tt.envID, RevisionID: tt.revID, TriggeredBy: "u",
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// mustFinalizeAt finalises a revision with an explicit finished_at so
// ordering assertions are deterministic.
func mustFinalizeAt(t *testing.T, pool *pgxpool.Pool, revID uuid.UUID, status, ts string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE deployment_revisions SET status = $2, finished_at = $3 WHERE id = $1`,
		revID, status, ts)
	if err != nil {
		t.Fatalf("finalize at %s: %v", ts, err)
	}
}
