package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestUserPreferences_EmptyWhenNeverWritten(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	user := mustSeedUser(t, s, "new-user@example.com", "ext-1")
	prefs, _, err := s.GetUserPreferences(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserPreferences: %v", err)
	}
	if len(prefs.HiddenProjects) != 0 {
		t.Fatalf("expected empty prefs, got %+v", prefs)
	}
}

func TestUserPreferences_UpsertRoundTrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	user := mustSeedUser(t, s, "pref-user@example.com", "ext-2")

	hidden := []uuid.UUID{
		uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
	saved, _, err := s.SetUserPreferences(ctx, user.ID, store.UserPreferences{
		HiddenProjects: hidden,
	})
	if err != nil {
		t.Fatalf("SetUserPreferences: %v", err)
	}
	if len(saved.HiddenProjects) != 2 {
		t.Fatalf("round-trip mismatch: %+v", saved)
	}

	got, _, err := s.GetUserPreferences(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserPreferences after save: %v", err)
	}
	if len(got.HiddenProjects) != 2 || got.HiddenProjects[0] != hidden[0] {
		t.Fatalf("read-back mismatch: %+v", got)
	}

	// Second upsert replaces wholesale (full-document PUT semantics)
	// — a regression guard for any future "merge" refactor that
	// would surprise the UI's edit-in-place model.
	_, _, err = s.SetUserPreferences(ctx, user.ID, store.UserPreferences{})
	if err != nil {
		t.Fatalf("clear preferences: %v", err)
	}
	got2, _, err := s.GetUserPreferences(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserPreferences after clear: %v", err)
	}
	if len(got2.HiddenProjects) != 0 {
		t.Fatalf("clear should empty hidden_projects, got %+v", got2)
	}
}

func mustSeedUser(t *testing.T, s *store.Store, email, externalID string) store.User {
	t.Helper()
	user, err := s.UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email:      email,
		Name:       "Test",
		Provider:   "test",
		ExternalID: externalID,
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user
}
