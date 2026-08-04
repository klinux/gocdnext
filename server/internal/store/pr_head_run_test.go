package store_test

import (
	"context"
	"errors"
	"testing"

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

func prHeadInput(materialID uuid.UUID, def domain.Pipeline) store.CreatePRHeadRunInput {
	return store.CreatePRHeadRunInput{
		MaterialID:     materialID,
		RawDef:         def,
		Revision:       "headsha01",
		Branch:         "feature/x",
		Cause:          "pull_request",
		ConfigRevision: "headsha01",
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

func countRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// Happy path + governance: with the toggle on and a global inject policy, the
// created run reflects the EFFECTIVE (post-ApplyPolicies) definition — the
// injected _compliance_scan job appears even though the stored pipeline def
// (seeded before the policy) doesn't have it. Proves policies run in-tx on the
// RAW head definition.
func TestCreatePRHeadRun_AppliesPolicyInTx(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t)) // ApplyProject with an scm seals the webhook secret
	ctx := context.Background()

	// A governed project needs a registered SCM source, so seed one here (unlike
	// the plain seedPipeline path).
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
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "demo", Pipelines: []*domain.Pipeline{p},
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: branch},
	}); err != nil {
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

	res, created, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, prHeadDef()))
	if err != nil || !created {
		t.Fatalf("CreatePRHeadRun = (created=%v, err=%v), want created run", created, err)
	}
	names := runJobNames(t, pool, ctx, res.RunID)
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
	s, ctx, _, materialID, _ := seedPRHead(t) // toggle left OFF
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, prHeadDef())); !errors.Is(err, store.ErrPRHeadConfigDisabled) {
		t.Fatalf("err = %v, want ErrPRHeadConfigDisabled", err)
	}
}

func TestCreatePRHeadRun_SystemManaged(t *testing.T) {
	s, ctx, _, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, err := pool.Exec(ctx, `UPDATE pipelines SET system_managed = true WHERE name = 'build'`); err != nil {
		t.Fatalf("mark system_managed: %v", err)
	}
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, prHeadDef())); !errors.Is(err, store.ErrPRHeadSystemManaged) {
		t.Fatalf("err = %v, want ErrPRHeadSystemManaged", err)
	}
}

func TestCreatePRHeadRun_NameMismatch(t *testing.T) {
	s, ctx, _, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Name = "not-build"
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, def)); !errors.Is(err, store.ErrPRHeadNameMismatch) {
		t.Fatalf("err = %v, want ErrPRHeadNameMismatch", err)
	}
}

func TestCreatePRHeadRun_ReservedName(t *testing.T) {
	s, ctx, _, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	// A head config can't mint a job in the reserved governance namespace.
	def.Stages = append(def.Stages, "_compliance_evil")
	def.Jobs = append(def.Jobs, domain.Job{
		Name: "_compliance_evil", Stage: "_compliance_evil", Tasks: []domain.Task{{Script: "steal"}},
	})
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, def)); !errors.Is(err, store.ErrPRHeadReservedName) {
		t.Fatalf("err = %v, want ErrPRHeadReservedName", err)
	}
}

func TestCreatePRHeadRun_MatrixCap(t *testing.T) {
	s, ctx, _, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	// 6^4 = 1296 > 1000 → over the cap.
	def.Jobs[0].Matrix = map[string][]string{
		"A": {"1", "2", "3", "4", "5", "6"},
		"B": {"1", "2", "3", "4", "5", "6"},
		"C": {"1", "2", "3", "4", "5", "6"},
		"D": {"1", "2", "3", "4", "5", "6"},
	}
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, def)); !errors.Is(err, store.ErrPRHeadTooManyJobs) {
		t.Fatalf("err = %v, want ErrPRHeadTooManyJobs", err)
	}
}

func TestCreatePRHeadRun_ExpectedProjectMismatch(t *testing.T) {
	s, ctx, _, materialID, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	in := prHeadInput(materialID, prHeadDef())
	in.ExpectedProjectID = uuid.New() // not this material's project
	if _, _, err := s.CreatePRHeadRun(ctx, in); !errors.Is(err, store.ErrPRHeadProjectMismatch) {
		t.Fatalf("err = %v, want ErrPRHeadProjectMismatch", err)
	}
}

func TestCreatePRHeadRun_MaterialNotFound(t *testing.T) {
	s, ctx, _, _, _ := seedPRHead(t)
	enablePRHead(t, s, ctx)
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(uuid.New(), prHeadDef())); !errors.Is(err, store.ErrPRHeadMaterialNotFound) {
		t.Fatalf("err = %v, want ErrPRHeadMaterialNotFound", err)
	}
}

// A replay of the same head SHA creates no second run and reports created=false.
func TestCreatePRHeadRun_DedupReplay(t *testing.T) {
	s, ctx, _, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)

	if _, created, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, prHeadDef())); err != nil || !created {
		t.Fatalf("first = (created=%v, err=%v), want created", created, err)
	}
	if _, created, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, prHeadDef())); err != nil || created {
		t.Fatalf("replay = (created=%v, err=%v), want created=false, nil err", created, err)
	}
	if n := countRows(t, pool, ctx,
		`SELECT count(*) FROM runs r JOIN pipelines pl ON pl.id = r.pipeline_id WHERE pl.name = 'build'`); n != 1 {
		t.Fatalf("run count = %d, want 1 (no second run on replay)", n)
	}
}

// Atomicity (#2): a failure AFTER the modification insert (here, a job wired to
// a non-existent stage, caught by insertRunRowsTx) rolls the modification back
// too, so no dedup ledger row is stranded and a retry can recover.
func TestCreatePRHeadRun_AtomicRollbackOnFailure(t *testing.T) {
	s, ctx, _, materialID, pool := seedPRHead(t)
	enablePRHead(t, s, ctx)
	def := prHeadDef()
	def.Jobs = append(def.Jobs, domain.Job{
		Name: "orphan", Stage: "ghost", Tasks: []domain.Task{{Script: "noop"}}, // stage not in Stages
	})
	if _, _, err := s.CreatePRHeadRun(ctx, prHeadInput(materialID, def)); err == nil {
		t.Fatal("expected an error for a job referencing an unknown stage")
	}
	if n := countRows(t, pool, ctx,
		`SELECT count(*) FROM modifications WHERE material_id = $1 AND revision = 'headsha01'`, materialID); n != 0 {
		t.Fatalf("modification count = %d, want 0 (rolled back with the failed run)", n)
	}
	if n := countRows(t, pool, ctx,
		`SELECT count(*) FROM runs r JOIN pipelines pl ON pl.id = r.pipeline_id WHERE pl.name = 'build'`); n != 0 {
		t.Fatalf("run count = %d, want 0", n)
	}
}
