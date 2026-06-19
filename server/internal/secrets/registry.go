package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// registrySources is the fixed set of external backend kinds, in display order.
var registrySources = []string{store.SecretSourceVault, store.SecretSourceGCP, store.SecretSourceAWS}

const (
	originEnv = "env"
	originDB  = "db"

	defaultRegistryTTL = 30 * time.Second // backstop reload (a missed NOTIFY converges within this)
	refreshTimeout     = 10 * time.Second
)

// RegistryConfig is the env-derived baseline. The DB (platform_settings)
// overlays it per backend; deleting a DB row falls back to these.
type RegistryConfig struct {
	VaultEnabled bool
	Vault        external.VaultConfig
	GCPEnabled   bool
	GCP          external.GCPConfig
	AWSEnabled   bool
	AWS          external.AWSConfig
	// TTL is the backstop reload interval; <=0 uses the default.
	TTL time.Duration
}

// backendConfig is the merged (env+DB) desired state for one source.
type backendConfig struct {
	source  string
	enabled bool
	origin  string // "env" | "db" — env failures are fatal at boot, DB failures aren't
	vault   external.VaultConfig
	gcp     external.GCPConfig
	aws     external.AWSConfig
}

// fingerprint is a stable hash of the connection-affecting config (secrets
// included, so rotating a secret_id forces a client rebuild). Safe to hold/log
// — it's a digest, never the values.
func (c backendConfig) fingerprint() string {
	var payload any
	switch c.source {
	case store.SecretSourceVault:
		payload = c.vault
	case store.SecretSourceGCP:
		payload = c.gcp
	case store.SecretSourceAWS:
		payload = c.aws
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// backendFactory builds a live client for a merged config. Injectable so unit
// tests run without a real Vault/GCP/AWS.
type backendFactory func(ctx context.Context, cfg backendConfig) (external.Backend, error)

func defaultFactory(ctx context.Context, cfg backendConfig) (external.Backend, error) {
	switch cfg.source {
	case store.SecretSourceVault:
		return external.NewVaultBackend(ctx, cfg.vault)
	case store.SecretSourceGCP:
		return external.NewGCPBackend(ctx, cfg.gcp)
	case store.SecretSourceAWS:
		return external.NewAWSBackend(ctx, cfg.aws)
	}
	return nil, fmt.Errorf("secrets: unknown backend source %q", cfg.source)
}

type clientEntry struct {
	fingerprint string
	backend     external.Backend
}

// SecretBackendRegistry is the single source of truth for which external
// backends are enabled and their live clients. Config is the env baseline
// overlaid by platform_settings rows (keys "secrets.<source>"); changes
// hot-reload via Invalidate (fed by the LISTEN/NOTIFY listener) with a TTL
// backstop. Clients are cached by config fingerprint and rebuilt only when the
// config changes. Implements the composite resolver's backendProvider.
type SecretBackendRegistry struct {
	store    *store.Store
	cipher   *crypto.Cipher
	baseline RegistryConfig
	factory  backendFactory
	log      *slog.Logger
	ttl      time.Duration

	mu      sync.RWMutex
	desired map[string]backendConfig
	clients map[string]*clientEntry

	dirty    atomic.Bool
	reloadCh chan string
	sf       singleflight.Group
}

// NewSecretBackendRegistry builds the registry (no I/O yet — call Prime to load
// + eagerly build, then Run for the refresh loop).
func NewSecretBackendRegistry(st *store.Store, cipher *crypto.Cipher, baseline RegistryConfig, log *slog.Logger) *SecretBackendRegistry {
	if log == nil {
		log = slog.Default()
	}
	ttl := baseline.TTL
	if ttl <= 0 {
		ttl = defaultRegistryTTL
	}
	return &SecretBackendRegistry{
		store:    st,
		cipher:   cipher,
		baseline: baseline,
		factory:  defaultFactory,
		log:      log,
		ttl:      ttl,
		desired:  map[string]backendConfig{},
		clients:  map[string]*clientEntry{},
		reloadCh: make(chan string, 1),
	}
}

// loadDesired computes the merged env+DB config for every source.
func (r *SecretBackendRegistry) loadDesired(ctx context.Context) (map[string]backendConfig, error) {
	out := map[string]backendConfig{
		store.SecretSourceVault: {source: store.SecretSourceVault, origin: originEnv, enabled: r.baseline.VaultEnabled, vault: r.baseline.Vault},
		store.SecretSourceGCP:   {source: store.SecretSourceGCP, origin: originEnv, enabled: r.baseline.GCPEnabled, gcp: r.baseline.GCP},
		store.SecretSourceAWS:   {source: store.SecretSourceAWS, origin: originEnv, enabled: r.baseline.AWSEnabled, aws: r.baseline.AWS},
	}
	for _, src := range registrySources {
		row, err := r.store.GetSecretBackend(ctx, src)
		if errors.Is(err, store.ErrPlatformSettingNotFound) {
			continue // keep env baseline
		}
		if err != nil {
			return nil, err
		}
		cfg, err := r.mergeDBRow(src, row)
		if err != nil {
			return nil, err
		}
		out[src] = cfg
	}
	return out, nil
}

// mergeDBRow overlays a platform_settings row onto a source config. The DB row
// is authoritative when present (including the enabled flag).
func (r *SecretBackendRegistry) mergeDBRow(source string, row store.PlatformSetting) (backendConfig, error) {
	creds, err := store.DecryptPlatformCredentials(r.cipher, row.CredentialsEnc)
	if err != nil {
		return backendConfig{}, err
	}
	return backendConfigFromValue(source, row.Value, creds, originDB), nil
}

// backendConfigFromValue maps a platform_settings `value` map + decrypted creds
// into a typed config. Shared by mergeDBRow and TestSecretBackend so the field
// contract lives in one place.
func backendConfigFromValue(source string, value map[string]any, creds map[string]string, origin string) backendConfig {
	c := backendConfig{source: source, origin: origin, enabled: asBool(value["enabled"])}
	switch source {
	case store.SecretSourceVault:
		c.vault = external.VaultConfig{
			Addr:       asString(value["addr"]),
			KVMount:    asString(value["kv_mount"]),
			AuthMethod: asString(value["auth"]),
			Role:       asString(value["role"]),
			RoleID:     asString(value["role_id"]),
			JWTPath:    asString(value["jwt_path"]),
			Namespace:  asString(value["namespace"]),
			SecretID:   creds["secret_id"],
			Token:      creds["token"],
		}
	case store.SecretSourceGCP:
		c.gcp = external.GCPConfig{Project: asString(value["project"])}
	case store.SecretSourceAWS:
		c.aws = external.AWSConfig{Region: asString(value["region"]), Endpoint: asString(value["endpoint"])}
	}
	return c
}

// TestSecretBackend builds a transient client from the given (non-secret value
// + plaintext creds) and runs a health check — the admin "Test connection"
// path. The client is closed before return; no secret value is fetched.
func TestSecretBackend(ctx context.Context, source string, value map[string]any, creds map[string]string) error {
	cfg := backendConfigFromValue(source, value, creds, originDB)
	be, err := defaultFactory(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeBackend(be)
	return be.HealthCheck(ctx)
}

// refresh reloads the desired map and clears the dirty flag. Client builds stay
// lazy (Backend builds on first use after a fingerprint change).
func (r *SecretBackendRegistry) refresh(ctx context.Context) error {
	desired, err := r.loadDesired(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.desired = desired
	r.mu.Unlock()
	r.dirty.Store(false)
	return nil
}

// Prime does the initial synchronous load + eager build. An env-configured
// backend that fails to build is fatal (preserves the v0.45.0 boot contract);
// a DB-configured one is logged and left to retry on demand (a bad UI save
// must not crash the server).
func (r *SecretBackendRegistry) Prime(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
		return fmt.Errorf("secrets: load backend config: %w", err)
	}
	r.mu.RLock()
	enabled := make([]backendConfig, 0, len(r.desired))
	for _, src := range registrySources {
		if c := r.desired[src]; c.enabled {
			enabled = append(enabled, c)
		}
	}
	r.mu.RUnlock()
	for _, c := range enabled {
		if _, _, err := r.Backend(ctx, c.source); err != nil {
			if c.origin == originEnv {
				return fmt.Errorf("secrets: backend %q (env): %w", c.source, err)
			}
			r.log.Warn("secret backend (db) failed to initialise; will retry on demand", "source", c.source, "err", err)
		}
	}
	return nil
}

// Run is the refresh loop: reloads on Invalidate (NOTIFY) and on a TTL tick
// (backstop). Blocks until ctx cancels.
func (r *SecretBackendRegistry) Run(ctx context.Context) {
	t := time.NewTicker(r.ttl)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tryRefresh(ctx)
		case <-r.reloadCh:
			r.tryRefresh(ctx)
		}
	}
}

func (r *SecretBackendRegistry) tryRefresh(ctx context.Context) {
	rc, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := r.refresh(rc); err != nil {
		// Keep the last good config — a transient DB blip must not disable
		// working backends.
		r.log.Warn("secret backend registry: refresh failed; keeping last config", "err", err)
	}
}

// Invalidate requests an immediate reload (coalesced). Called by the listener
// on a NOTIFY and by the admin write path locally.
func (r *SecretBackendRegistry) Invalidate(source string) {
	r.dirty.Store(true)
	select {
	case r.reloadCh <- source:
	default: // a reload is already pending — coalesce
	}
}

// HandleNotice adapts the registry to the LISTEN/NOTIFY listener.
func (r *SecretBackendRegistry) HandleNotice(payload string) { r.Invalidate(payload) }

// Backend returns the live client for a source plus its config fingerprint,
// building (and caching) the client on first use or after a config change.
// ErrBackendNotConfigured when the source is disabled — the composite resolver
// turns that into a loud, name-citing dispatch error. The fingerprint lets the
// resolver key its value cache so a config change can't serve a stale value.
// Implements backendProvider.
func (r *SecretBackendRegistry) Backend(ctx context.Context, source string) (external.Backend, string, error) {
	r.mu.RLock()
	cfg, ok := r.desired[source]
	ce := r.clients[source]
	r.mu.RUnlock()
	if !ok || !cfg.enabled {
		return nil, "", ErrBackendNotConfigured
	}
	fp := cfg.fingerprint()
	if ce != nil && ce.fingerprint == fp {
		return ce.backend, fp, nil
	}
	v, err, _ := r.sf.Do(source+"\x00"+fp, func() (any, error) {
		// Another flight may have built this exact config while we waited.
		r.mu.RLock()
		cur := r.clients[source]
		r.mu.RUnlock()
		if cur != nil && cur.fingerprint == fp {
			return cur.backend, nil
		}
		be, ferr := r.factory(ctx, cfg)
		if ferr != nil {
			return nil, ferr
		}
		r.mu.Lock()
		if old := r.clients[source]; old != nil {
			closeBackend(old.backend)
		}
		r.clients[source] = &clientEntry{fingerprint: fp, backend: be}
		r.mu.Unlock()
		return be, nil
	})
	if err != nil {
		return nil, "", err
	}
	return v.(external.Backend), fp, nil
}

// ConfiguredSources is the enabled set, for the API's configured_sources.
// Cheap cached read (no I/O) — the Run loop keeps the snapshot fresh.
func (r *SecretBackendRegistry) ConfiguredSources() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(registrySources))
	for _, src := range registrySources {
		if c, ok := r.desired[src]; ok && c.enabled {
			out = append(out, src)
		}
	}
	return out
}

// closeBackend releases a replaced client when it holds closeable resources
// (the GCP gRPC client); Vault/AWS are no-ops.
func closeBackend(b external.Backend) {
	if c, ok := b.(io.Closer); ok {
		_ = c.Close()
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
