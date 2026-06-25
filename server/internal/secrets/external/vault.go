package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	vault "github.com/hashicorp/vault/api"
)

// Vault auth methods. AppRole is the primary (role_id + secret_id);
// kubernetes (SA JWT → role) and a static token are also supported.
const (
	VaultAuthAppRole    = "approle"
	VaultAuthKubernetes = "kubernetes"
	VaultAuthToken      = "token"

	defaultK8sJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // well-known mount path, not a credential
)

// VaultConfig is the connection + auth config (server-level env).
type VaultConfig struct {
	Addr       string
	KVMount    string // default "secret"
	AuthMethod string // approle | kubernetes | token
	Role       string // kubernetes auth role
	RoleID     string // approle
	SecretID   string // approle
	Token      string // static token (dev)
	JWTPath    string // kubernetes SA token path
	Namespace  string // Vault Enterprise namespace
	CACert     string // PEM CA bundle to verify the server cert (private/internal CA)
	Insecure   bool   // skip TLS verification — explicit opt-in, logged loudly
}

// VaultBackend reads KV (v1 or v2 auto-handled) from HashiCorp Vault. The
// bearer token is held only inside the SDK client; on a 403 it re-authenticates
// once and retries (covers a token that lapsed between dispatches).
type VaultBackend struct {
	client *vault.Client
	mount  string
	cfg    VaultConfig
	mu     sync.Mutex // serialises re-auth
}

// NewVaultBackend dials Vault and authenticates once (fail-fast at boot).
func NewVaultBackend(ctx context.Context, cfg VaultConfig) (*VaultBackend, error) {
	if cfg.Addr == "" {
		return nil, errors.New("vault: addr is required")
	}
	vc, err := vaultClientConfig(cfg)
	if err != nil {
		return nil, err
	}
	client, err := vault.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("vault: new client: %w", err)
	}
	if cfg.Insecure {
		// Loud, explicit: TLS verification is off, so the server certificate
		// is NOT validated. Log the addr (never a credential) so an operator
		// can see exactly which backend is running insecure.
		slog.Warn("vault: TLS verification DISABLED (insecure_skip_verify) — the server certificate is not validated; prefer a ca_cert bundle",
			"addr", cfg.Addr)
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	// mount empty → full-path mode: the secret's `path` is the complete Vault
	// logical path (operator includes the engine mount + the KV v2 /data/
	// segment, as the Vault UI shows it). mount set → paths are relative to
	// it. No silent "secret" default — that prepended secret/data/ to an
	// already-complete path and 403'd operators whose engine is mounted
	// elsewhere (e.g. cora/data/...).
	b := &VaultBackend{client: client, mount: cfg.KVMount, cfg: cfg}
	if err := b.authenticate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// vaultClientConfig builds the SDK config and applies TLS settings when a CA
// bundle and/or insecure-skip-verify is configured. Split out of
// NewVaultBackend so the TLS wiring is unit-testable without a live server.
// An invalid CA PEM fails loud (no silent fall-through to system roots).
func vaultClientConfig(cfg VaultConfig) (*vault.Config, error) {
	vc := vault.DefaultConfig()
	vc.Address = cfg.Addr
	if cfg.Insecure || cfg.CACert != "" {
		tls := &vault.TLSConfig{Insecure: cfg.Insecure}
		if cfg.CACert != "" {
			tls.CACertBytes = []byte(cfg.CACert)
		}
		if err := vc.ConfigureTLS(tls); err != nil {
			return nil, fmt.Errorf("vault: configure tls: %w", err)
		}
	}
	return vc, nil
}

func (b *VaultBackend) Name() string { return "vault" }

// HealthCheck validates the backend is reachable and the current token is
// usable via auth/token/lookup-self (no KV permission needed). A 403 triggers
// the same re-auth-and-retry as Fetch, so a lapsed token self-heals.
func (b *VaultBackend) HealthCheck(ctx context.Context) error {
	_, err := b.client.Logical().ReadWithContext(ctx, "auth/token/lookup-self")
	if err != nil && isVaultForbidden(err) {
		b.mu.Lock()
		reauthErr := b.authenticate(ctx)
		b.mu.Unlock()
		if reauthErr == nil {
			_, err = b.client.Logical().ReadWithContext(ctx, "auth/token/lookup-self")
		}
	}
	if err != nil {
		return fmt.Errorf("vault: health check: %w", err)
	}
	return nil
}

func (b *VaultBackend) authenticate(ctx context.Context) error {
	// Trim surrounding whitespace on every credential before use. Vault IDs and
	// tokens never carry it, but a value pasted from a terminal or mounted from
	// a k8s Secret often has a trailing newline — Vault then rejects the login
	// as "invalid secret id" / "invalid token". Trimming at the point of use
	// covers all config sources (UI, env, DB) uniformly.
	switch b.cfg.AuthMethod {
	case VaultAuthToken:
		token := strings.TrimSpace(b.cfg.Token)
		if token == "" {
			return errors.New("vault: token auth needs a token")
		}
		b.client.SetToken(token)
		return nil
	case VaultAuthAppRole:
		roleID := strings.TrimSpace(b.cfg.RoleID)
		secretID := strings.TrimSpace(b.cfg.SecretID)
		if roleID == "" || secretID == "" {
			return errors.New("vault: approle auth needs role_id and secret_id")
		}
		return b.loginAndSet(ctx, "auth/approle/login", map[string]any{
			"role_id":   roleID,
			"secret_id": secretID,
		})
	case VaultAuthKubernetes:
		role := strings.TrimSpace(b.cfg.Role)
		if role == "" {
			return errors.New("vault: kubernetes auth needs a role")
		}
		jwtPath := b.cfg.JWTPath
		if jwtPath == "" {
			jwtPath = defaultK8sJWTPath
		}
		jwt, err := os.ReadFile(jwtPath)
		if err != nil {
			return fmt.Errorf("vault: kubernetes auth: read SA token: %w", err)
		}
		return b.loginAndSet(ctx, "auth/kubernetes/login", map[string]any{
			"role": role,
			"jwt":  strings.TrimSpace(string(jwt)),
		})
	default:
		return fmt.Errorf("vault: unknown auth method %q (want approle|kubernetes|token)", b.cfg.AuthMethod)
	}
}

func (b *VaultBackend) loginAndSet(ctx context.Context, path string, data map[string]any) error {
	resp, err := b.client.Logical().WriteWithContext(ctx, path, data)
	if err != nil {
		return fmt.Errorf("vault: login at %s: %w", path, err)
	}
	if resp == nil || resp.Auth == nil || resp.Auth.ClientToken == "" {
		return fmt.Errorf("vault: login at %s returned no token", path)
	}
	b.client.SetToken(resp.Auth.ClientToken)
	return nil
}

// Fetch returns the value at <path> under key. Vault KV stores key/value
// maps, so a key is required. KV v2 (the common default) is tried first,
// then v1 — no sys/ permission needed to detect the mount version.
func (b *VaultBackend) Fetch(ctx context.Context, path, key string) (string, error) {
	if key == "" {
		return "", errors.New("vault: ref_key is required (a Vault secret is a key/value map)")
	}
	val, err := b.read(ctx, path, key)
	if err != nil && isVaultForbidden(err) {
		// Token may have lapsed — re-auth once and retry.
		b.mu.Lock()
		reauthErr := b.authenticate(ctx)
		b.mu.Unlock()
		if reauthErr == nil {
			val, err = b.read(ctx, path, key)
		}
	}
	return val, err
}

func (b *VaultBackend) read(ctx context.Context, path, key string) (string, error) {
	// Full-path mode (mount unset): `path` is the complete Vault logical path;
	// detect KV v1/v2 from the response envelope instead of probing a /data/
	// prefix we can't assume the operator wants.
	if b.mount == "" {
		m, ok, err := b.readKVAuto(ctx, path)
		if err != nil {
			return "", err
		}
		if ok {
			return valueFromMap(m, key)
		}
		return "", ErrSecretNotFound
	}
	// KV v2: data lives under <mount>/data/<path>, value under .data.data.<key>.
	if m, ok, err := b.readKV(ctx, b.mount+"/data/"+path, true); err != nil {
		return "", err
	} else if ok {
		return valueFromMap(m, key)
	}
	// KV v1: <mount>/<path>, value under .data.<key>.
	if m, ok, err := b.readKV(ctx, b.mount+"/"+path, false); err != nil {
		return "", err
	} else if ok {
		return valueFromMap(m, key)
	}
	return "", ErrSecretNotFound
}

// readKV reads a logical path and returns the relevant data map. For v2 the
// payload is nested under .data; for v1 it's .data itself.
func (b *VaultBackend) readKV(ctx context.Context, logicalPath string, v2 bool) (map[string]any, bool, error) {
	resp, err := b.client.Logical().ReadWithContext(ctx, logicalPath)
	if err != nil {
		return nil, false, fmt.Errorf("vault: read %s: %w", logicalPath, err)
	}
	if resp == nil || resp.Data == nil {
		return nil, false, nil
	}
	if !v2 {
		return resp.Data, true, nil
	}
	inner, ok := resp.Data["data"].(map[string]any)
	if !ok {
		return nil, false, nil // not a v2 shape
	}
	return inner, true, nil
}

// readKVAuto reads a complete logical path (full-path mode) and detects KV
// v1 vs v2 from the response envelope: a v2 read returns
// {data:{...}, metadata:{...}}, so a nested `data` map sitting next to
// `metadata` means v2 (value under .data.data); otherwise it's v1 (value
// under .data). Requiring `metadata` too avoids misreading a v1 secret that
// happens to carry a key literally named "data".
func (b *VaultBackend) readKVAuto(ctx context.Context, logicalPath string) (map[string]any, bool, error) {
	resp, err := b.client.Logical().ReadWithContext(ctx, logicalPath)
	if err != nil {
		return nil, false, fmt.Errorf("vault: read %s: %w", logicalPath, err)
	}
	if resp == nil || resp.Data == nil {
		return nil, false, nil
	}
	if inner, ok := resp.Data["data"].(map[string]any); ok {
		if _, hasMeta := resp.Data["metadata"]; hasMeta {
			return inner, true, nil // KV v2 envelope
		}
	}
	return resp.Data, true, nil // KV v1 (flat)
}

func valueFromMap(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", ErrSecretNotFound
	}
	if s, isStr := v.(string); isStr {
		return s, nil
	}
	return fmt.Sprintf("%v", v), nil
}

func isVaultForbidden(err error) bool {
	var re *vault.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode == 403
	}
	return false
}
