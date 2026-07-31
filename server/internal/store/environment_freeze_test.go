package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func testActor() store.FreezeActor {
	return store.FreezeActor{
		ID:    uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Email: "alice@acme.io",
		Name:  "Alice",
	}
}

// freezeAudit reads back the audit rows for a project's freeze events.
func freezeAudit(t *testing.T, pool *pgxpool.Pool, action string) []struct {
	ActorEmail string
	TargetID   string
	TargetType string
	Metadata   string
} {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT actor_email, target_id, target_type, metadata::text
		   FROM audit_events WHERE action = $1 ORDER BY at`, action)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []struct {
		ActorEmail string
		TargetID   string
		TargetType string
		Metadata   string
	}
	for rows.Next() {
		var r struct {
			ActorEmail string
			TargetID   string
			TargetType string
			Metadata   string
		}
		if err := rows.Scan(&r.ActorEmail, &r.TargetID, &r.TargetType, &r.Metadata); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestFreezeEnvironment_IdempotentAndAudited(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-idem")

	froze, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "month-end close")
	if err != nil {
		t.Fatalf("FreezeEnvironment: %v", err)
	}
	if !froze {
		t.Fatal("first freeze reported no change")
	}

	st, found, err := s.EnvironmentFrozenState(ctx, projectID, "production")
	if err != nil || !found {
		t.Fatalf("EnvironmentFrozenState: found=%v err=%v", found, err)
	}
	firstFrozenAt := st.FrozenAt

	// Re-freezing must NOT reset the original record: overwriting who stopped
	// production, when, and why is exactly the history this feature exists to
	// keep, and it must not emit a second audit event either.
	froze, err = s.FreezeEnvironment(ctx, projectID, "production", store.FreezeActor{
		ID: uuid.New(), Email: "mallory@acme.io",
	}, "different reason")
	if err != nil {
		t.Fatalf("re-freeze: %v", err)
	}
	if froze {
		t.Error("re-freeze reported a state change")
	}
	st, _, err = s.EnvironmentFrozenState(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("state after re-freeze: %v", err)
	}
	if st.FrozenBy != "alice@acme.io" || st.Reason != "month-end close" || !st.FrozenAt.Equal(firstFrozenAt) {
		t.Errorf("re-freeze overwrote the original: %+v", st)
	}

	events := freezeAudit(t, pool, store.AuditActionEnvironmentFreeze)
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 (idempotent freeze must not re-audit)", len(events))
	}
	// target_id is (project, name) — NOT environments.id, which may not exist.
	if want := projectID.String() + ":production"; events[0].TargetID != want {
		t.Errorf("target_id = %q, want %q", events[0].TargetID, want)
	}
	if events[0].TargetType != "environment" {
		t.Errorf("target_type = %q, want environment", events[0].TargetType)
	}
	if events[0].ActorEmail != "alice@acme.io" {
		t.Errorf("actor_email = %q, want the canonical actor", events[0].ActorEmail)
	}
	if !strings.Contains(events[0].Metadata, "month-end close") {
		t.Errorf("metadata missing the reason: %s", events[0].Metadata)
	}
}

func TestUnfreezeEnvironment_IdempotentAndAudited(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-thaw")

	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	thawed, err := s.UnfreezeEnvironment(ctx, projectID, "production", testActor())
	if err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if !thawed {
		t.Fatal("unfreeze reported no change")
	}
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); found {
		t.Error("environment still frozen after unfreeze")
	}

	// Unfreezing again is a no-op and must not emit a second audit event.
	thawed, err = s.UnfreezeEnvironment(ctx, projectID, "production", testActor())
	if err != nil {
		t.Fatalf("second unfreeze: %v", err)
	}
	if thawed {
		t.Error("second unfreeze reported a state change")
	}
	if got := len(freezeAudit(t, pool, store.AuditActionEnvironmentUnfreeze)); got != 1 {
		t.Fatalf("unfreeze audit events = %d, want 1", got)
	}
}

func TestFreezeEnvironment_ActorFallsThroughWhitespaceAndOversizedEmail(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-actor")

	// OIDC claims are persisted raw. A whitespace-only email must be TRIMMED
	// and then fall through to Name — selecting it and letting the DB CHECK
	// reject the row would turn an IdP quirk into a 500.
	if _, err := s.FreezeEnvironment(ctx, projectID, "ws", store.FreezeActor{
		ID: uuid.New(), Email: "   ", Name: "Alice",
	}, "r"); err != nil {
		t.Fatalf("whitespace email freeze: %v", err)
	}
	if st, _, _ := s.EnvironmentFrozenState(ctx, projectID, "ws"); st.FrozenBy != "Alice" {
		t.Errorf("frozen_by = %q, want the Name fallback", st.FrozenBy)
	}

	// A pathologically long email falls through the same way, all the way to
	// the id when the name is unusable too.
	id := uuid.New()
	if _, err := s.FreezeEnvironment(ctx, projectID, "big", store.FreezeActor{
		ID: id, Email: strings.Repeat("a", 400) + "@acme.io", Name: "  ",
	}, "r"); err != nil {
		t.Fatalf("oversized email freeze: %v", err)
	}
	if st, _, _ := s.EnvironmentFrozenState(ctx, projectID, "big"); st.FrozenBy != id.String() {
		t.Errorf("frozen_by = %q, want the id fallback", st.FrozenBy)
	}

	// Nothing usable at all is a typed error raised BEFORE the insert.
	_, err := s.FreezeEnvironment(ctx, projectID, "none", store.FreezeActor{}, "r")
	if !errors.Is(err, store.ErrFreezeActorUnusable) {
		t.Errorf("err = %v, want ErrFreezeActorUnusable", err)
	}
}

func TestFreezeEnvironment_ValidatesNameAndReason(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-validate")

	// Re-validated in the STORE, not only at the API: internal and test callers
	// bypass the handler entirely (defence in depth).
	tests := []struct {
		name, env, reason string
		want              error
	}{
		{"slash in name", "prod/uction", "r", store.ErrFreezeNameInvalid},
		{"leading dash", "-prod", "r", store.ErrFreezeNameInvalid},
		{"empty name", "", "r", store.ErrFreezeNameInvalid},
		{"whitespace-only name", "   ", "r", store.ErrFreezeNameInvalid},
		{"oversized name", strings.Repeat("a", 65), "r", store.ErrFreezeNameInvalid},
		{"empty reason", "production", "", store.ErrFreezeReasonInvalid},
		{"whitespace-only reason", "production", "  \t ", store.ErrFreezeReasonInvalid},
		{"oversized reason", "production", strings.Repeat("x", 501), store.ErrFreezeReasonInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.FreezeEnvironment(ctx, projectID, tt.env, testActor(), tt.reason)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}

	// Nothing above may have landed a row.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM environment_freezes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rejected inputs wrote %d rows", n)
	}
}

func TestEnvironmentFreezes_DBChecksRejectDirectWrites(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-checks")

	// The CHECKs use btrim so a caller going straight to SQL still cannot
	// persist a blank/oversized name, reason or actor.
	bad := []struct {
		name              string
		env, actor, reasn string
	}{
		{"blank name", "   ", "a@b.c", "r"},
		{"oversized name", strings.Repeat("a", 65), "a@b.c", "r"},
		{"blank reason", "production", "a@b.c", "  "},
		{"oversized reason", "production", "a@b.c", strings.Repeat("x", 501)},
		{"blank actor", "production", "  ", "r"},
		{"oversized actor", "production", strings.Repeat("a", 321), "r"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				`INSERT INTO environment_freezes (project_id, name, frozen_by, reason) VALUES ($1,$2,$3,$4)`,
				projectID, tt.env, tt.actor, tt.reasn)
			if err == nil {
				t.Fatal("direct insert succeeded, want a CHECK violation")
			}
		})
	}
}

func TestFrozenEnvironments_BatchedOrderedAndScoped(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-batch")
	otherID := seedProject(t, s, "freeze-batch-other")

	for _, env := range []string{"production", "staging", "canary"} {
		if _, err := s.FreezeEnvironment(ctx, projectID, env, testActor(), "r"); err != nil {
			t.Fatalf("freeze %s: %v", env, err)
		}
	}
	// A same-named freeze in ANOTHER project must not leak in.
	if _, err := s.FreezeEnvironment(ctx, otherID, "dev", testActor(), "r"); err != nil {
		t.Fatalf("freeze other: %v", err)
	}

	got, err := s.FrozenEnvironments(ctx, projectID, []string{"staging", "production", "dev", "qa"})
	if err != nil {
		t.Fatalf("FrozenEnvironments: %v", err)
	}
	// Sorted by name — the scheduler picks got[0] as the run-level queue_reason
	// owner, so a non-deterministic order would make the stamp alternate.
	if len(got) != 2 || got[0] != "production" || got[1] != "staging" {
		t.Fatalf("frozen = %v, want [production staging]", got)
	}

	// Empty input must not even hit the database.
	if got, err := s.FrozenEnvironments(ctx, projectID, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty names: got %v err %v", got, err)
	}
}

func TestListRunsHeldByEnvironment_ExactMatchNotLike(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, _, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)

	// `_` is legal in an environment name AND is a LIKE single-char wildcard.
	// A LIKE-based lookup for `staging_eu` would also match `stagingXeu`, so
	// this asserts the exact-equality contract.
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status='queued', queue_reason='frozen-deploy:stagingXeu' WHERE id=$1`, runID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	held, err := s.ListRunsHeldByEnvironment(ctx, projectID, "staging_eu")
	if err != nil {
		t.Fatalf("ListRunsHeldByEnvironment: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("LIKE wildcard matched a different environment: %v", held)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE runs SET queue_reason='frozen-deploy:staging_eu' WHERE id=$1`, runID); err != nil {
		t.Fatalf("stamp exact: %v", err)
	}
	held, err = s.ListRunsHeldByEnvironment(ctx, projectID, "staging_eu")
	if err != nil {
		t.Fatalf("ListRunsHeldByEnvironment (exact): %v", err)
	}
	if len(held) != 1 || held[0] != runID {
		t.Fatalf("held = %v, want [%s]", held, runID)
	}
}

func TestProjectEnvFreezeLockKey_DistinctNamespace(t *testing.T) {
	t.Parallel()
	projectID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	pipelineID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	// Stable across calls — the key is computed independently in every process
	// that takes the lock, so a non-deterministic hash would silently stop
	// serialising anything. (Two variables rather than a direct self-comparison:
	// staticcheck reads the inline form as a tautology.)
	first := store.ProjectEnvFreezeLockKey(projectID, "production")
	second := store.ProjectEnvFreezeLockKey(projectID, "production")
	if first != second {
		t.Fatalf("key is not stable: %d vs %d", first, second)
	}
	// Distinct per env and per project: a freeze on one must not serialise the
	// other.
	if store.ProjectEnvFreezeLockKey(projectID, "production") ==
		store.ProjectEnvFreezeLockKey(projectID, "staging") {
		t.Error("key collides across environments")
	}
	if store.ProjectEnvFreezeLockKey(projectID, "production") ==
		store.ProjectEnvFreezeLockKey(uuid.New(), "production") {
		t.Error("key collides across projects")
	}
	// And NOT the gate-pass key: they are taken in a fixed order by the same
	// transactions, so sharing a key space would collapse the ordering the
	// deadlock-safety argument depends on.
	if store.ProjectEnvFreezeLockKey(projectID, "production") ==
		store.LaneEnvLockKey(pipelineID, "branch", "main", "production") {
		t.Error("freeze key collides with the lane-env gate-pass key")
	}
}

// The GUARANTEE, stated precisely: once FreezeEnvironment has COMMITTED, no
// admission of a deploy to that environment can commit afterwards.
//
// This does NOT assert "the freeze always wins a race" — that would be false and
// flaky. It asserts the ordering contract: the test lets the freeze commit
// first, then attempts an admission, and the admission must be refused. A freeze
// that loses the lock race simply applies to the NEXT admission.
func TestFreezeSerialisesAgainstAdmission(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	// Put the job back to dispatchable so the admission has something to claim.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='queued', agent_id=NULL, started_at=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	jobProject := projectIDForRun(t, pool, runID)
	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1,'h') RETURNING id`,
		"freeze-serialise-agent").Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	// Freeze commits FIRST.
	if _, err := s.FreezeEnvironment(ctx, jobProject, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// Every admission attempted afterwards is refused, repeatedly.
	for i := range 5 {
		_, outcome, err := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, jobProject, "production")
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		if outcome != store.DeployAdmissionFrozen {
			t.Fatalf("admission %d = %q, want frozen after the freeze committed", i, outcome)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobID).Scan(&status); err != nil {
		t.Fatalf("job status: %v", err)
	}
	if status != "queued" {
		t.Fatalf("job status = %q, want queued (nothing was admitted)", status)
	}
}

// Concurrent freeze + admission never deadlock and never both "succeed": the
// per-(project, env) advisory lock serialises them, so the outcome is always one
// of the two consistent orderings.
func TestFreezeAndAdmissionConcurrentlyDoNotDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='queued', agent_id=NULL, started_at=NULL WHERE id=$1`, jobID); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	projectID := projectIDForRun(t, pool, runID)
	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ($1,'h') RETURNING id`,
		"freeze-concurrent-agent").Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}

	type admitResult struct {
		outcome store.DeployAdmission
		err     error
	}
	admitCh := make(chan admitResult, 1)
	freezeCh := make(chan error, 1)
	start := make(chan struct{})

	go func() {
		<-start
		_, outcome, err := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "production")
		admitCh <- admitResult{outcome, err}
	}()
	go func() {
		<-start
		_, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close")
		freezeCh <- err
	}()
	close(start)

	admit := <-admitCh
	if err := <-freezeCh; err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if admit.err != nil {
		t.Fatalf("admission: %v", admit.err)
	}
	// Either ordering is legitimate; what must NOT happen is a deadlock (the
	// test would hang) or an admitted deploy that survives a committed freeze.
	if admit.outcome == store.DeployAdmitted {
		// The admission won the lock. The freeze then applies from here on.
		_, outcome, err := s.AssignDeployJobIfEnvNotFrozen(ctx, jobID, agentID, projectID, "production")
		if err != nil {
			t.Fatalf("post-freeze admission: %v", err)
		}
		if outcome != store.DeployAdmissionFrozen {
			t.Fatalf("post-freeze admission = %q, want frozen", outcome)
		}
	}
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); !found {
		t.Fatal("the environment is not frozen after FreezeEnvironment returned")
	}
}

// The freeze and its audit row commit TOGETHER or not at all.
//
// audit.Emit is best-effort everywhere else by design — a failed audit write
// must not roll back a successful deploy. Freeze history cannot work that way:
// "production was frozen for six hours and nobody knows who did it" is the exact
// failure this feature exists to prevent. A trigger forces the audit insert to
// fail; the freeze must not survive it.
func TestFreezeEnvironment_AuditFailureRollsBackTheFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "freeze-audit-atomic")

	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION reject_freeze_audit() RETURNS trigger AS $$
		BEGIN
			IF NEW.action = 'environment.freeze' THEN
				RAISE EXCEPTION 'audit unavailable';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_freeze_audit_trg BEFORE INSERT ON audit_events
			FOR EACH ROW EXECUTE FUNCTION reject_freeze_audit();
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS reject_freeze_audit_trg ON audit_events;
			 DROP FUNCTION IF EXISTS reject_freeze_audit();`)
	})

	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err == nil {
		t.Fatal("freeze succeeded despite the audit insert failing")
	}
	// The whole point: no half-state. An environment that reports frozen with no
	// audit trail is worse than one that refused to freeze.
	if _, found, err := s.EnvironmentFrozenState(ctx, projectID, "production"); err != nil {
		t.Fatalf("state: %v", err)
	} else if found {
		t.Fatal("the environment is frozen but its audit row was never written")
	}
}

// Same contract for the thaw: unfreeze is exactly as audit-sensitive as freeze.
func TestUnfreezeEnvironment_AuditFailureKeepsTheFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "unfreeze-audit-atomic")

	if _, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION reject_unfreeze_audit() RETURNS trigger AS $$
		BEGIN
			IF NEW.action = 'environment.unfreeze' THEN
				RAISE EXCEPTION 'audit unavailable';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_unfreeze_audit_trg BEFORE INSERT ON audit_events
			FOR EACH ROW EXECUTE FUNCTION reject_unfreeze_audit();
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DROP TRIGGER IF EXISTS reject_unfreeze_audit_trg ON audit_events;
			 DROP FUNCTION IF EXISTS reject_unfreeze_audit();`)
	})

	if _, err := s.UnfreezeEnvironment(ctx, projectID, "production", testActor()); err == nil {
		t.Fatal("unfreeze succeeded despite the audit insert failing")
	}
	// Fail-safe direction: the environment stays FROZEN. Silently thawing
	// production with no record is the outcome that must not happen.
	if _, found, _ := s.EnvironmentFrozenState(ctx, projectID, "production"); !found {
		t.Fatal("the environment was unfrozen but its audit row was never written")
	}
}

// A rollback racing a freeze is serialised by the same lock, with the same
// ordering contract as dispatch — and neither side hangs.
func TestRollbackAndFreezeConcurrent_NoDeadlock(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	runID, _, _, jobID, _ := seedRunningJob(t, pool)
	projectID := projectIDForRun(t, pool, runID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='success', finished_at=NOW() WHERE id=$1`, jobID); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	envID, err := s.EnsureEnvironment(ctx, projectID, "production")
	if err != nil {
		t.Fatalf("ensure env: %v", err)
	}
	revID, err := s.CreateDeploymentRevision(ctx, store.CreateDeploymentRevisionInput{
		EnvironmentID: envID, RunID: runID, JobRunID: jobID, Version: "1.0.0",
	})
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE deployment_revisions SET status='success', finished_at=NOW() WHERE id=$1`, revID); err != nil {
		t.Fatalf("mark success: %v", err)
	}

	done := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := s.RollbackToRevision(ctx, store.RollbackInput{
			ProjectID: projectID, EnvironmentID: envID, RevisionID: revID, TriggeredBy: "tester",
		})
		done <- err
	}()
	go func() {
		<-start
		_, err := s.FreezeEnvironment(ctx, projectID, "production", testActor(), "close")
		done <- err
	}()
	close(start)

	for i := range 2 {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, store.ErrEnvironmentFrozen) {
				t.Fatalf("goroutine %d: %v", i, err)
			}
		case <-ctx.Done():
			t.Fatal("timed out — rollback and freeze deadlocked on the freeze lock")
		}
	}

	// Once the freeze has committed, every later rollback is refused.
	if _, err := s.RollbackToRevision(ctx, store.RollbackInput{
		ProjectID: projectID, EnvironmentID: envID, RevisionID: revID, TriggeredBy: "tester",
	}); !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("post-freeze rollback = %v, want ErrEnvironmentFrozen", err)
	}
}
