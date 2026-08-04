package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestProjectTrustSameRepoPRConfig_GetSetRoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "pay", Name: "pay"}); err != nil {
		t.Fatalf("ApplyProject: %v", err)
	}

	// Default is false (NOT NULL DEFAULT false).
	got, err := s.GetProjectTrustSameRepoPRConfigBySlug(ctx, "pay")
	if err != nil {
		t.Fatalf("Get default: %v", err)
	}
	if got {
		t.Fatalf("default = %v, want false", got)
	}

	// Enable → read back true.
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "pay", true); err != nil {
		t.Fatalf("Set true: %v", err)
	}
	if got, err = s.GetProjectTrustSameRepoPRConfigBySlug(ctx, "pay"); err != nil || !got {
		t.Fatalf("after enable = %v (err=%v), want true", got, err)
	}

	// Disable → read back false (idempotent flip both ways).
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "pay", false); err != nil {
		t.Fatalf("Set false: %v", err)
	}
	if got, _ = s.GetProjectTrustSameRepoPRConfigBySlug(ctx, "pay"); got {
		t.Fatalf("after disable = %v, want false", got)
	}
}

// A drift re-apply (or any ApplyProject) must NOT reset the security toggle:
// UpsertProject's ON CONFLICT only touches name/description/config_path, so an
// enabled flag survives a config sync. This pins that so a future column added
// to the upsert SET can't silently disable the feature.
func TestProjectTrustSameRepoPRConfig_PreservedAcrossApply(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "pay", Name: "pay"}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "pay", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Re-apply the project (what a default-branch push / drift sync does).
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "pay", Name: "pay"}); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got, err := s.GetProjectTrustSameRepoPRConfigBySlug(ctx, "pay"); err != nil || !got {
		t.Fatalf("after re-apply = %v (err=%v), want true (preserved)", got, err)
	}
}

func TestProjectTrustSameRepoPRConfig_UnknownProject(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// A missing project is a typed ErrProjectNotFound, not an opaque error,
	// on both the read and the write path (RowsAffected == 0 on Set).
	if _, err := s.GetProjectTrustSameRepoPRConfigBySlug(ctx, "nope"); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("Get unknown = %v, want ErrProjectNotFound", err)
	}
	if err := s.SetProjectTrustSameRepoPRConfigBySlug(ctx, "nope", true); !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("Set unknown = %v, want ErrProjectNotFound", err)
	}
}
