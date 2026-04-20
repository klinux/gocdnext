package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Sample RSA-looking PEM. We never actually parse it in the store;
// it's treated as an opaque ciphertext roundtrip. Tests for real
// parsing live alongside the registry (UI.9.b).
const testPEM = `-----BEGIN RSA PRIVATE KEY-----
FAKE-TEST-KEY-0123456789abcdefghijklmnopqrstuvwxyz0123456789abcdef
-----END RSA PRIVATE KEY-----
`

func ptrInt64(v int64) *int64 { return &v }

func TestVCSIntegration_InsertListBootstrapRoundtrip(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	created, err := s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name:          "primary-github",
		Kind:          store.VCSKindGitHubApp,
		DisplayName:   "gocdnext primary",
		AppID:         ptrInt64(123456),
		PrivateKeyPEM: []byte(testPEM),
		WebhookSecret: "wh-secret",
		APIBase:       "",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatalf("id not set")
	}
	if !created.HasPrivateKey || !created.HasWebhookSecret {
		t.Fatalf("flags = %+v, both should be true on fresh insert", created)
	}

	// Admin list never leaks ciphertext or plaintext secret.
	list, err := s.ListConfiguredVCSIntegrations(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].AppID == nil || *list[0].AppID != 123456 {
		t.Fatalf("app_id mismatch: %+v", list[0])
	}

	// Bootstrap path decrypts both secrets.
	boot, err := s.ListBootstrapVCSIntegrations(ctx)
	if err != nil || len(boot) != 1 {
		t.Fatalf("bootstrap: %v len=%d", err, len(boot))
	}
	if string(boot[0].PrivateKeyPEM) != testPEM {
		t.Fatalf("private key roundtrip failed")
	}
	if boot[0].WebhookSecret != "wh-secret" {
		t.Fatalf("webhook secret roundtrip failed: %q", boot[0].WebhookSecret)
	}
}

func TestVCSIntegration_UpdatePreservesSecretsWhenEmpty(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	if _, err := s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name: "gh", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(1), PrivateKeyPEM: []byte(testPEM),
		WebhookSecret: "first", Enabled: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Second upsert with empty secrets: app_id must bump, other
	// fields may change, but the stored ciphertext for both key
	// and webhook_secret stays intact.
	if _, err := s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name: "gh", Kind: store.VCSKindGitHubApp,
		DisplayName: "renamed",
		AppID:       ptrInt64(999),
		Enabled:     false, // also flip enabled to exercise the path
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	list, _ := s.ListConfiguredVCSIntegrations(ctx)
	if list[0].DisplayName != "renamed" || *list[0].AppID != 999 || list[0].Enabled {
		t.Fatalf("update didn't apply: %+v", list[0])
	}

	// Re-enable and confirm the ORIGINAL secrets still decrypt.
	_ = s.SetVCSIntegrationEnabled(ctx, list[0].ID, true)
	boot, _ := s.ListBootstrapVCSIntegrations(ctx)
	if len(boot) != 1 || string(boot[0].PrivateKeyPEM) != testPEM || boot[0].WebhookSecret != "first" {
		t.Fatalf("secrets were wiped on empty-field update: %+v", boot)
	}
}

func TestVCSIntegration_DisabledExcludedFromBootstrap(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	_, _ = s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name: "off", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(42), PrivateKeyPEM: []byte(testPEM),
		Enabled: false,
	})
	boot, _ := s.ListBootstrapVCSIntegrations(ctx)
	if len(boot) != 0 {
		t.Fatalf("disabled row leaked into bootstrap: %+v", boot)
	}
}

func TestVCSIntegration_Delete(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	created, _ := s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name: "d", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(1), PrivateKeyPEM: []byte(testPEM),
		Enabled: true,
	})
	if err := s.DeleteVCSIntegration(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteVCSIntegration(ctx, created.ID); !errors.Is(err, store.ErrVCSIntegrationNotFound) {
		t.Fatalf("second delete err = %v, want ErrVCSIntegrationNotFound", err)
	}
}

func TestVCSIntegration_Validation(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newAuthCipher(t))
	ctx := context.Background()

	cases := []struct {
		name string
		in   store.UpsertVCSIntegrationInput
		want string
	}{
		{
			name: "missing name",
			in:   store.UpsertVCSIntegrationInput{Kind: store.VCSKindGitHubApp, AppID: ptrInt64(1), PrivateKeyPEM: []byte(testPEM)},
			want: "name required",
		},
		{
			name: "invalid kind",
			in:   store.UpsertVCSIntegrationInput{Name: "x", Kind: "bitbucket_app", AppID: ptrInt64(1), PrivateKeyPEM: []byte(testPEM)},
			want: "unsupported vcs kind",
		},
		{
			name: "github_app missing app_id",
			in:   store.UpsertVCSIntegrationInput{Name: "x", Kind: store.VCSKindGitHubApp, PrivateKeyPEM: []byte(testPEM)},
			want: "positive app_id",
		},
		{
			name: "github_app zero app_id",
			in:   store.UpsertVCSIntegrationInput{Name: "x", Kind: store.VCSKindGitHubApp, AppID: ptrInt64(0), PrivateKeyPEM: []byte(testPEM)},
			want: "positive app_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpsertVCSIntegration(ctx, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

func TestVCSIntegration_NoCipher(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool) // cipher NOT set
	ctx := context.Background()

	_, err := s.UpsertVCSIntegration(ctx, store.UpsertVCSIntegrationInput{
		Name: "x", Kind: store.VCSKindGitHubApp,
		AppID: ptrInt64(1), PrivateKeyPEM: []byte(testPEM),
	})
	if !errors.Is(err, store.ErrAuthProviderCipherUnset) {
		t.Fatalf("err = %v, want ErrAuthProviderCipherUnset", err)
	}
}
