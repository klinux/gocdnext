package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func projectIDOfPipeline(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pipelineID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT project_id FROM pipelines WHERE id=$1`, pipelineID).Scan(&id); err != nil {
		t.Fatalf("project of pipeline: %v", err)
	}
	return id
}

// seedBuildRun inserts a run with an exact duration (start→finish) `daysAgo`
// days ago. When withArtifact, it also lays the stage_run → job_run → ready
// artifact chain so the run qualifies as a "build". Returns the run id.
func seedBuildRun(t *testing.T, pool *pgxpool.Pool, ctx context.Context, projectID, pipelineID uuid.UUID, counter int, cause, status string, durSecs, daysAgo int, withArtifact bool) uuid.UUID {
	t.Helper()
	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO runs (pipeline_id, counter, cause, status, revisions, created_at, started_at, finished_at)
		VALUES ($1,$2,$3,$4,'{}'::jsonb,
		        now() - make_interval(days => $5, secs => $6 + 30),
		        now() - make_interval(days => $5, secs => $6),
		        now() - make_interval(days => $5))
		RETURNING id`, pipelineID, counter, cause, status, daysAgo, durSecs).Scan(&runID); err != nil {
		t.Fatalf("seed build run: %v", err)
	}
	if withArtifact {
		var stageID, jobID uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO stage_runs (run_id,name,ordinal,status) VALUES ($1,'build',0,'success') RETURNING id`, runID).Scan(&stageID); err != nil {
			t.Fatalf("seed stage: %v", err)
		}
		if err := pool.QueryRow(ctx, `INSERT INTO job_runs (run_id,stage_run_id,name,status) VALUES ($1,$2,'compile','success') RETURNING id`, runID, stageID).Scan(&jobID); err != nil {
			t.Fatalf("seed job: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO artifacts (run_id,job_run_id,pipeline_id,project_id,path,storage_key,status)
			VALUES ($1,$2,$3,$4,'app.tar.gz',$5,'ready')`,
			runID, jobID, pipelineID, projectID, "key-"+runID.String()); err != nil {
			t.Fatalf("seed artifact: %v", err)
		}
		// Mirror MarkArtifactReady: set the durable produced_artifact flag (no DB
		// trigger does this). The build query reads the flag, not the artifact row.
		if _, err := pool.Exec(ctx, `UPDATE runs SET produced_artifact = true WHERE id=$1`, runID); err != nil {
			t.Fatalf("seed produced flag: %v", err)
		}
	}
	return runID
}

// markDeployed attaches a deployment_revision to a run, making it a deploy run
// (which must then be excluded from build-time percentiles).
func markDeployed(t *testing.T, pool *pgxpool.Pool, ctx context.Context, projectID, runID uuid.UUID) {
	t.Helper()
	var envID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (project_id,name) VALUES ($1,'prod')
		ON CONFLICT (project_id,name) DO UPDATE SET name=EXCLUDED.name RETURNING id`, projectID).Scan(&envID); err != nil {
		t.Fatalf("seed env: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_revisions (environment_id, run_id, version, status, finished_at)
		VALUES ($1,$2,'v1','success', now())`, envID, runID); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
}

func TestBuildTimesReport(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pay := applyLabeledProject(t, s, ctx, "pay", "team", "payments", "build")
	payProj := projectIDOfPipeline(t, pool, ctx, pay["build"])

	// 10 qualifying build runs with exact, distinct durations → deterministic
	// percentiles: p50=330, p85=519, p95=573 (percentile_cont interpolation).
	durs := []int{60, 120, 180, 240, 300, 360, 420, 480, 540, 600}
	for i, d := range durs {
		seedBuildRun(t, pool, ctx, payProj, pay["build"], i+1, "webhook", "success", d, 1, true)
	}

	// --- runs that must be EXCLUDED ---
	seedBuildRun(t, pool, ctx, payProj, pay["build"], 101, "pull_request", "success", 9000, 1, true) // PR
	seedBuildRun(t, pool, ctx, payProj, pay["build"], 102, "merge_group", "success", 9000, 1, true)  // merge-queue probe
	seedBuildRun(t, pool, ctx, payProj, pay["build"], 103, "webhook", "failed", 9000, 1, true)       // not success
	seedBuildRun(t, pool, ctx, payProj, pay["build"], 104, "webhook", "success", 9000, 1, false)     // no artifact
	seedBuildRun(t, pool, ctx, payProj, pay["build"], 105, "webhook", "success", 9000, 40, true)     // outside window
	dep := seedBuildRun(t, pool, ctx, payProj, pay["build"], 106, "webhook", "success", 9000, 1, true)
	markDeployed(t, pool, ctx, payProj, dep) // deploy run, not a build

	// a second group, so the breakdown has >1 row
	shop := applyLabeledProject(t, s, ctx, "shop", "team", "storefront", "build")
	shopProj := projectIDOfPipeline(t, pool, ctx, shop["build"])
	for i, d := range []int{100, 200, 300} { // p50 = 200
		seedBuildRun(t, pool, ctx, shopProj, shop["build"], i+1, "tag", "success", d, 2, true)
	}

	rep, err := s.BuildTimesReport(ctx, "team", 30)
	if err != nil {
		t.Fatalf("build times: %v", err)
	}

	// --- overall: only the 10 qualifying payments builds + 3 storefront = 13 ---
	if rep.Overall.Builds != 13 {
		t.Fatalf("overall builds = %d, want 13 (PR/merge/failed/no-artifact/old/deploy excluded)", rep.Overall.Builds)
	}

	byGroup := map[string]store.BuildTimeGroup{}
	for _, g := range rep.Groups {
		byGroup[g.Group] = g
	}
	if len(byGroup) != 2 {
		t.Fatalf("groups = %+v", rep.Groups)
	}

	pg := byGroup["payments"]
	if pg.Builds != 10 {
		t.Errorf("payments builds = %d, want 10", pg.Builds)
	}
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"p50", pg.P50Sec, 330}, {"p85", pg.P85Sec, 519}, {"p95", pg.P95Sec, 573},
	} {
		if math.Abs(c.got-c.want) > 1 {
			t.Errorf("payments %s = %v, want ~%v", c.name, c.got, c.want)
		}
	}

	sg := byGroup["storefront"]
	if sg.Builds != 3 || math.Abs(sg.P50Sec-200) > 1 {
		t.Errorf("storefront = %+v, want builds=3 p50~200", sg)
	}

	// Durability: artifact retention deletes artifact rows, but the produced
	// flag persists — so the same builds must still count after the sweep.
	if _, err := pool.Exec(ctx, `DELETE FROM artifacts`); err != nil {
		t.Fatalf("simulate retention sweep: %v", err)
	}
	after, err := s.BuildTimesReport(ctx, "team", 30)
	if err != nil {
		t.Fatalf("build times after sweep: %v", err)
	}
	if after.Overall.Builds != rep.Overall.Builds {
		t.Errorf("after artifact sweep builds = %d, want %d (flag must be durable)",
			after.Overall.Builds, rep.Overall.Builds)
	}
}

func TestLabelKeysWithData(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// "team" project has a run in window → key shows.
	live := applyLabeledProject(t, s, ctx, "live", "team", "payments", "build")
	seedRuns(t, pool, ctx, live["build"], 1, 2, "success", 1)

	// "app" project has a label but NO runs → key must be hidden.
	applyLabeledProject(t, s, ctx, "idle", "app", "billing", "build")

	// "cxl" project has runs, but only canceled ones → still hidden, since the
	// analytics panels ignore canceled runs (an all-zero dashboard otherwise).
	cxl := applyLabeledProject(t, s, ctx, "cxl", "env", "canceled-only", "build")
	seedRuns(t, pool, ctx, cxl["build"], 1, 3, "canceled", 1)

	keys, err := s.LabelKeysWithData(ctx, 30)
	if err != nil {
		t.Fatalf("label keys with data: %v", err)
	}
	has := map[string]bool{}
	for _, k := range keys {
		has[k] = true
	}
	if !has["team"] {
		t.Errorf("expected 'team' (has runs) in %v", keys)
	}
	if has["app"] {
		t.Errorf("did not expect 'app' (no runs) in %v", keys)
	}
	if has["env"] {
		t.Errorf("did not expect 'env' (only canceled runs) in %v", keys)
	}

	// Unfiltered keys still include every label (the picker's fallback source).
	all, err := s.LabelKeys(ctx)
	if err != nil {
		t.Fatalf("label keys: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered keys = %v, want [app env team]", all)
	}
}
