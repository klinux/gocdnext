package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func testCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipherFromHex(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewCipherFromHex: %v", err)
	}
	return c
}

func TestSetSecret_CreatesAndUpdates(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()

	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID := applied.ProjectID

	created, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: projectID, Name: "GH_TOKEN", Value: []byte("ghp_abc"),
	})
	if err != nil || !created {
		t.Fatalf("first set: created=%v err=%v", created, err)
	}
	updated, err := s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: projectID, Name: "GH_TOKEN", Value: []byte("ghp_new"),
	})
	if err != nil || updated {
		t.Fatalf("second set: created=%v err=%v (want created=false)", updated, err)
	}
	// Re-read via ResolveSecrets to ensure the new value replaced the old.
	got, err := s.ResolveSecrets(ctx, cipher, projectID, []string{"GH_TOKEN"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["GH_TOKEN"] != "ghp_new" {
		t.Fatalf("value = %q, want ghp_new", got["GH_TOKEN"])
	}
}

func TestListSecrets_NamesOnlyNeverLeaksValue(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()

	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	_, _ = s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "A", Value: []byte("v1"),
	})
	_, _ = s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "B", Value: []byte("v2"),
	})

	list, err := s.ListSecrets(ctx, applied.ProjectID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "A" || list[1].Name != "B" {
		t.Fatalf("list = %+v", list)
	}
}

func TestResolveSecrets_UnknownNameIsOmitted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()

	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	_, _ = s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "KNOWN", Value: []byte("v"),
	})
	got, err := s.ResolveSecrets(ctx, cipher, applied.ProjectID, []string{"KNOWN", "MISSING"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := got["MISSING"]; ok {
		t.Fatalf("missing secret appeared in result: %+v", got)
	}
	if got["KNOWN"] != "v" {
		t.Fatalf("known secret value = %q", got["KNOWN"])
	}
}

func TestDeleteSecret_NotFoundErrors(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	err := s.DeleteSecret(ctx, applied.ProjectID, "ABSENT")
	if !errors.Is(err, store.ErrSecretNotFound) {
		t.Fatalf("err = %v, want ErrSecretNotFound", err)
	}
}

func TestDeleteSecret_RemovesRow(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()

	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})
	_, _ = s.SetSecret(ctx, cipher, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "X", Value: []byte("v"),
	})
	if err := s.DeleteSecret(ctx, applied.ProjectID, "X"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := s.ListSecrets(ctx, applied.ProjectID)
	if len(list) != 0 {
		t.Fatalf("secret still in list: %+v", list)
	}
}

func TestSetSecret_RejectsInvalidNames(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	cipher := testCipher(t)
	ctx := context.Background()

	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	bad := []string{
		"",                // empty
		"1STARTS_WITH_NUM",
		"has-dash",
		"spaces are bad",
		strings.Repeat("A", 100),
	}
	for _, name := range bad {
		_, err := s.SetSecret(ctx, cipher, store.SecretSet{
			ProjectID: applied.ProjectID, Name: name, Value: []byte("v"),
		})
		if err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
}

func TestSetSecret_NilCipherFails(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	applied, _ := s.ApplyProject(ctx, store.ApplyProjectInput{Slug: "demo", Name: "Demo"})

	_, err := s.SetSecret(ctx, nil, store.SecretSet{
		ProjectID: applied.ProjectID, Name: "X", Value: []byte("v"),
	})
	if err == nil {
		t.Fatalf("expected error when cipher is nil")
	}
}
