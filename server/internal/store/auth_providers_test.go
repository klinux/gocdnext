package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newAuthCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

func TestConfiguredProvider_InsertListAndBootstrap(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	created, err := s.UpsertConfiguredProvider(ctx, store.UpsertAuthProviderInput{
		Name:         "google",
		Kind:         store.ProviderKindOIDC,
		DisplayName:  "Google Workspace",
		ClientID:     "gcp-abc",
		ClientSecret: "super-secret",
		Issuer:       "https://accounts.google.com",
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatalf("id not set")
	}

	// Admin-facing list never leaks the secret.
	list, err := s.ListConfiguredProviders(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].ClientID != "gcp-abc" || list[0].Issuer != "https://accounts.google.com" {
		t.Fatalf("row = %+v", list[0])
	}

	// Bootstrap path decrypts.
	boot, err := s.ListBootstrapProviders(ctx)
	if err != nil || len(boot) != 1 {
		t.Fatalf("bootstrap: %v len=%d", err, len(boot))
	}
	if boot[0].ClientSecret != "super-secret" {
		t.Fatalf("secret roundtrip = %q", boot[0].ClientSecret)
	}
}

func TestConfiguredProvider_UpdatePreservesSecret(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	_, err := s.UpsertConfiguredProvider(ctx, store.UpsertAuthProviderInput{
		Name: "github", Kind: store.ProviderKindGitHub,
		ClientID: "gh-client", ClientSecret: "gh-secret",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Second call with empty secret keeps the existing one, but
	// flips enabled and updates client id.
	_, err = s.UpsertConfiguredProvider(ctx, store.UpsertAuthProviderInput{
		Name: "github", Kind: store.ProviderKindGitHub,
		ClientID: "gh-client-rotated", ClientSecret: "",
		Enabled: false,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Disabled rows don't come back via the enabled list.
	boot, err := s.ListBootstrapProviders(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(boot) != 0 {
		t.Fatalf("disabled provider leaked into bootstrap: %+v", boot)
	}

	// Re-enable via the CRUD endpoint + verify the old secret still
	// decrypts. Looking up via ListConfiguredProviders → id then
	// flipping enabled with SetAuthProviderEnabled.
	list, _ := s.ListConfiguredProviders(ctx)
	if list[0].ClientID != "gh-client-rotated" {
		t.Fatalf("client id = %q", list[0].ClientID)
	}
	if err := s.SetAuthProviderEnabled(ctx, list[0].ID, true); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	boot, _ = s.ListBootstrapProviders(ctx)
	if len(boot) != 1 || boot[0].ClientSecret != "gh-secret" {
		t.Fatalf("secret not preserved: %+v", boot)
	}
}

func TestConfiguredProvider_Delete(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	created, _ := s.UpsertConfiguredProvider(ctx, store.UpsertAuthProviderInput{
		Name: "keycloak", Kind: store.ProviderKindOIDC,
		ClientID: "kc", ClientSecret: "s", Issuer: "https://kc/realms/r",
		Enabled: true,
	})

	if err := s.DeleteConfiguredProvider(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteConfiguredProvider(ctx, created.ID); !errors.Is(err, store.ErrAuthProviderNotFound) {
		t.Fatalf("second delete err = %v, want ErrAuthProviderNotFound", err)
	}
}

func TestConfiguredProvider_Validation(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	cases := []struct {
		name string
		in   store.UpsertAuthProviderInput
		want string
	}{
		{
			name: "missing name",
			in:   store.UpsertAuthProviderInput{Kind: store.ProviderKindGitHub, ClientID: "c", ClientSecret: "s"},
			want: "name + client id required",
		},
		{
			name: "invalid kind",
			in:   store.UpsertAuthProviderInput{Name: "x", Kind: "saml", ClientID: "c", ClientSecret: "s"},
			want: "invalid kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpsertConfiguredProvider(ctx, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func TestConfiguredProvider_NoCipher(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool) // cipher NOT set
	ctx := context.Background()

	_, err := s.UpsertConfiguredProvider(ctx, store.UpsertAuthProviderInput{
		Name: "github", Kind: store.ProviderKindGitHub,
		ClientID: "c", ClientSecret: "s",
	})
	if !errors.Is(err, store.ErrAuthProviderCipherUnset) {
		t.Fatalf("err = %v, want ErrAuthProviderCipherUnset", err)
	}
}
