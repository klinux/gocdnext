package external

import (
	"context"
	"errors"
	"fmt"
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

	defaultVaultMount   = "secret"
	defaultK8sJWTPath    = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // well-known mount path, not a credential
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
	vc := vault.DefaultConfig()
	vc.Address = cfg.Addr
	client, err := vault.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("vault: new client: %w", err)
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	mount := cfg.KVMount
	if mount == "" {
		mount = defaultVaultMount
	}
	b := &VaultBackend{client: client, mount: mount, cfg: cfg}
	if err := b.authenticate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *VaultBackend) Name() string { return "vault" }

func (b *VaultBackend) authenticate(ctx context.Context) error {
	switch b.cfg.AuthMethod {
	case VaultAuthToken:
		if b.cfg.Token == "" {
			return errors.New("vault: token auth needs a token")
		}
		b.client.SetToken(b.cfg.Token)
		return nil
	case VaultAuthAppRole:
		if b.cfg.RoleID == "" || b.cfg.SecretID == "" {
			return errors.New("vault: approle auth needs role_id and secret_id")
		}
		return b.loginAndSet(ctx, "auth/approle/login", map[string]any{
			"role_id":   b.cfg.RoleID,
			"secret_id": b.cfg.SecretID,
		})
	case VaultAuthKubernetes:
		if b.cfg.Role == "" {
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
			"role": b.cfg.Role,
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
