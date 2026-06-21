package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func newGovStore(t *testing.T) (*store.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t)) // ApplyProject seals the scm webhook secret
	return s, pool, context.Background()
}

func applyWithSCM(t *testing.T, s *store.Store, ctx context.Context, slug string, pipelines []*domain.Pipeline) {
	t.Helper()
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug, Pipelines: pipelines,
		SCMSource: &store.SCMSourceInput{
			Provider: "github", URL: "https://github.com/acme/" + slug, DefaultBranch: "main",
		},
	})
	if err != nil {
		t.Fatalf("apply project %s: %v", slug, err)
	}
}

// pipelineByName reads a project's pipeline row (effective definition) by name.
func pipelineByName(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug, name string) (id string, def string, ok bool) {
	t.Helper()
	row := pool.QueryRow(ctx, `
		SELECT p.id::text, p.definition::text
		FROM pipelines p JOIN projects pr ON pr.id = p.project_id
		WHERE pr.slug = $1 AND p.name = $2`, slug, name)
	if err := row.Scan(&id, &def); err != nil {
		return "", "", false
	}
	return id, def, true
}

func materialConfigs(t *testing.T, pool *pgxpool.Pool, ctx context.Context, pipelineID string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT config::text FROM materials WHERE pipeline_id = $1`, pipelineID)
	if err != nil {
		t.Fatalf("materials query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan material: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func TestComplianceSynthetic_AppliesToAllOnEmptyProject(t *testing.T) {
	s, pool, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global-scan", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}

	// Empty project (no pipelines) but with an scm binding + a global policy →
	// the synthetic `_compliance` pipeline must be created so the policy runs.
	applyWithSCM(t, s, ctx, "empty", nil)

	id, def, ok := pipelineByName(t, pool, ctx, "empty", "_compliance")
	if !ok {
		t.Fatal("synthetic _compliance pipeline was not created")
	}
	if !strings.Contains(def, "_compliance_scan") {
		t.Fatalf("synthetic pipeline missing policy job: %s", def)
	}
	mats := materialConfigs(t, pool, ctx, id)
	if len(mats) == 0 || !strings.Contains(mats[0], "github.com/acme/empty") {
		t.Fatalf("synthetic pipeline missing its git material: %v", mats)
	}
}

func TestComplianceSynthetic_CreatedOnFrameworkAssign(t *testing.T) {
	s, pool, ctx := newGovStore(t)
	applyWithSCM(t, s, ctx, "svc", nil) // empty, ungoverned → no synthetic yet
	if _, _, ok := pipelineByName(t, pool, ctx, "svc", "_compliance"); ok {
		t.Fatal("synthetic created before governance")
	}

	fw, err := s.InsertComplianceFramework(ctx, store.FrameworkInput{Name: "SOC2"})
	if err != nil {
		t.Fatalf("framework: %v", err)
	}
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "soc2-scan", Mode: "inject", Enabled: true,
		FrameworkIDs: []string{fw.ID}, ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	pid, _ := uuid.Parse(mustProjectID(t, s, ctx, "svc"))
	if err := s.SetProjectFrameworks(ctx, pid, []string{fw.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// Recompute (via assignment) must have synthesised the pipeline.
	if _, def, ok := pipelineByName(t, pool, ctx, "svc", "_compliance"); !ok || !strings.Contains(def, "_compliance_scan") {
		t.Fatalf("synthetic not created on framework assign (ok=%v def=%s)", ok, def)
	}

	// Removing the framework tears it back down.
	if err := s.SetProjectFrameworks(ctx, pid, nil); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	if _, _, ok := pipelineByName(t, pool, ctx, "svc", "_compliance"); ok {
		t.Fatal("synthetic not removed after un-governance")
	}
}

func TestComplianceTriggerOverrideOnRepoPipeline(t *testing.T) {
	s, pool, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	applyWithSCM(t, s, ctx, "app", []*domain.Pipeline{{
		Name: "main", Stages: []string{"build"}, Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
	}})

	// No synthetic (repo has a pipeline); the repo pipeline carries a
	// compliance-owned default-branch material so the repo can't suppress it.
	if _, _, ok := pipelineByName(t, pool, ctx, "app", "_compliance"); ok {
		t.Fatal("unexpected synthetic when repo pipeline exists")
	}
	id, def, ok := pipelineByName(t, pool, ctx, "app", "main")
	if !ok || !strings.Contains(def, "_compliance_scan") {
		t.Fatalf("repo pipeline not merged with policy: %s", def)
	}
	mats := materialConfigs(t, pool, ctx, id)
	found := false
	for _, m := range mats {
		if strings.Contains(m, `"branch": "main"`) && !strings.Contains(m, `paths`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("governed repo pipeline missing non-suppressible default-branch material: %v", mats)
	}
}

func TestComplianceBlocksGovernedWithoutSCMOrPipeline(t *testing.T) {
	s, _, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// No SCM + no pipeline + governed → cannot enforce → refuse.
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "void", Name: "void"})
	if err == nil || !errors.Is(err, store.ErrComplianceWouldDropEnforcement) {
		t.Fatalf("expected enforcement-drop refusal, got %v", err)
	}
}

func TestComplianceRefusesGovernedPipelineWithoutSCM(t *testing.T) {
	s, _, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// A project WITH a pipeline but NO scm binding can't have a non-suppressible
	// trigger installed → governance is refused (the remaining no-scm bypass).
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "nobind", Name: "nobind",
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"build"}, Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
		}},
	})
	if err == nil || !errors.Is(err, store.ErrComplianceWouldDropEnforcement) {
		t.Fatalf("expected refusal for governed pipeline without scm, got %v", err)
	}
}

func TestComplianceMaterialMergePreservesRepoFields(t *testing.T) {
	s, pool, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	url := "https://github.com/acme/merge"
	sshURL := "git@github.com:acme/merge.git" // same repo, SSH remote (normalises equal)
	fp := domain.GitFingerprint(url, "main")
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "merge", Name: "merge",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: url, DefaultBranch: "main"},
		Pipelines: []*domain.Pipeline{{
			Name: "main", Stages: []string{"build"}, Jobs: []domain.Job{{Name: "compile", Stage: "build"}},
			// Explicit repo material on the default branch: SSH remote, with
			// credentials, a tag trigger, and a path filter.
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{
					URL: sshURL, Branch: "main", Events: []string{"tag"},
					SecretRef: "gh-token", Paths: []string{"docs/**"},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	id, _, ok := pipelineByName(t, pool, ctx, "merge", "main")
	if !ok {
		t.Fatal("pipeline missing")
	}
	var merged string
	for _, m := range materialConfigs(t, pool, ctx, id) {
		if strings.Contains(m, `"branch": "main"`) {
			merged = m
		}
	}
	if merged == "" {
		t.Fatal("default-branch material not found")
	}
	if !strings.Contains(merged, "gh-token") {
		t.Errorf("secret_ref not preserved: %s", merged)
	}
	if !strings.Contains(merged, "git@github.com:acme/merge.git") {
		t.Errorf("explicit SSH clone URL not preserved (would break the agent's clone): %s", merged)
	}
	if !strings.Contains(merged, `"push"`) || !strings.Contains(merged, `"tag"`) {
		t.Errorf("events not unioned (want push+tag): %s", merged)
	}
	if strings.Contains(merged, "docs") {
		t.Errorf("paths not cleared on compliance override: %s", merged)
	}
}

func TestComplianceSyntheticRefreshedOnDefaultBranchChange(t *testing.T) {
	s, pool, ctx := newGovStore(t)
	if _, err := s.InsertCompliancePolicy(ctx, store.PolicyInput{
		Name: "global", Mode: "inject", Enabled: true, AppliesToAll: true,
		ConfigYAML: scanPolicyYAML,
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// Empty governed project on default branch "main" → synthetic on main.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "drift", Name: "drift",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: "https://github.com/acme/drift", DefaultBranch: "main"},
	}); err != nil {
		t.Fatalf("apply main: %v", err)
	}
	id, _, ok := pipelineByName(t, pool, ctx, "drift", "_compliance")
	if !ok {
		t.Fatal("synthetic not created")
	}

	// Default branch changes to "release" → the synthetic's material must move
	// with it (old fingerprint pruned), or pushes on release wouldn't fire.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "drift", Name: "drift",
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: "https://github.com/acme/drift", DefaultBranch: "release"},
	}); err != nil {
		t.Fatalf("apply release: %v", err)
	}
	mats := materialConfigs(t, pool, ctx, id)
	if len(mats) != 1 {
		t.Fatalf("expected exactly one (refreshed) material, got %d: %v", len(mats), mats)
	}
	if !strings.Contains(mats[0], `"branch": "release"`) {
		t.Fatalf("synthetic material not refreshed to new default branch: %v", mats)
	}
}

func TestComplianceReservedPipelineName(t *testing.T) {
	s, _, ctx := newGovStore(t)
	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "x", Name: "x",
		Pipelines: []*domain.Pipeline{{Name: "_compliance", Stages: []string{"s"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-name rejection, got %v", err)
	}
}
