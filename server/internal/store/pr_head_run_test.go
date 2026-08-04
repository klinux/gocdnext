package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedPRHead applies the "build" pipeline (slug "demo") and returns the store,
// its project id and the material id — the anchor CreatePRHeadRun derives from.
// The toggle is left OFF; tests enable it explicitly.
func seedPRHead(t *testing.T) (*store.Store, context.Context, uuid.UUID, uuid.UUID, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	s := store.New(pool)
	ctx := context.Background()
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT project_id FROM pipelines WHERE id=$1`, pipelineID).Scan(&projectID); err != nil {
		t.Fatalf("project id: %v", err)
	}
	return s, ctx, projectID, materialID, pool
}

func enablePRHead(t *testing.T, s *store.Store, ctx context.Context) {
	t.Helper()
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "demo", true); err != nil {
		t.Fatalf("enable toggle: %v", err)
	}
}

// prHeadDef is a minimal, valid head definition whose name matches the seeded
// "build" pipeline.
func prHeadDef() domain.Pipeline {
	return domain.Pipeline{
		Name:   "build",
		Stages: []string{"build"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}},
		},
	}
}

func prHeadInput(projectID, materialID uuid.UUID, def domain.Pipeline) store.CreatePRHeadRunInput {
	return store.CreatePRHeadRunInput{
		MaterialID: materialID,
		ProjectID:  projectID,
		RawDef:     def,
		Revision:   "headsha01",
		Branch:     "feature/x",
		Provider:   "github",
		Delivery:   "delivery-1",
	}
}

func runJobNames(t *testing.T, pool *pgxpool.Pool, ctx context.Context, runID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT jr.name FROM job_runs jr JOIN stage_runs sr ON sr.id = jr.stage_run_id WHERE sr.run_id = $1`, runID)
	if err != nil {
		t.Fatalf("job names query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan job name: %v", err)
		}
		out = append(out, n)
	}
	return out
}

func runDefinitionText(t *testing.T, pool *pgxpool.Pool, ctx context.Context, runID uuid.UUID) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(ctx, `SELECT definition::text FROM runs WHERE id = $1`, runID).Scan(&def); err != nil {
		t.Fatalf("run definition query: %v", err)
	}
	return def
}

func countRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

func assertNoWrites(t *testing.T, pool *pgxpool.Pool, ctx context.Context, materialID uuid.UUID) {
	t.Helper()
	if n := countRows(t, pool, ctx, `SELECT count(*) FROM modifications WHERE material_id = $1`, materialID); n != 0 {
		t.Fatalf("modification count = %d, want 0 (no residual write)", n)
	}
	if n := countRows(t, pool, ctx, `SELECT count(*) FROM runs r JOIN pipelines pl ON pl.id = r.pipeline_id WHERE pl.name = 'build'`); n != 0 {
		t.Fatalf("run count = %d, want 0 (no residual write)", n)
	}
}

// Happy path + governance: with the toggle on and a global inject policy, the
// created run reflects the EFFECTIVE (post-ApplyPolicies) definition — the
// injected _compliance_scan job appears even though the stored pipeline def
// (seeded before the policy) doesn't have it. Proves policies run in-tx.
func TestCreatePRHeadRun_AppliesPolicyInTx(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t)) // ApplyProject with an scm seals the webhook secret
	ctx := context.Background()

	// A governed project needs a registered SCM source, so seed one here.
	url, branch := "https://github.com/acme/demo", "main"
	fp := store.FingerprintFor(url, branch)
	p := &domain.Pipeline{
		Name: "build", Stages: []string{"build"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "demo", Pipelines: []*domain.Pipeline{p},
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: branch},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	enablePRHead(t, s, ctx)
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material id: %v", err)
	}
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global-scan", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}

	run, created, err := s.CreatePRHeadRun(ctx, prHeadInput(res.ProjectID, materialID, prHeadDef()))
	if err != nil || !created {
		t.Fatalf("CreatePRHeadRun = (created=%v, err=%v), want created run", created, err)
	}
	names := runJobNames(t, pool, ctx, run.RunID)
	var hasCompile, hasScan bool
	for _, n := range names {
		hasCompile = hasCompile || n == "compile"
		hasScan = hasScan || n == "_compliance_scan"
	}
	if !hasCompile || !hasScan {
		t.Fatalf("run jobs = %v, want both compile and the policy-injected _compliance_scan", names)
	}
}

func TestCreatePRHeadRun_DisabledToggle(t *testing.T) {
	s, ctx, projectID, materialID, _ := seedPRHead(t) // toggle left OFF
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, prHeadDef())); !errors.Is(err, store.ErrPRHeadConfigDisabled) {
		t.Fatalf("err = %v, want ErrPRHeadConfigDisabled", err)
	}
}

func TestCreatePRHeadRun_SystemManaged(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, err := pool.Exec(ctx, `UPDATE pipelines SET system_managed = true WHERE name = 'build'`); err != nil {
		t.Fatalf("mark system_managed: %v", err)
	}
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, prHeadDef())); !errors.Is(err, store.ErrPRHeadSystemManaged) {
		t.Fatalf("err = %v, want ErrPRHeadSystemManaged", err)
	}
}

func TestCreatePRHeadRun_NameMismatch(t *testing.T) {
	s, ctx, projectID, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Name = "not-build"
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); !errors.Is(err, store.ErrPRHeadNameMismatch) {
		t.Fatalf("err = %v, want ErrPRHeadNameMismatch", err)
	}
}

func TestCreatePRHeadRun_ReservedName(t *testing.T) {
	s, ctx, projectID, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Stages = append(def.Stages, "_compliance_evil")
	def.Jobs = append(def.Jobs, domain.Job{
		Name: "_compliance_evil", Stage: "_compliance_evil", Tasks: []domain.Task{{Script: "steal"}},
	})
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); !errors.Is(err, store.ErrPRHeadReservedName) {
		t.Fatalf("err = %v, want ErrPRHeadReservedName", err)
	}
}

func TestCreatePRHeadRun_MatrixCap(t *testing.T) {
	s, ctx, projectID, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Jobs[0].Matrix = map[string][]string{ // 6^4 = 1296 > 1000
		"A": {"1", "2", "3", "4", "5", "6"},
		"B": {"1", "2", "3", "4", "5", "6"},
		"C": {"1", "2", "3", "4", "5", "6"},
		"D": {"1", "2", "3", "4", "5", "6"},
	}
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); !errors.Is(err, store.ErrPRHeadTooManyJobs) {
		t.Fatalf("err = %v, want ErrPRHeadTooManyJobs", err)
	}
}

func TestCreatePRHeadRun_ProjectMismatch(t *testing.T) {
	s, ctx, _, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	// Pass a project that is NOT the material's owning project.
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(uuid.New(), materialID, prHeadDef())); !errors.Is(err, store.ErrPRHeadProjectMismatch) {
		t.Fatalf("err = %v, want ErrPRHeadProjectMismatch", err)
	}
}

func TestCreatePRHeadRun_MaterialNotFound(t *testing.T) {
	s, ctx, projectID, _, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, uuid.New(), prHeadDef())); !errors.Is(err, store.ErrPRHeadMaterialNotFound) {
		t.Fatalf("err = %v, want ErrPRHeadMaterialNotFound", err)
	}
}

func TestCreatePRHeadRun_MissingRequiredInput(t *testing.T) {
	s, ctx, projectID, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	base := prHeadInput(projectID, materialID, prHeadDef())
	for name, mut := range map[string]func(*store.CreatePRHeadRunInput){
		"no revision": func(in *store.CreatePRHeadRunInput) { in.Revision = "" },
		"no branch":   func(in *store.CreatePRHeadRunInput) { in.Branch = "" },
		"no provider": func(in *store.CreatePRHeadRunInput) { in.Provider = "" },
		"no delivery": func(in *store.CreatePRHeadRunInput) { in.Delivery = "" },
		"no project":  func(in *store.CreatePRHeadRunInput) { in.ProjectID = uuid.Nil },
	} {
		in := base
		mut(&in)
		if _, _, err := s.CreatePRHeadRun(ctx, in); !errors.Is(err, store.ErrPRHeadInvalidInput) {
			t.Fatalf("%s: err = %v, want ErrPRHeadInvalidInput", name, err)
		}
	}
}

// A "null" (or malformed) cause_detail must error, not panic on a nil map.
func TestCreatePRHeadRun_NullCauseDetail(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	in := prHeadInput(projectID, materialID, prHeadDef())
	in.CauseDetail = json.RawMessage("null")
	if _, _, err := s.CreatePRHeadRun(ctx, in); !errors.Is(err, store.ErrPRHeadInvalidInput) {
		t.Fatalf("err = %v, want ErrPRHeadInvalidInput", err)
	}
	assertNoWrites(t, pool, ctx, materialID)
}

// Provenance: the store-set keys win over anything the resolver's CauseDetail
// tries to put there; the cause is always pull_request; the lane is pr:<number>.
func TestCreatePRHeadRun_ProvenanceStoreSetWins(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	in := prHeadInput(projectID, materialID, prHeadDef())
	in.CauseDetail = json.RawMessage(`{
		"pr_number": 42, "pr_title": "legit",
		"provider": "EVIL", "delivery": "EVIL", "material_id": "EVIL",
		"config_source": "EVIL", "config_revision": "EVIL", "config_digest": "EVIL"
	}`)
	run, created, err := s.CreatePRHeadRun(ctx, in)
	if err != nil || !created {
		t.Fatalf("CreatePRHeadRun = (created=%v, err=%v)", created, err)
	}

	var cause, detailText, ref string
	if err := pool.QueryRow(ctx, `SELECT cause, cause_detail::text, ref FROM runs WHERE id = $1`, run.RunID).
		Scan(&cause, &detailText, &ref); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if cause != "pull_request" {
		t.Errorf("cause = %q, want pull_request", cause)
	}
	if ref != "pr:42" {
		t.Errorf("ref = %q, want pr:42 (lane from pr_number)", ref)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(detailText), &d); err != nil {
		t.Fatalf("cause_detail json: %v", err)
	}
	if d["provider"] != "github" || d["delivery"] != "delivery-1" {
		t.Errorf("provider/delivery = %v/%v, want github/delivery-1 (store-set wins)", d["provider"], d["delivery"])
	}
	if d["config_source"] != "pr_head" || d["config_revision"] != "headsha01" {
		t.Errorf("config_source/revision = %v/%v, want pr_head/headsha01", d["config_source"], d["config_revision"])
	}
	if d["material_id"] != materialID.String() {
		t.Errorf("material_id = %v, want %v (store-set)", d["material_id"], materialID)
	}
	if dig, _ := d["config_digest"].(string); dig == "EVIL" || dig == "" {
		t.Errorf("config_digest = %v, want a real store-set digest", d["config_digest"])
	}
	if d["pr_title"] != "legit" {
		t.Errorf("pr_title = %v, want legit (non-store-set key preserved)", d["pr_title"])
	}
}

func TestCreatePRHeadRun_DedupReplay(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, created, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, prHeadDef())); err != nil || !created {
		t.Fatalf("first = (created=%v, err=%v), want created", created, err)
	}
	if _, created, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, prHeadDef())); err != nil || created {
		t.Fatalf("replay = (created=%v, err=%v), want created=false, nil err", created, err)
	}
	if n := countRows(t, pool, ctx,
		`SELECT count(*) FROM runs r JOIN pipelines pl ON pl.id = r.pipeline_id WHERE pl.name = 'build'`); n != 1 {
		t.Fatalf("run count = %d, want 1 (no second run on replay)", n)
	}
}

// Atomicity (#2): a failure AFTER the modification insert (a job wired to a
// non-existent stage, caught by insertRunRowsTx) rolls the modification back too.
func TestCreatePRHeadRun_AtomicRollbackOnFailure(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Jobs = append(def.Jobs, domain.Job{
		Name: "orphan", Stage: "ghost", Tasks: []domain.Task{{Script: "noop"}}, // stage not in Stages
	})
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); err == nil {
		t.Fatal("expected an error for a job referencing an unknown stage")
	}
	assertNoWrites(t, pool, ctx, materialID)
}

// #1: a runner profile cap is enforced on the head definition in-tx, so a
// PR-head can't exceed the admin's max_cpu.
func TestCreatePRHeadRun_ProfileOverCap(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{Name: "tight", Engine: "kubernetes", MaxCPU: "500m"}); err != nil {
		t.Fatalf("profile: %v", err)
	}
	def := prHeadDef()
	def.Jobs[0].Profile = "tight"
	def.Jobs[0].Resources = domain.ResourceSpec{Requests: domain.ResourceQuantities{CPU: "2"}} // 2 > 500m
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); !errors.Is(err, store.ErrPRHeadProfile) {
		t.Fatalf("err = %v, want ErrPRHeadProfile", err)
	}
	assertNoWrites(t, pool, ctx, materialID)
}

// #1: profile resource defaults are applied to the head definition and land in
// the run's snapshot (which dispatch reads for resources).
func TestCreatePRHeadRun_ProfileDefaultsApplied(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{Name: "std", Engine: "kubernetes", DefaultCPURequest: "123m"}); err != nil {
		t.Fatalf("profile: %v", err)
	}
	def := prHeadDef()
	def.Jobs[0].Profile = "std" // no resources declared → default filled
	run, created, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def))
	if err != nil || !created {
		t.Fatalf("CreatePRHeadRun = (created=%v, err=%v)", created, err)
	}
	if snap := runDefinitionText(t, pool, ctx, run.RunID); !strings.Contains(snap, "123m") {
		t.Fatalf("run snapshot missing the profile default request: %s", snap)
	}
}

// #1: an unknown cluster reference on the head definition is rejected in-tx.
func TestCreatePRHeadRun_UnknownCluster(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Jobs[0].Cluster = "ghost"
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, def)); !errors.Is(err, store.ErrPRHeadCluster) {
		t.Fatalf("err = %v, want ErrPRHeadCluster", err)
	}
	assertNoWrites(t, pool, ctx, materialID)
}

// #5: a concurrent disable of the toggle is LINEARISED against run creation by
// the project row FOR SHARE lock. With a deterministic barrier (wait until
// CreatePRHeadRun is observably blocked on a lock), the disable that commits
// while it waits is seen — the run is refused and nothing is written.
func TestCreatePRHeadRun_ConcurrentDisableLinearized(t *testing.T) {
	s, ctx, projectID, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)

	// T1 on a separate conn: hold an uncommitted UPDATE disabling the toggle,
	// taking a row-exclusive lock on the project row.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx1, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin t1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if _, err := tx1.Exec(ctx, `UPDATE projects SET trust_same_repo_pr_config = false WHERE id = $1`, projectID); err != nil {
		t.Fatalf("t1 update: %v", err)
	}
	var t1pid int
	if err := tx1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&t1pid); err != nil {
		t.Fatalf("t1 pid: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, e := s.CreatePRHeadRun(ctx, prHeadInput(projectID, materialID, prHeadDef()))
		done <- e
	}()

	// Deterministic barrier: wait until a session is blocked SPECIFICALLY by T1
	// (t1pid appears in its pg_blocking_pids), so an unrelated lock can't release
	// the test early — this proves CreatePRHeadRun is waiting on T1's row lock.
	blocked := false
	for i := 0; i < 300; i++ {
		if countRows(t, pool, ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, t1pid) > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("CreatePRHeadRun did not block on T1's project-row lock")
	}

	// Commit the disable; the blocked FOR SHARE now reads trust=false.
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}

	if err := <-done; !errors.Is(err, store.ErrPRHeadConfigDisabled) {
		t.Fatalf("CreatePRHeadRun err = %v, want ErrPRHeadConfigDisabled (disable linearised)", err)
	}
	assertNoWrites(t, pool, ctx, materialID)
}
