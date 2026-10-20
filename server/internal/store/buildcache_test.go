package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestProjectCacheToken_SetVerifyClear(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	projectID := seedProject(t, s, "bc-token")

	// Disabled by default.
	if enabled, err := s.GetProjectCacheEnabledBySlug(ctx, "bc-token"); err != nil || enabled {
		t.Fatalf("default enabled=%v err=%v, want false", enabled, err)
	}
	if _, ok, err := s.VerifyProjectCacheToken(ctx, "bc-token", hex64("a")); err != nil || ok {
		t.Fatalf("verify before set ok=%v err=%v, want false", ok, err)
	}

	// Enable with a hash.
	hash := hex64("b")
	if err := s.SetProjectCacheTokenHashBySlug(ctx, "bc-token", hash); err != nil {
		t.Fatalf("set: %v", err)
	}
	if enabled, err := s.GetProjectCacheEnabledBySlug(ctx, "bc-token"); err != nil || !enabled {
		t.Fatalf("enabled after set=%v err=%v, want true", enabled, err)
	}

	// Correct hash resolves the project id; wrong hash does not.
	gotID, ok, err := s.VerifyProjectCacheToken(ctx, "bc-token", hash)
	if err != nil || !ok {
		t.Fatalf("verify ok=%v err=%v, want true", ok, err)
	}
	if gotID != projectID {
		t.Errorf("verify id=%s, want %s", gotID, projectID)
	}
	if _, ok, _ := s.VerifyProjectCacheToken(ctx, "bc-token", hex64("c")); ok {
		t.Error("wrong hash verified")
	}
	// A different project's slug with this hash must not match.
	if _, ok, _ := s.VerifyProjectCacheToken(ctx, "other", hash); ok {
		t.Error("cross-project token verified")
	}

	// Clear disables it.
	if err := s.ClearProjectCacheTokenBySlug(ctx, "bc-token"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if enabled, _ := s.GetProjectCacheEnabledBySlug(ctx, "bc-token"); enabled {
		t.Error("still enabled after clear")
	}
	if _, ok, _ := s.VerifyProjectCacheToken(ctx, "bc-token", hash); ok {
		t.Error("token verified after clear")
	}
}

func TestProjectCacheToken_UnknownProject(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	if err := s.SetProjectCacheTokenHashBySlug(ctx, "nope", hex64("a")); !errors.Is(err, store.ErrProjectNotFound) {
		t.Errorf("set unknown: %v, want ErrProjectNotFound", err)
	}
	if err := s.ClearProjectCacheTokenBySlug(ctx, "nope"); !errors.Is(err, store.ErrProjectNotFound) {
		t.Errorf("clear unknown: %v, want ErrProjectNotFound", err)
	}
	if _, err := s.GetProjectCacheEnabledBySlug(ctx, "nope"); !errors.Is(err, store.ErrProjectNotFound) {
		t.Errorf("get unknown: %v, want ErrProjectNotFound", err)
	}
	if _, ok, err := s.VerifyProjectCacheToken(ctx, "nope", hex64("a")); err != nil || ok {
		t.Errorf("verify unknown ok=%v err=%v, want false/nil", ok, err)
	}
}

// hex64 builds a valid 64-char lowercase-hex digest from a seed char so the
// CHECK constraint (^[0-9a-f]{64}$) accepts it.
func hex64(seed string) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = seed[0]
	}
	return string(out)
}
