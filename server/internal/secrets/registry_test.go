package secrets

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// regBackend is a fake external.Backend that records build/close counts.
type regBackend struct {
	source string
	closed *int32
}

func (b *regBackend) Name() string { return b.source }
func (b *regBackend) Fetch(context.Context, string, string) (string, error) {
	return "v", nil
}
func (b *regBackend) HealthCheck(context.Context) error { return nil }
func (b *regBackend) Close() error                      { atomic.AddInt32(b.closed, 1); return nil }

func regCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	c, err := crypto.NewCipherFromHex("11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00" + "11" + "22" + "33" + "44" + "55" + "66" + "77" + "88" + "99" + "aa" + "bb" + "cc" + "dd" + "ee" + "ff" + "00")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// newRegistryWithFakeFactory builds a registry whose factory is a counting
// fake (no real Vault/AWS/GCP), so behaviour is testable without network.
func newRegistryWithFakeFactory(t *testing.T, baseline RegistryConfig) (*SecretBackendRegistry, *int32, *int32) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	r := NewSecretBackendRegistry(st, regCipher(t), baseline, nil)
	var builds, closes int32
	r.factory = func(_ context.Context, cfg backendConfig) (external.Backend, error) {
		atomic.AddInt32(&builds, 1)
		return &regBackend{source: cfg.source, closed: &closes}, nil
	}
	return r, &builds, &closes
}

func TestRegistry_EnvBaseline_BuildsAndCaches(t *testing.T) {
	r, builds, _ := newRegistryWithFakeFactory(t, RegistryConfig{
		VaultEnabled: true,
		Vault:        external.VaultConfig{Addr: "http://vault", AuthMethod: "token", Token: "t"},
	})
	ctx := context.Background()
	if err := r.Prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if got := r.ConfiguredSources(); len(got) != 1 || got[0] != store.SecretSourceVault {
		t.Fatalf("ConfiguredSources = %v, want [vault]", got)
	}
	// Prime eagerly built vault once; a Backend call reuses the cached client.
	if _, _, err := r.Backend(ctx, store.SecretSourceVault); err != nil {
		t.Fatalf("backend: %v", err)
	}
	if b := atomic.LoadInt32(builds); b != 1 {
		t.Fatalf("builds = %d, want 1 (cached after first build)", b)
	}
}

func TestRegistry_NotConfigured(t *testing.T) {
	r, _, _ := newRegistryWithFakeFactory(t, RegistryConfig{})
	if err := r.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if _, _, err := r.Backend(context.Background(), store.SecretSourceAWS); !errors.Is(err, ErrBackendNotConfigured) {
		t.Fatalf("err = %v, want ErrBackendNotConfigured", err)
	}
	if got := r.ConfiguredSources(); len(got) != 0 {
		t.Fatalf("ConfiguredSources = %v, want empty", got)
	}
}

func TestRegistry_DBOverlay_EnablesAndRebuildsOnChange(t *testing.T) {
	r, builds, closes := newRegistryWithFakeFactory(t, RegistryConfig{}) // nothing in env
	ctx := context.Background()
	if err := r.Prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if got := r.ConfiguredSources(); len(got) != 0 {
		t.Fatalf("pre: ConfiguredSources = %v, want empty", got)
	}

	// Enable vault via the DB.
	if err := r.store.SetSecretBackend(ctx, r.cipher, store.SecretBackendInput{
		Source: store.SecretSourceVault,
		Value:  map[string]any{"enabled": true, "addr": "http://a", "auth": "token"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := r.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := r.ConfiguredSources(); len(got) != 1 || got[0] != store.SecretSourceVault {
		t.Fatalf("post-enable: ConfiguredSources = %v, want [vault]", got)
	}
	if _, _, err := r.Backend(ctx, store.SecretSourceVault); err != nil {
		t.Fatalf("backend: %v", err)
	}
	if b := atomic.LoadInt32(builds); b != 1 {
		t.Fatalf("builds = %d, want 1", b)
	}

	// Change the addr → fingerprint changes → next Backend rebuilds, old closed.
	if err := r.store.SetSecretBackend(ctx, r.cipher, store.SecretBackendInput{
		Source: store.SecretSourceVault,
		Value:  map[string]any{"enabled": true, "addr": "http://b", "auth": "token"},
	}); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	if err := r.refresh(ctx); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if _, _, err := r.Backend(ctx, store.SecretSourceVault); err != nil {
		t.Fatalf("backend 2: %v", err)
	}
	if b := atomic.LoadInt32(builds); b != 2 {
		t.Fatalf("builds = %d, want 2 (rebuild on config change)", b)
	}
	if c := atomic.LoadInt32(closes); c != 1 {
		t.Fatalf("closes = %d, want 1 (old client released on rebuild)", c)
	}
}

func TestBackendConfigFromValue_VaultTLS(t *testing.T) {
	cfg := backendConfigFromValue(store.SecretSourceVault, map[string]any{
		"addr":                 "https://vault.example.com",
		"auth":                 "token",
		"ca_cert":              "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		"insecure_skip_verify": true,
	}, map[string]string{"token": "t"}, originDB)

	if cfg.vault.CACert == "" {
		t.Error("CACert not mapped from value.ca_cert")
	}
	if !cfg.vault.Insecure {
		t.Error("Insecure not mapped from value.insecure_skip_verify")
	}

	// Defaults: a config without the TLS keys stays secure (verify on, no CA).
	plain := backendConfigFromValue(store.SecretSourceVault, map[string]any{
		"addr": "https://vault.example.com", "auth": "token",
	}, map[string]string{"token": "t"}, originDB)
	if plain.vault.Insecure {
		t.Error("Insecure must default to false")
	}
	if plain.vault.CACert != "" {
		t.Error("CACert must default to empty")
	}
}

func TestRegistry_DBDisablesEnvBackend(t *testing.T) {
	r, _, _ := newRegistryWithFakeFactory(t, RegistryConfig{
		VaultEnabled: true,
		Vault:        external.VaultConfig{Addr: "http://vault", AuthMethod: "token", Token: "t"},
	})
	ctx := context.Background()
	if err := r.Prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// DB row disables the env-enabled backend.
	if err := r.store.SetSecretBackend(ctx, r.cipher, store.SecretBackendInput{
		Source: store.SecretSourceVault,
		Value:  map[string]any{"enabled": false},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := r.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := r.ConfiguredSources(); len(got) != 0 {
		t.Fatalf("ConfiguredSources = %v, want empty (DB disabled it)", got)
	}
	if _, _, err := r.Backend(ctx, store.SecretSourceVault); !errors.Is(err, ErrBackendNotConfigured) {
		t.Fatalf("err = %v, want ErrBackendNotConfigured", err)
	}
}

func TestRegistry_Prime_EnvFailFatal_DBFailNonFatal(t *testing.T) {
	pool := dbtest.SetupPool(t)
	st := store.New(pool)
	ctx := context.Background()

	// env-enabled backend whose factory errors → Prime is fatal.
	rEnv := NewSecretBackendRegistry(st, regCipher(t), RegistryConfig{
		AWSEnabled: true, AWS: external.AWSConfig{Region: "us-east-1"},
	}, nil)
	rEnv.factory = func(context.Context, backendConfig) (external.Backend, error) {
		return nil, errors.New("boom")
	}
	if err := rEnv.Prime(ctx); err == nil {
		t.Fatal("env-origin build failure must be fatal at Prime")
	}

	// DB-enabled backend whose factory errors → Prime tolerates it (warn).
	rDB := NewSecretBackendRegistry(st, regCipher(t), RegistryConfig{}, nil)
	rDB.factory = func(context.Context, backendConfig) (external.Backend, error) {
		return nil, errors.New("boom")
	}
	if err := rDB.store.SetSecretBackend(ctx, rDB.cipher, store.SecretBackendInput{
		Source: store.SecretSourceGCP,
		Value:  map[string]any{"enabled": true, "project": "p"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := rDB.Prime(ctx); err != nil {
		t.Fatalf("DB-origin build failure must NOT be fatal, got %v", err)
	}
}
