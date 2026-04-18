package store_test

import (
	"context"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// gitPipeline builds a minimal domain.Pipeline for apply tests.
// Materials list may contain git specs; the fingerprint must be set by the
// caller (store.FingerprintFor) so re-applying with the same URL/branch maps
// to the same material row.
func gitPipeline(t *testing.T, name string, stages []string, gits ...struct{ URL, Branch string }) *domain.Pipeline {
	t.Helper()
	p := &domain.Pipeline{Name: name, Stages: stages}
	for _, g := range gits {
		p.Materials = append(p.Materials, domain.Material{
			Type:        domain.MaterialGit,
			Fingerprint: store.FingerprintFor(g.URL, g.Branch),
			AutoUpdate:  true,
			Git:         &domain.GitMaterial{URL: g.URL, Branch: g.Branch, Events: []string{"push"}},
		})
	}
	return p
}

func TestApplyProject_CreatesNewProject(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	in := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
	}

	got, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}
	if !got.ProjectCreated {
		t.Fatalf("ProjectCreated = false, want true")
	}
	if len(got.Pipelines) != 1 {
		t.Fatalf("len(Pipelines) = %d, want 1", len(got.Pipelines))
	}
	p := got.Pipelines[0]
	if p.Name != "build" || !p.Created {
		t.Fatalf("pipeline = %+v, want build/created", p)
	}
	if p.MaterialsAdded != 1 {
		t.Fatalf("MaterialsAdded = %d, want 1", p.MaterialsAdded)
	}
	if len(got.PipelinesRemoved) != 0 {
		t.Fatalf("PipelinesRemoved = %v, want empty", got.PipelinesRemoved)
	}
}

func TestApplyProject_ReApplyIdempotent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	in := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
	}

	first, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.ProjectCreated {
		t.Fatalf("2nd apply ProjectCreated = true, want false")
	}
	if second.ProjectID != first.ProjectID {
		t.Fatalf("ProjectID changed on re-apply: %s vs %s", first.ProjectID, second.ProjectID)
	}
	if len(second.Pipelines) != 1 {
		t.Fatalf("len(Pipelines) = %d", len(second.Pipelines))
	}
	p := second.Pipelines[0]
	if p.Created {
		t.Fatalf("2nd apply pipeline Created = true")
	}
	if p.MaterialsAdded != 0 || p.MaterialsRemoved != 0 {
		t.Fatalf("2nd apply diff: added=%d removed=%d, want 0/0", p.MaterialsAdded, p.MaterialsRemoved)
	}
	if p.PipelineID != first.Pipelines[0].PipelineID {
		t.Fatalf("PipelineID changed on re-apply")
	}
}

func TestApplyProject_AddAndRemoveMaterial(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	a := struct{ URL, Branch string }{"https://github.com/org/demo", "main"}
	b := struct{ URL, Branch string }{"https://github.com/org/lib", "main"}

	in1 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build"}, a)},
	}
	if _, err := s.ApplyProject(ctx, in1); err != nil {
		t.Fatalf("apply 1: %v", err)
	}

	in2 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build"}, a, b)},
	}
	r2, err := s.ApplyProject(ctx, in2)
	if err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	if got := r2.Pipelines[0].MaterialsAdded; got != 1 {
		t.Fatalf("after add: MaterialsAdded = %d, want 1", got)
	}

	in3 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build"}, b)},
	}
	r3, err := s.ApplyProject(ctx, in3)
	if err != nil {
		t.Fatalf("apply 3: %v", err)
	}
	if got := r3.Pipelines[0].MaterialsRemoved; got != 1 {
		t.Fatalf("after remove: MaterialsRemoved = %d, want 1", got)
	}
	if got := r3.Pipelines[0].MaterialsAdded; got != 0 {
		t.Fatalf("after remove: MaterialsAdded = %d, want 0", got)
	}

	var materialCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM materials m
		 JOIN pipelines p ON p.id = m.pipeline_id
		 JOIN projects pr ON pr.id = p.project_id
		 WHERE pr.slug = 'demo'`,
	).Scan(&materialCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if materialCount != 1 {
		t.Fatalf("material rows = %d, want 1", materialCount)
	}
}

func TestApplyProject_RemovePipeline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	g := struct{ URL, Branch string }{"https://github.com/org/demo", "main"}

	in1 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, g),
			gitPipeline(t, "deploy", []string{"deploy"}, g),
		},
	}
	if _, err := s.ApplyProject(ctx, in1); err != nil {
		t.Fatalf("apply 1: %v", err)
	}

	in2 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build"}, g)},
	}
	r2, err := s.ApplyProject(ctx, in2)
	if err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	if len(r2.PipelinesRemoved) != 1 || r2.PipelinesRemoved[0] != "deploy" {
		t.Fatalf("PipelinesRemoved = %v, want [deploy]", r2.PipelinesRemoved)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pipelines p
		 JOIN projects pr ON pr.id = p.project_id
		 WHERE pr.slug = 'demo'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("pipeline rows = %d, want 1", remaining)
	}
}

func TestApplyProject_WithSCMSourcePersists(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	in := store.ApplyProjectInput{
		Slug: "scm", Name: "SCM",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           "https://github.com/org/demo",
			DefaultBranch: "main",
			WebhookSecret: "s3cret",
		},
	}

	got, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}
	if got.SCMSource == nil || !got.SCMSource.Created {
		t.Fatalf("SCMSource = %+v, want created", got.SCMSource)
	}

	var url, provider, branch, webhookSecret string
	if err := pool.QueryRow(ctx,
		`SELECT url, provider, default_branch, COALESCE(webhook_secret, '')
		 FROM scm_sources WHERE project_id = $1`, got.ProjectID,
	).Scan(&url, &provider, &branch, &webhookSecret); err != nil {
		t.Fatalf("scm_sources row: %v", err)
	}
	if url != "https://github.com/org/demo" || provider != "github" || branch != "main" || webhookSecret != "s3cret" {
		t.Fatalf("scm_sources row: url=%s provider=%s branch=%s secret=%s", url, provider, branch, webhookSecret)
	}
}

func TestApplyProject_WithSCMSourceIdempotentReapply(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	in := store.ApplyProjectInput{
		Slug: "scm", Name: "SCM",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
		SCMSource: &store.SCMSourceInput{Provider: "github", URL: "https://github.com/org/demo", DefaultBranch: "main"},
	}

	first, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.SCMSource == nil || second.SCMSource.Created {
		t.Fatalf("2nd apply should not create a new scm_source: %+v", second.SCMSource)
	}
	if second.SCMSource.ID != first.SCMSource.ID {
		t.Fatalf("scm_source id changed on re-apply: %s vs %s", first.SCMSource.ID, second.SCMSource.ID)
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scm_sources WHERE project_id = $1`, first.ProjectID).Scan(&count)
	if count != 1 {
		t.Fatalf("scm_sources rows = %d, want 1", count)
	}
}

func TestApplyProject_SCMSourceRequiresURLAndProvider(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "scm", Name: "SCM",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
		SCMSource: &store.SCMSourceInput{Provider: "github"}, // URL missing
	})
	if err == nil {
		t.Fatalf("expected error when SCMSource.URL is empty")
	}
}

func TestApplyProject_NilSCMSourceIsNoOp(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	in := store.ApplyProjectInput{
		Slug: "no-scm", Name: "No SCM",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
	}
	got, err := s.ApplyProject(ctx, in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.SCMSource != nil {
		t.Fatalf("SCMSource = %+v, want nil", got.SCMSource)
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scm_sources WHERE project_id = $1`, got.ProjectID).Scan(&count)
	if count != 0 {
		t.Fatalf("scm_sources rows = %d, want 0", count)
	}
}

func TestApplyProject_DefinitionVersionBumpsOnChange(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	g := struct{ URL, Branch string }{"https://github.com/org/demo", "main"}

	in1 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build"}, g)},
	}
	if _, err := s.ApplyProject(ctx, in1); err != nil {
		t.Fatalf("apply 1: %v", err)
	}

	in2 := store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{gitPipeline(t, "build", []string{"build", "test"}, g)},
	}
	if _, err := s.ApplyProject(ctx, in2); err != nil {
		t.Fatalf("apply 2: %v", err)
	}

	var version int
	if err := pool.QueryRow(ctx,
		`SELECT definition_version FROM pipelines p
		 JOIN projects pr ON pr.id = p.project_id
		 WHERE pr.slug = 'demo' AND p.name = 'build'`,
	).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version < 2 {
		t.Fatalf("definition_version = %d, want >= 2", version)
	}

	if _, err := s.ApplyProject(ctx, in2); err != nil {
		t.Fatalf("apply 3: %v", err)
	}
	var versionAfterNoop int
	if err := pool.QueryRow(ctx,
		`SELECT definition_version FROM pipelines p
		 JOIN projects pr ON pr.id = p.project_id
		 WHERE pr.slug = 'demo' AND p.name = 'build'`,
	).Scan(&versionAfterNoop); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if versionAfterNoop != version {
		t.Fatalf("definition_version bumped on noop apply: %d → %d", version, versionAfterNoop)
	}
}
