package scheduler_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/deploysvc"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func freezeActor() store.FreezeActor {
	return store.FreezeActor{ID: uuid.New(), Email: "ops@acme.io"}
}

func jobStatusOf(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("job status: %v", err)
	}
	return status
}

// A frozen environment holds the deploy at the EARLY GATE: the job never reaches
// an agent, and the run carries the reason that explains it.
func TestDispatchRun_FrozenEnvironmentHoldsDeploy(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-hold", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-hold")

	// Frozen BEFORE anything has ever deployed to `prod` — there is no
	// environments row yet, which is exactly the case an id-keyed freeze would
	// miss and the first deploy would slip through.
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "month-end close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	agentID := seedAgentRow(t, pool, "freeze-hold-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)

	assertNoAssignment(t, sess)
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("job status = %q, want queued (a freeze holds, it never fails the job)", got)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want frozen-deploy:prod", got)
	}
}

// Repeated ticks on a still-frozen run must not rewrite queue_reason: the run is
// not moving, and a per-tick UPDATE is pure WAL + dead tuples forever.
func TestDispatchRun_FrozenRunDoesNotChurnQueueReason(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-churn", domain.SupersedeOff)
	projectID := projectIDForSlug(t, pool, "freeze-churn")
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	agentID := seedAgentRow(t, pool, "freeze-churn-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)
	// xmin is the inserting/updating transaction id: unchanged xmin == no row
	// version was written. A tuple-level assertion is the only one that actually
	// proves "no write" — comparing the VALUE would pass even with churn.
	var xmin1, xmin2 string
	if err := pool.QueryRow(ctx, `SELECT xmin::text FROM runs WHERE id=$1`, run.RunID).Scan(&xmin1); err != nil {
		t.Fatalf("xmin: %v", err)
	}
	for range 3 {
		sched.DispatchRun(ctx, run.RunID)
	}
	if err := pool.QueryRow(ctx, `SELECT xmin::text FROM runs WHERE id=$1`, run.RunID).Scan(&xmin2); err != nil {
		t.Fatalf("xmin (2): %v", err)
	}
	if xmin1 != xmin2 {
		t.Fatalf("run row was rewritten across ticks (xmin %s -> %s): queue_reason churn", xmin1, xmin2)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want it preserved across ticks", got)
	}
}

// Once the environment thaws, the dedicated freeze clear drops the stamp and the
// deploy dispatches — no operator action, no waiting for a special path.
func TestDispatchRun_ThawReleasesTheDeploy(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-thaw-sched", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-thaw-sched")
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	agentID := seedAgentRow(t, pool, "freeze-thaw-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)
	markReady(t, sessions, sess.ID)

	sched.DispatchRun(ctx, run.RunID)
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("pre-thaw job status = %q, want queued", got)
	}

	if _, err := s.UnfreezeEnvironment(ctx, projectID, "prod", freezeActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	sched.DispatchRun(ctx, run.RunID)

	if got := jobStatusOf(t, pool, jobID); got != "running" {
		t.Fatalf("post-thaw job status = %q, want running", got)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "" {
		t.Fatalf("queue_reason = %q, want cleared after the thaw", got)
	}
}

// A run held by a freeze AND a supersede AT THE SAME TICK gets exactly ONE
// queue_reason, written at most once.
//
// This needs TWO deploy jobs in the same stage, and that is the whole point: the
// early freeze gate `continue`s before beginDeployGuard, so a single job that is
// frozen never reaches the supersede gate and the two blockers never coexist.
// One frozen env plus one supersede-blocked SIBLING is the only shape that
// actually exercises holdPriority.
//
// Run in both job orderings, because the loop visits jobs by name: the winner
// must come from the priority order, never from which job happened to be first.
func TestDispatchRun_MixedBlockersStampQueueReasonOnce(t *testing.T) {
	orderings := []struct {
		name              string
		frozenJob, supJob string
	}{
		// ListDispatchableJobs orders by name, so these two put the frozen job
		// first and second respectively.
		{"frozen job visited first", "a-ship-prod", "b-ship-staging"},
		{"supersede job visited first", "b-ship-prod", "a-ship-staging"},
	}
	for i, tc := range orderings {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.SetupPool(t)
			s := store.New(pool)
			sessions := grpcsrv.NewSessionStore()
			sched := scheduler.New(s, sessions, quietLogger(), testDSN)
			ctx := context.Background()

			slug := "freeze-mixed-" + strconv.Itoa(i)
			pipelineID, older, newer, projectID := seedTwoEnvSupersedeRuns(t, pool, slug,
				tc.frozenJob, "prod", tc.supJob, "staging")

			// staging is supersede-blocked: a newer run in the lane already
			// cleared the gate for it.
			if _, err := pool.Exec(ctx, `
				INSERT INTO run_gate_pass (run_id, pipeline_id, ref, counter, environment)
				VALUES ($1, $2, 'main', $3, 'staging')
			`, newer.RunID, pipelineID, newer.Counter); err != nil {
				t.Fatalf("insert newer marker: %v", err)
			}
			// prod is frozen.
			if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
				t.Fatalf("freeze: %v", err)
			}

			agentID := seedAgentRow(t, pool, slug+"-agent")
			sess := sessions.CreateSession(agentID, nil, 4, 0)
			markReady(t, sessions, sess.ID)

			sched.DispatchRun(ctx, older.RunID)

			// Both blockers fired this tick. The FREEZE wins: it is the one a
			// human declared and only a human can lift, so reporting
			// "superseded" would send the operator to the wrong place while
			// production is actually change-frozen.
			if got := queueReasonOf(t, pool, older.RunID); got != "frozen-deploy:prod" {
				t.Fatalf("queue_reason = %q, want frozen-deploy:prod (freeze outranks supersede)", got)
			}
			// Neither deploy was admitted.
			if got := jobStatusOf(t, pool, jobIDByName(t, pool, older.RunID, tc.frozenJob)); got != "queued" {
				t.Fatalf("frozen job = %q, want queued", got)
			}
			if got := jobStatusOf(t, pool, jobIDByName(t, pool, older.RunID, tc.supJob)); got != "queued" {
				t.Fatalf("supersede-blocked job = %q, want queued", got)
			}

			// ONE write: repeated ticks with BOTH blockers still active must not
			// produce another row version.
			var xmin1, xmin2 string
			if err := pool.QueryRow(ctx, `SELECT xmin::text FROM runs WHERE id=$1`, older.RunID).Scan(&xmin1); err != nil {
				t.Fatalf("xmin: %v", err)
			}
			for range 3 {
				sched.DispatchRun(ctx, older.RunID)
			}
			if err := pool.QueryRow(ctx, `SELECT xmin::text FROM runs WHERE id=$1`, older.RunID).Scan(&xmin2); err != nil {
				t.Fatalf("xmin (2): %v", err)
			}
			if xmin1 != xmin2 {
				t.Fatalf("run row rewritten across ticks (xmin %s -> %s): mixed-blocker churn", xmin1, xmin2)
			}

			// Thawing hands the explanation to the remaining blocker — the
			// handoff is the second write, and the only one.
			if _, err := s.UnfreezeEnvironment(ctx, projectID, "prod", freezeActor()); err != nil {
				t.Fatalf("unfreeze: %v", err)
			}
			sched.DispatchRun(ctx, older.RunID)
			if got := queueReasonOf(t, pool, older.RunID); got != "supersede-blocked:staging" {
				t.Fatalf("queue_reason after thaw = %q, want supersede-blocked:staging", got)
			}
		})
	}
}

// A `serial-busy` run whose predecessor finishes must clear to NULL and
// dispatch: the freeze work must not have stranded the generic transition.
func TestDispatchRun_SerialBusyStillClearsOnRealTransition(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	_, older, newer := seedSerialDeployRuns(t, pool, "freeze-serial")
	agentID := seedAgentRow(t, pool, "freeze-serial-agent")
	sess := sessions.CreateSession(agentID, nil, 4, 0)
	markReady(t, sessions, sess.ID)

	// The older run goes running; the newer one queues behind it.
	sched.DispatchRun(ctx, older.RunID)
	sched.DispatchRun(ctx, newer.RunID)
	if got := queueReasonOf(t, pool, newer.RunID); !strings.HasPrefix(got, "serial-busy:") {
		t.Fatalf("queue_reason = %q, want serial-busy:*", got)
	}

	// Predecessor finishes -> the gate clears and the successor dispatches with
	// queue_reason back to NULL.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, older.RunID); err != nil {
		t.Fatalf("finish predecessor: %v", err)
	}
	sched.DispatchRun(ctx, newer.RunID)
	if got := queueReasonOf(t, pool, newer.RunID); got != "" {
		t.Fatalf("queue_reason = %q, want cleared once the predecessor finished", got)
	}
	if got := jobStatusOf(t, pool, soleJobID(t, newer)); got != "running" {
		t.Fatalf("successor job = %q, want running", got)
	}
}

// Two frozen environments in one active stage: the run-level explanation is the
// first in NAME order and stays put until THAT one thaws — no alternation.
func TestDispatchRun_TwoFrozenEnvsStableWinner(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	run, projectID := seedTwoEnvDeployRun(t, pool, "freeze-two", "prod", "staging")
	for _, env := range []string{"prod", "staging"} {
		if _, err := s.FreezeEnvironment(ctx, projectID, env, freezeActor(), "close"); err != nil {
			t.Fatalf("freeze %s: %v", env, err)
		}
	}
	agentID := seedAgentRow(t, pool, "freeze-two-agent")
	sess := sessions.CreateSession(agentID, nil, 4, 0)
	markReady(t, sessions, sess.ID)

	for range 3 {
		sched.DispatchRun(ctx, run.RunID)
		if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
			t.Fatalf("queue_reason = %q, want a stable frozen-deploy:prod (name order)", got)
		}
	}

	// Thawing the NON-winner does not change the explanation...
	if _, err := s.UnfreezeEnvironment(ctx, projectID, "staging", freezeActor()); err != nil {
		t.Fatalf("unfreeze staging: %v", err)
	}
	sched.DispatchRun(ctx, run.RunID)
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want frozen-deploy:prod after the non-winner thawed", got)
	}
	// ...and the staging deploy is free to run while prod stays held. A freeze
	// names the dominant blocker; it is NOT a whole-run halt.
	if got := jobStatusOf(t, pool, jobIDByName(t, pool, run.RunID, "ship-staging")); got != "running" {
		t.Fatalf("staging job = %q, want running (a sibling freeze must not halt it)", got)
	}
	if got := jobStatusOf(t, pool, jobIDByName(t, pool, run.RunID, "ship-prod")); got != "queued" {
		t.Fatalf("prod job = %q, want queued (still frozen)", got)
	}
}

// A native deploy (server-managed, no agent) is held by the freeze too — and the
// refusal is a CLEAN SKIP: the job stays queued, no revision, no watch, no sync.
func TestDispatchRun_FrozenEnvironmentBlocksNativeTakeover(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sync := &fakeSyncer{}
	sched := scheduler.New(s, sessions, quietLogger(), testDSN).
		WithNativeDeployer(deploysvc.NewNativeDeployer(sync, s, quietLogger()))
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-native", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-native")
	registerDeployTarget(t, s, projectID, "prod", "trigger")
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	sched.DispatchRun(ctx, run.RunID)

	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("job status = %q, want queued (a freeze must not fail the deploy)", got)
	}
	if sync.calls != 0 {
		t.Fatalf("sync called %d times during a freeze, want 0", sync.calls)
	}
	var revisions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deployment_revisions WHERE job_run_id=$1`, jobID).Scan(&revisions); err != nil {
		t.Fatalf("revision count: %v", err)
	}
	if revisions != 0 {
		t.Fatalf("deployment_revisions = %d, want 0 (nothing was admitted)", revisions)
	}
	if got := queueReasonOf(t, pool, run.RunID); got != "frozen-deploy:prod" {
		t.Fatalf("queue_reason = %q, want frozen-deploy:prod", got)
	}
}

// A freeze landing AFTER the pre-scan is caught by the authoritative re-check
// inside the admitting transaction. Driving the store method directly is the
// point: it is the boundary the whole guarantee rests on.
func TestAssignDeployJobIfEnvNotFrozen_IsTheAdmissionBoundary(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-admit", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-admit")
	agentID := seedAgentRow(t, pool, "freeze-admit-agent")

	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	_, outcome, err := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "prod")
	if err != nil {
		t.Fatalf("admission: %v", err)
	}
	// Distinguishable from a lost CAS — collapsing the two into a bool would
	// make a freeze indistinguishable from a scheduler race in logs and tests.
	if outcome != store.DeployAdmissionFrozen {
		t.Fatalf("outcome = %q, want frozen", outcome)
	}
	if got := jobStatusOf(t, pool, jobID); got != "queued" {
		t.Fatalf("job status = %q, want queued (nothing admitted)", got)
	}

	if _, err := s.UnfreezeEnvironment(ctx, projectID, "prod", freezeActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	assigned, outcome, err := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "prod")
	if err != nil {
		t.Fatalf("admission after thaw: %v", err)
	}
	if outcome != store.DeployAdmitted || assigned.ID != jobID {
		t.Fatalf("outcome = %q assigned = %+v, want admitted for %s", outcome, assigned, jobID)
	}

	// Re-admitting the same job now loses the CAS — a THIRD outcome, not an error.
	_, outcome, err = s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "prod")
	if err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if outcome != store.DeployAdmissionLost {
		t.Fatalf("outcome = %q, want lost", outcome)
	}
}

// The generic AssignJob is deliberately left untouched: the common non-deploy hot
// path must not pay for a freeze lock + probe it can never need.
func TestAssignJob_IsNotFreezeAware(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, run, _ := seedDeployRuns(t, pool, "freeze-assignjob", domain.SupersedeOff)
	jobID := soleJobID(t, run)
	projectID := projectIDForSlug(t, pool, "freeze-assignjob")
	agentID := seedAgentRow(t, pool, "freeze-assignjob-agent")

	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", freezeActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	_, ok, err := s.AssignJob(ctx, jobID, agentID)
	if err != nil {
		t.Fatalf("AssignJob: %v", err)
	}
	if !ok {
		t.Fatal("AssignJob refused a job during a freeze — it must stay freeze-agnostic; " +
			"the deploy-specific AssignDeployJobIfEnvNotFrozen owns that check")
	}
}

// ---- fixtures -------------------------------------------------------------

// jobIDByName resolves one job_run of a run by its declared name.
func jobIDByName(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM job_runs WHERE run_id=$1 AND name=$2`, runID, name).Scan(&id); err != nil {
		t.Fatalf("job %q: %v", name, err)
	}
	return id
}

// seedTwoEnvDeployRun applies a pipeline with TWO deploy jobs in the SAME stage,
// each targeting a different environment — the shape that exercises "one frozen,
// one dispatchable" and the stable-winner rule.
func seedTwoEnvDeployRun(t *testing.T, pool *pgxpool.Pool, slug, envA, envB string) (store.RunCreated, uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := domain.GitFingerprint(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "deploy", Supersede: domain.SupersedeOff, Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{
					Name: "ship-" + envA, Stage: "deploy", Image: "alpine:3.19",
					Tasks:  []domain.Task{{Script: "echo a"}},
					Deploy: &domain.DeploySpec{Environment: envA},
				},
				{
					Name: "ship-" + envB, Stage: "deploy", Image: "alpine:3.19",
					Tasks:  []domain.Task{{Script: "echo b"}},
					Deploy: &domain.DeploySpec{Environment: envB},
				},
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
		Revision: seededRunCommit, Branch: "main", Provider: "github",
		Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return run, applied.ProjectID
}

// seedSerialDeployRuns applies a SERIAL deploy pipeline and creates two runs, so
// the second queues behind the first on the concurrency gate.
func seedSerialDeployRuns(t *testing.T, pool *pgxpool.Pool, slug string) (uuid.UUID, store.RunCreated, store.RunCreated) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := domain.GitFingerprint(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "deploy", Concurrency: domain.ConcurrencySerial,
			Supersede: domain.SupersedeOff, Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{
				Name: "ship", Stage: "deploy", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo deploy"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	mk := func(rev, delivery string) store.RunCreated {
		run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID: pipelineID, MaterialID: materialID,
			Revision: rev, Branch: "main", Provider: "github",
			Delivery: delivery, TriggeredBy: "system:webhook",
		})
		if err != nil {
			t.Fatalf("run %s: %v", delivery, err)
		}
		return run
	}
	return pipelineID, mk("aaa0123456789aaa0123456789aaa0123456789a", slug+"-older"),
		mk("bbb0123456789bbb0123456789bbb0123456789b", slug+"-newer")
}

// seedTwoEnvSupersedeRuns applies a supersede=branch pipeline with TWO deploy
// jobs in the SAME stage (different environments, caller-chosen names so the
// dispatch loop's name ordering can be varied) and creates two runs in the lane.
func seedTwoEnvSupersedeRuns(t *testing.T, pool *pgxpool.Pool, slug, jobA, envA, jobB, envB string) (uuid.UUID, store.RunCreated, store.RunCreated, uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/org/" + slug
	fp := domain.GitFingerprint(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "deploy", Supersede: domain.SupersedeBranch, Stages: []string{"deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{
					Name: jobA, Stage: "deploy", Image: "alpine:3.19",
					Tasks:  []domain.Task{{Script: "echo a"}},
					Deploy: &domain.DeploySpec{Environment: envA, Version: "v1"},
				},
				{
					Name: jobB, Stage: "deploy", Image: "alpine:3.19",
					Tasks:  []domain.Task{{Script: "echo b"}},
					Deploy: &domain.DeploySpec{Environment: envB, Version: "v1"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applied.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	mk := func(rev, delivery string) store.RunCreated {
		run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID: pipelineID, MaterialID: materialID,
			Revision: rev, Branch: "main", Provider: "github",
			Delivery: delivery, TriggeredBy: "system:webhook",
		})
		if err != nil {
			t.Fatalf("run %s: %v", delivery, err)
		}
		return run
	}
	return pipelineID,
		mk("aaa0123456789aaa0123456789aaa0123456789a", slug+"-older"),
		mk("bbb0123456789bbb0123456789bbb0123456789b", slug+"-newer"),
		applied.ProjectID
}
