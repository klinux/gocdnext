package external_test

import (
	"context"
	"errors"
	"testing"
	"time"

	vault "github.com/hashicorp/vault/api"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
)

// startVault boots a Vault dev-mode container (root token) and returns its
// address. Skips the test when Docker/the image is unavailable, matching the
// s3/localstack integration tests.
func startVault(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "hashicorp/vault:1.15",
			ExposedPorts: []string{"8200/tcp"},
			Env: map[string]string{
				"VAULT_DEV_ROOT_TOKEN_ID":  "root",
				"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
			},
			WaitingFor: wait.ForLog("Vault server started").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("vault container unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	host, _ := ctr.Host(ctx)
	port, _ := ctr.MappedPort(ctx, "8200/tcp")
	return "http://" + host + ":" + port.Port()
}

func TestVaultBackend_KVv2(t *testing.T) {
	addr := startVault(t)
	ctx := context.Background()

	// Seed a KV v2 secret via a raw client (the default dev `secret/` mount
	// is KV v2 → write under data/, value nested under .data.data).
	vc, err := vault.NewClient(&vault.Config{Address: addr})
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	vc.SetToken("root")
	if _, err := vc.Logical().WriteWithContext(ctx, "secret/data/myapp", map[string]any{
		"data": map[string]any{"PASSWORD": "s3cr3t-pw", "USER": "svc"},
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	b, err := external.NewVaultBackend(ctx, external.VaultConfig{
		Addr: addr, AuthMethod: external.VaultAuthToken, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("new vault backend: %v", err)
	}
	if b.Name() != "vault" {
		t.Fatalf("name = %q", b.Name())
	}

	// HealthCheck (Test-connection path) validates token + reachability.
	if err := b.HealthCheck(ctx); err != nil {
		t.Fatalf("health check: %v", err)
	}

	got, err := b.Fetch(ctx, "myapp", "PASSWORD")
	if err != nil || got != "s3cr3t-pw" {
		t.Fatalf("fetch PASSWORD = %q, %v", got, err)
	}

	// Missing key → not found (silent omit upstream).
	if _, err := b.Fetch(ctx, "myapp", "ABSENT"); !errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("missing key err = %v, want ErrSecretNotFound", err)
	}
	// Missing path → not found.
	if _, err := b.Fetch(ctx, "no/such", "k"); !errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("missing path err = %v, want ErrSecretNotFound", err)
	}
	// Empty key → loud error (a Vault secret is a key/value map).
	if _, err := b.Fetch(ctx, "myapp", ""); err == nil {
		t.Fatal("empty key should be rejected")
	}

	// Full-path mode: an empty KVMount means the ref path is the COMPLETE
	// Vault logical path (engine mount + the KV v2 /data/ segment included,
	// as the Vault UI shows it). v1/v2 is detected from the response, so the
	// same v2 secret resolves via its full path — no silent secret/data/
	// prefix that would have produced secret/data/secret/data/myapp.
	fp, err := external.NewVaultBackend(ctx, external.VaultConfig{
		Addr: addr, AuthMethod: external.VaultAuthToken, Token: "root", // KVMount empty
	})
	if err != nil {
		t.Fatalf("new full-path backend: %v", err)
	}
	if got, err := fp.Fetch(ctx, "secret/data/myapp", "PASSWORD"); err != nil || got != "s3cr3t-pw" {
		t.Fatalf("full-path fetch = %q, %v (want s3cr3t-pw)", got, err)
	}
	// A bogus full path → not found (not a 500).
	if _, err := fp.Fetch(ctx, "secret/data/nope", "PASSWORD"); !errors.Is(err, external.ErrSecretNotFound) {
		t.Fatalf("full-path missing = %v, want ErrSecretNotFound", err)
	}
}

func TestVaultBackend_AppRoleLogin(t *testing.T) {
	addr := startVault(t)
	ctx := context.Background()

	vc, _ := vault.NewClient(&vault.Config{Address: addr})
	vc.SetToken("root")
	// Enable approle, grant a KV-read policy, create a role bound to it,
	// then read role_id + a fresh secret_id.
	if _, err := vc.Logical().WriteWithContext(ctx, "sys/auth/approle", map[string]any{"type": "approle"}); err != nil {
		t.Fatalf("enable approle: %v", err)
	}
	if _, err := vc.Logical().WriteWithContext(ctx, "sys/policies/acl/ci-read", map[string]any{
		"policy": `path "secret/data/*" { capabilities = ["read"] }`,
	}); err != nil {
		t.Fatalf("put policy: %v", err)
	}
	if _, err := vc.Logical().WriteWithContext(ctx, "auth/approle/role/ci", map[string]any{
		"token_policies": "ci-read", "secret_id_ttl": "10m", "token_ttl": "10m",
	}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	roleResp, err := vc.Logical().ReadWithContext(ctx, "auth/approle/role/ci/role-id")
	if err != nil {
		t.Fatalf("read role-id: %v", err)
	}
	sidResp, err := vc.Logical().WriteWithContext(ctx, "auth/approle/role/ci/secret-id", nil)
	if err != nil {
		t.Fatalf("gen secret-id: %v", err)
	}
	roleID, _ := roleResp.Data["role_id"].(string)
	secretID, _ := sidResp.Data["secret_id"].(string)
	if roleID == "" || secretID == "" {
		t.Fatalf("approle creds empty: role=%q secret=%q", roleID, secretID)
	}
	if _, err := vc.Logical().WriteWithContext(ctx, "secret/data/app2", map[string]any{
		"data": map[string]any{"TOKEN": "approle-value"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The backend authenticates via AppRole (the configured primary).
	b, err := external.NewVaultBackend(ctx, external.VaultConfig{
		Addr: addr, AuthMethod: external.VaultAuthAppRole, RoleID: roleID, SecretID: secretID, KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("approle backend: %v", err)
	}
	got, err := b.Fetch(ctx, "app2", "TOKEN")
	if err != nil || got != "approle-value" {
		t.Fatalf("fetch via approle = %q, %v", got, err)
	}
}
