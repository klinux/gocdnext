package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedGatePipeline applies a pipeline with an approval gate ("gate", stage
// "approve") governing the given downstream jobs (stage "deploy"), creates a run,
// and returns it with the gate awaiting. The downstream shape is the variable
// under test: a deploy: job, a bare environment: migration, several envs, or a
// plain job that governs nothing.
func seedGatePipeline(t *testing.T, pool *pgxpool.Pool, slug string, downstream []domain.Job) (runID, projectID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)

	jobs := append([]domain.Job{{
		Name: "gate", Stage: "approve",
		Approval: &domain.ApprovalSpec{Description: "Ship it?"},
	}}, downstream...)

	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "release", Supersede: domain.SupersedeOff, Stages: []string{"approve", "deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: jobs,
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID = res.ProjectID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: res.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: branch, Provider: "github",
		Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return run.RunID, projectID
}

func deployJob(name, env string) domain.Job {
	return domain.Job{
		Name: name, Stage: "deploy", Image: "alpine:3.19",
		Tasks:  []domain.Task{{Script: "true"}},
		Deploy: &domain.DeploySpec{Environment: env, Version: "v1"},
	}
}

// migrationJob carries a bare environment: (no deploy:) — the #206 shape that
// GovernedFreezeEnvs covers but GovernedEnvs does not.
func migrationJob(name, env string) domain.Job {
	return domain.Job{
		Name: name, Stage: "deploy", Image: "alpine:3.19",
		Tasks:       []domain.Task{{Script: "true"}},
		Environment: env,
	}
}

func gateOf(t *testing.T, s *store.Store, runID uuid.UUID) store.JobDetail {
	t.Helper()
	d, err := s.GetRunDetailWithLogs(context.Background(), runID, store.LogWindow{})
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}
	for _, st := range d.Stages {
		for _, j := range st.Jobs {
			if j.Name == "gate" {
				return j
			}
		}
	}
	t.Fatal("no gate job in run detail")
	return store.JobDetail{}
}

func freeze(t *testing.T, s *store.Store, projectID uuid.UUID, env string) {
	t.Helper()
	if _, err := s.FreezeEnvironment(context.Background(), projectID, env, testActor(), "close"); err != nil {
		t.Fatalf("freeze %s: %v", env, err)
	}
}

// The lifecycle: an awaiting gate governing a frozen env reads held; unfreezing
// clears it. This is the exact read-side signal the UI badge renders.
func TestGetRunDetail_FreezeHoldLifecycle(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-life", []domain.Job{deployJob("ship-production", "production")})

	if g := gateOf(t, s, runID); g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("before freeze: held=%v envs=%v, want false/none", g.HeldByFreeze, g.FrozenEnvs)
	}

	freeze(t, s, projectID, "production")
	g := gateOf(t, s, runID)
	if !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("frozen: held=%v envs=%v, want true/[production]", g.HeldByFreeze, g.FrozenEnvs)
	}

	if _, err := s.UnfreezeEnvironment(context.Background(), projectID, "production", testActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if g := gateOf(t, s, runID); g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("after unfreeze: held=%v envs=%v, want false/none", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// #227 review #4: a gate governing a bare environment: migration job (no deploy:)
// must read held — this guards the GovernedFreezeEnvs-over-GovernedEnvs superset.
func TestGetRunDetail_MigrationOnlyEnvHeld(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-migr", []domain.Job{migrationJob("migrate-db", "production")})

	freeze(t, s, projectID, "production")
	g := gateOf(t, s, runID)
	if !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("migration-only gate: held=%v envs=%v, want true/[production]", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// A gate governing several envs, only one frozen → held, with the frozen SUBSET.
func TestGetRunDetail_MultiEnvPartialFreeze(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-multi", []domain.Job{
		deployJob("ship-staging", "staging"),
		deployJob("ship-production", "production"),
	})

	freeze(t, s, projectID, "production") // staging stays open
	g := gateOf(t, s, runID)
	if !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("partial freeze: held=%v envs=%v, want true/[production] (subset, not staging)", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// A plain approval gate that governs no env is never held, even under a freeze.
func TestGetRunDetail_PlainGateGovernsNothing(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-plain", []domain.Job{{
		Name: "work", Stage: "deploy", Image: "alpine:3.19", Tasks: []domain.Task{{Script: "true"}},
	}})

	freeze(t, s, projectID, "production")
	if g := gateOf(t, s, runID); g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("plain gate: held=%v envs=%v, want false/none", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// Only an awaiting gate carries the freeze fields — a decided one never does.
func TestGetRunDetail_DecidedGateNoFreezeFields(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-decided", []domain.Job{deployJob("ship-production", "production")})

	if _, err := pool.Exec(context.Background(),
		`UPDATE job_runs SET status='success', decision='approved', decided_by='someone'
		 WHERE run_id=$1 AND name='gate'`, runID); err != nil {
		t.Fatalf("decide gate: %v", err)
	}
	freeze(t, s, projectID, "production")
	if g := gateOf(t, s, runID); g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("decided gate: held=%v envs=%v, want false/none (not awaiting)", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// A pre-00067 historical run has a '{}' snapshot → no governed envs → no badge,
// and never a crash (review #1 caveat).
func TestGetRunDetail_EmptySnapshotGraceful(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-empty", []domain.Job{deployJob("ship-production", "production")})

	if _, err := pool.Exec(context.Background(), `UPDATE runs SET definition='{}'::jsonb WHERE id=$1`, runID); err != nil {
		t.Fatalf("blank snapshot: %v", err)
	}
	freeze(t, s, projectID, "production")
	if g := gateOf(t, s, runID); g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("empty snapshot: held=%v envs=%v, want false/none", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// A non-'{}' but semantically INVALID snapshot (a JSON string, not a pipeline
// object) → decode fails → fail-safe (no badge, no crash) AND the decode-error
// counter increments so a persistent corruption isn't completely silent (review).
func TestGetRunDetail_InvalidSnapshotGracefulAndCounted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-invalid", []domain.Job{deployJob("ship-production", "production")})

	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET definition='"garbage"'::jsonb WHERE id=$1`, runID); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	freeze(t, s, projectID, "production")

	// Delta, not Reset() — the registry is process-wide and shared with sibling
	// tests running in parallel.
	before := testutil.ToFloat64(metrics.GateFreezeAnnotationErrors.WithLabelValues("run_detail", "decode"))
	g := gateOf(t, s, runID)
	after := testutil.ToFloat64(metrics.GateFreezeAnnotationErrors.WithLabelValues("run_detail", "decode"))

	if g.HeldByFreeze || len(g.FrozenEnvs) != 0 {
		t.Fatalf("invalid snapshot: held=%v envs=%v, want false/none (fail-safe)", g.HeldByFreeze, g.FrozenEnvs)
	}
	if after <= before {
		t.Fatalf("decode-error counter did not increment: before=%v after=%v", before, after)
	}
}

// ---- project-detail strip (#227 part 2) ------------------------------------

// seedGatedProject applies ONE project with several gated pipelines (each: a
// "gate" + the given downstream jobs) and creates a run per pipeline, each sitting
// at its stage-0 gate. All gates share the name "gate" on purpose — the freeze
// correlation must key on run_id, not name.
func seedGatedProject(t *testing.T, pool *pgxpool.Pool, slug string, pipes map[string][]domain.Job) (projectID uuid.UUID, runIDs map[string]uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	var pipelines []*domain.Pipeline
	fp := map[string]string{}
	for name, downstream := range pipes {
		url := "https://github.com/org/" + slug + "-" + name
		fp[name] = store.FingerprintFor(url, "main")
		jobs := append([]domain.Job{{
			Name: "gate", Stage: "approve", Approval: &domain.ApprovalSpec{Description: "Ship?"},
		}}, downstream...)
		pipelines = append(pipelines, &domain.Pipeline{
			Name: name, Supersede: domain.SupersedeOff, Stages: []string{"approve", "deploy"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp[name], AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: jobs,
		})
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: slug, Name: slug, Pipelines: pipelines})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID = res.ProjectID
	runIDs = make(map[string]uuid.UUID, len(res.Pipelines))
	for _, p := range res.Pipelines {
		var materialID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp[p.Name]).Scan(&materialID); err != nil {
			t.Fatalf("material %s: %v", p.Name, err)
		}
		run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
			PipelineID: p.PipelineID, MaterialID: materialID, ModificationID: 1,
			Revision: "deadbeef", Branch: "main", Provider: "github", Delivery: slug, TriggeredBy: "system:webhook",
		})
		if err != nil {
			t.Fatalf("run %s: %v", p.Name, err)
		}
		runIDs[p.Name] = run.RunID
	}
	return projectID, runIDs
}

func projectGate(t *testing.T, s *store.Store, slug, pipelineName string) store.JobRunSummaryLite {
	t.Helper()
	d, err := s.GetProjectDetail(context.Background(), slug, 20)
	if err != nil {
		t.Fatalf("project detail: %v", err)
	}
	for _, p := range d.Pipelines {
		if p.Name != pipelineName {
			continue
		}
		for _, st := range p.LatestRunStages {
			for _, j := range st.Jobs {
				if j.Name == "gate" {
					return j
				}
			}
		}
	}
	t.Fatalf("no gate on the strip for pipeline %q", pipelineName)
	return store.JobRunSummaryLite{}
}

// The project strip shows held on the awaiting gate when its governed env is
// frozen, and clears on unfreeze — the signal the flow/list node renders.
func TestGetProjectDetail_GateHeldLifecycle(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, _ := seedGatedProject(t, pool, "pd-held", map[string][]domain.Job{
		"release": {deployJob("ship-production", "production")},
	})

	if g := projectGate(t, s, "pd-held", "release"); g.HeldByFreeze {
		t.Fatalf("before freeze: held=%v, want false", g.HeldByFreeze)
	}
	freeze(t, s, projectID, "production")
	if g := projectGate(t, s, "pd-held", "release"); !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("frozen: held=%v envs=%v, want true/[production]", g.HeldByFreeze, g.FrozenEnvs)
	}
	if _, err := s.UnfreezeEnvironment(context.Background(), projectID, "production", testActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if g := projectGate(t, s, "pd-held", "release"); g.HeldByFreeze {
		t.Fatalf("after unfreeze: held=%v, want false", g.HeldByFreeze)
	}
}

// Migration-only gate (bare environment:, no deploy:) on the strip.
func TestGetProjectDetail_MigrationOnlyHeld(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, _ := seedGatedProject(t, pool, "pd-migr", map[string][]domain.Job{
		"release": {migrationJob("migrate-db", "production")},
	})
	freeze(t, s, projectID, "production")
	if g := projectGate(t, s, "pd-migr", "release"); !g.HeldByFreeze || len(g.FrozenEnvs) != 1 {
		t.Fatalf("migration-only strip gate: held=%v envs=%v, want true/[production]", g.HeldByFreeze, g.FrozenEnvs)
	}
}

// A '{}' snapshot (excluded by ListRunSnapshotsForFreeze) → no badge, no crash.
func TestGetProjectDetail_EmptySnapshotGraceful(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, runIDs := seedGatedProject(t, pool, "pd-empty", map[string][]domain.Job{
		"release": {deployJob("ship-production", "production")},
	})
	if _, err := pool.Exec(context.Background(), `UPDATE runs SET definition='{}'::jsonb WHERE id=$1`, runIDs["release"]); err != nil {
		t.Fatalf("blank snapshot: %v", err)
	}
	freeze(t, s, projectID, "production")
	if g := projectGate(t, s, "pd-empty", "release"); g.HeldByFreeze {
		t.Fatalf("empty snapshot: held=%v, want false", g.HeldByFreeze)
	}
}

// Correctness anchor on the strip: freeze derives from the SNAPSHOT, not the live
// def. Drift the live def to '{}'; the snapshot still governs → still held.
func TestGetProjectDetail_SnapshotBeatsLiveDrift(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, runIDs := seedGatedProject(t, pool, "pd-drift", map[string][]domain.Job{
		"release": {deployJob("ship-production", "production")},
	})
	if _, err := pool.Exec(context.Background(),
		`UPDATE pipelines pl SET definition='{}'::jsonb FROM runs r WHERE r.id=$1 AND pl.id=r.pipeline_id`, runIDs["release"]); err != nil {
		t.Fatalf("drift live def: %v", err)
	}
	freeze(t, s, projectID, "production")
	if g := projectGate(t, s, "pd-drift", "release"); !g.HeldByFreeze {
		t.Fatalf("snapshot vs live: held=%v, want true (snapshot wins)", g.HeldByFreeze)
	}
}

// Two pipelines in ONE project, BOTH gates named "gate": only the one whose run's
// snapshot governs the frozen env is held. Proves correlation is by run_id, not
// gate name.
func TestGetProjectDetail_MultiPipelineSameGateName(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, _ := seedGatedProject(t, pool, "pd-multi", map[string][]domain.Job{
		"alpha": {deployJob("ship-a", "production")},
		"beta":  {deployJob("ship-b", "staging")},
	})
	freeze(t, s, projectID, "production") // only alpha's env

	if g := projectGate(t, s, "pd-multi", "alpha"); !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("alpha: held=%v envs=%v, want true/[production]", g.HeldByFreeze, g.FrozenEnvs)
	}
	if g := projectGate(t, s, "pd-multi", "beta"); g.HeldByFreeze {
		t.Fatalf("beta (staging, unfrozen): held=%v, want false (per-run correlation)", g.HeldByFreeze)
	}
}

// One run's snapshot is corrupt; it degrades ALONE (+ counts) while the sibling's
// held badge is unaffected.
func TestGetProjectDetail_InvalidSnapshotIsolated(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	projectID, runIDs := seedGatedProject(t, pool, "pd-iso", map[string][]domain.Job{
		"good": {deployJob("ship-g", "production")},
		"bad":  {deployJob("ship-b", "production")},
	})
	if _, err := pool.Exec(context.Background(),
		`UPDATE runs SET definition='"garbage"'::jsonb WHERE id=$1`, runIDs["bad"]); err != nil {
		t.Fatalf("corrupt bad snapshot: %v", err)
	}
	freeze(t, s, projectID, "production")

	before := testutil.ToFloat64(metrics.GateFreezeAnnotationErrors.WithLabelValues("project_detail", "decode"))
	good := projectGate(t, s, "pd-iso", "good")
	bad := projectGate(t, s, "pd-iso", "bad")
	after := testutil.ToFloat64(metrics.GateFreezeAnnotationErrors.WithLabelValues("project_detail", "decode"))

	if !good.HeldByFreeze {
		t.Fatalf("good: held=%v, want true (unaffected by sibling's bad snapshot)", good.HeldByFreeze)
	}
	if bad.HeldByFreeze {
		t.Fatalf("bad: held=%v, want false (degraded alone)", bad.HeldByFreeze)
	}
	if after <= before {
		t.Fatalf("project_detail/decode counter did not increment: %v→%v", before, after)
	}
}

// The correctness anchor: freeze state is derived from the run's SNAPSHOT
// (runs.definition), never the live pipeline. Drift the live def to empty; the
// snapshot still governs production, so the badge must still show held.
func TestGetRunDetail_UsesSnapshotNotLiveDefinition(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	runID, projectID := seedGatePipeline(t, pool, "freeze-drift", []domain.Job{deployJob("ship-production", "production")})

	// Live pipeline def wiped (post-apply drift). If the read used pl.definition
	// it would now govern nothing → false. It must use the run snapshot.
	if _, err := pool.Exec(context.Background(),
		`UPDATE pipelines pl SET definition='{}'::jsonb FROM runs r WHERE r.id=$1 AND pl.id=r.pipeline_id`, runID); err != nil {
		t.Fatalf("drift live def: %v", err)
	}
	freeze(t, s, projectID, "production")
	g := gateOf(t, s, runID)
	if !g.HeldByFreeze || len(g.FrozenEnvs) != 1 || g.FrozenEnvs[0] != "production" {
		t.Fatalf("snapshot vs live: held=%v envs=%v, want true/[production] (snapshot wins)", g.HeldByFreeze, g.FrozenEnvs)
	}
}
