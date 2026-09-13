package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func TestProjectBadges_TokenGateAndLatestRunFilters(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	s.SetAuthCipher(newAuthCipher(t))
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo",
		Name: "Demo",
		Pipelines: []*domain.Pipeline{
			gitPipeline(t, "build", []string{"build", "test"}, struct{ URL, Branch string }{"https://github.com/org/demo", "main"}),
		},
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           "https://github.com/org/demo",
			DefaultBranch: "main",
		},
	}); err != nil {
		t.Fatalf("bind scm source: %v", err)
	}

	token := "test-token"
	hash := badgetoken.HashToken(token)

	enabled, err := s.GetProjectBadgeEnabledBySlug(ctx, "demo")
	if err != nil {
		t.Fatalf("Get enabled default: %v", err)
	}
	if enabled {
		t.Fatalf("badge default enabled = true, want false")
	}

	mainRun, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("main run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status=$2, created_at=NOW() - interval '1 minute' WHERE id=$1`,
		mainRun.RunID, string(domain.StatusSuccess)); err != nil {
		t.Fatalf("mark main: %v", err)
	}
	featureIn := baseTriggerInput(pipelineID, materialID, 2)
	featureIn.Branch = "feature/badge"
	featureIn.Revision = "ffffffffffffffffffffffffffffffffffffffff"
	featureRun, err := s.CreateRunFromModification(ctx, featureIn)
	if err != nil {
		t.Fatalf("feature run: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE runs SET status=$2, created_at=NOW() WHERE id=$1`,
		featureRun.RunID, string(domain.StatusFailed)); err != nil {
		t.Fatalf("mark feature: %v", err)
	}

	if _, ok, err := s.GetBadgeLatestRun(ctx, "demo", hash, "build", ""); err != nil || ok {
		t.Fatalf("disabled lookup ok=%v err=%v, want no row", ok, err)
	}
	if err := s.SetProjectBadgeTokenHashBySlug(ctx, "demo", hash); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if enabled, err = s.GetProjectBadgeEnabledBySlug(ctx, "demo"); err != nil || !enabled {
		t.Fatalf("after enable = %v err=%v, want true", enabled, err)
	}

	latestDefault, ok, err := s.GetBadgeLatestRun(ctx, "demo", hash, "build", "")
	if err != nil || !ok {
		t.Fatalf("latest default ok=%v err=%v", ok, err)
	}
	if latestDefault.ID != mainRun.RunID || latestDefault.Status != string(domain.StatusSuccess) {
		t.Fatalf("latest default = %+v, want main success", latestDefault)
	}

	latestMain, ok, err := s.GetBadgeLatestRun(ctx, "demo", hash, "build", "main")
	if err != nil || !ok {
		t.Fatalf("latest main ok=%v err=%v", ok, err)
	}
	if latestMain.ID != mainRun.RunID || latestMain.Status != string(domain.StatusSuccess) {
		t.Fatalf("latest main = %+v, want main success", latestMain)
	}
	if time.Since(latestMain.CreatedAt) > time.Hour {
		t.Fatalf("CreatedAt not populated: %v", latestMain.CreatedAt)
	}

	latestFeature, ok, err := s.GetBadgeLatestRun(ctx, "demo", hash, "build", "feature/badge")
	if err != nil || !ok {
		t.Fatalf("latest feature ok=%v err=%v", ok, err)
	}
	if latestFeature.ID != featureRun.RunID || latestFeature.Status != string(domain.StatusFailed) {
		t.Fatalf("latest feature = %+v, want feature failed", latestFeature)
	}

	if _, ok, err := s.GetBadgeLatestRun(ctx, "demo", hash, "missing", ""); err != nil || ok {
		t.Fatalf("missing pipeline ok=%v err=%v, want no row", ok, err)
	}
	if _, ok, err := s.GetBadgeLatestRun(ctx, "demo", badgetoken.HashToken("wrong"), "build", ""); err != nil || ok {
		t.Fatalf("wrong token ok=%v err=%v, want no row", ok, err)
	}

	if err := s.ClearProjectBadgeTokenBySlug(ctx, "demo"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if enabled, err = s.GetProjectBadgeEnabledBySlug(ctx, "demo"); err != nil || enabled {
		t.Fatalf("after disable = %v err=%v, want false", enabled, err)
	}
}

func TestProjectBadges_UnknownProject(t *testing.T) {
	s := store.New(dbtest.SetupPool(t))
	ctx := context.Background()
	if _, err := s.GetProjectBadgeEnabledBySlug(ctx, "nope"); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("Get unknown = %v, want ErrProjectNotFound", err)
	}
	if err := s.SetProjectBadgeTokenHashBySlug(ctx, "nope", badgetoken.HashToken("x")); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("Set unknown = %v, want ErrProjectNotFound", err)
	}
	if err := s.ClearProjectBadgeTokenBySlug(ctx, "nope"); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("Clear unknown = %v, want ErrProjectNotFound", err)
	}
}
