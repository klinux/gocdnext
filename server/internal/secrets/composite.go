package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/secrets/external"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// CompositeResolver implements Resolver for the reference model: it loads each
// declared name's entry in one store round-trip, then dispatches per source —
// `db` decrypts with the cipher (the DBResolver path), an external source
// fetches from the configured backend client by {ref_path, ref_key}. External
// lookups are cached (short TTL) so a fan-out of jobs on the same path hits the
// backend once. Unknown names and external not-found are silently omitted (the
// scheduler's diff reports them); a referenced-but-unconfigured backend, a
// db-without-cipher, or any decrypt/fetch error fails loud — citing the secret
// NAME, never a value.
// ErrBackendNotConfigured signals that a secret references an external source
// (vault/gcp/aws) that isn't enabled on this server. The resolver turns it
// into a loud, name-citing dispatch error (config drift, fail-closed) — never
// a silent omission.
var ErrBackendNotConfigured = errors.New("secrets: external backend not configured")

// backendProvider hands the resolver the live backend client for a source plus
// a fingerprint of the backend's config. The SecretBackendRegistry implements
// it (DB-configured, hot-reloaded); staticBackends wraps a fixed map for
// env-only deployments and tests. The fingerprint changes when the backend's
// connection config changes, so the resolver can fold it into the value-cache
// key — a config change can't serve a value cached from the old backend.
type backendProvider interface {
	Backend(ctx context.Context, source string) (external.Backend, string, error)
}

// staticBackends is a fixed source→client map (no hot-reload). Used for
// env-only wiring and tests; its fingerprint is constant.
type staticBackends map[string]external.Backend

func (s staticBackends) Backend(_ context.Context, source string) (external.Backend, string, error) {
	b, ok := s[source]
	if !ok {
		return nil, "", ErrBackendNotConfigured
	}
	return b, "static", nil
}

type CompositeResolver struct {
	store    *store.Store
	cipher   *crypto.Cipher // may be nil (pure-external deployment)
	backends backendProvider
	cache    *external.TTLCache
	timeout  time.Duration      // per-lookup deadline (0 disables)
	sf       singleflight.Group // dedupes concurrent misses on the same {source,path,key}
	log      *slog.Logger
}

// CompositeConfig wires the resolver. Supply EITHER Provider (the registry,
// for DB-configured hot-reloadable backends) OR Backends (a fixed map, for
// env-only/tests). Provider wins when both are set.
type CompositeConfig struct {
	Store    *store.Store
	Cipher   *crypto.Cipher
	Provider backendProvider
	Backends map[string]external.Backend
	Cache    *external.TTLCache
	// Timeout bounds a single external lookup so a hung Vault/AWS/GCP can't
	// pin a scheduler goroutine and stall dispatch of other jobs. 0 disables.
	Timeout time.Duration
	Log     *slog.Logger
}

// NewCompositeResolver validates dependencies.
func NewCompositeResolver(cfg CompositeConfig) (*CompositeResolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("secrets: composite resolver needs a store")
	}
	provider := cfg.Provider
	if provider == nil {
		if len(cfg.Backends) == 0 {
			return nil, errors.New("secrets: composite resolver needs a backend provider or at least one backend")
		}
		provider = staticBackends(cfg.Backends)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &CompositeResolver{
		store:    cfg.Store,
		cipher:   cfg.Cipher,
		backends: provider,
		cache:    cfg.Cache,
		timeout:  cfg.Timeout,
		log:      log,
	}, nil
}

// Resolve fulfils the Resolver contract.
func (r *CompositeResolver) Resolve(ctx context.Context, projectID uuid.UUID, names []string) (map[string]string, error) {
	entries, err := r.store.ResolveSecretEntries(ctx, projectID, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Source == store.SecretSourceDB {
			if r.cipher == nil {
				return nil, fmt.Errorf("secrets: %q is db-stored but no cipher is configured", e.Name)
			}
			plain, derr := r.cipher.Decrypt(e.ValueEnc)
			if derr != nil {
				return nil, fmt.Errorf("secrets: decrypt %q: %w", e.Name, derr)
			}
			out[e.Name] = string(plain)
			continue
		}

		backend, fingerprint, berr := r.backends.Backend(ctx, e.Source)
		if errors.Is(berr, ErrBackendNotConfigured) {
			return nil, fmt.Errorf("secrets: %q references backend %q which is not configured on this server", e.Name, e.Source)
		}
		if berr != nil {
			return nil, fmt.Errorf("secrets: backend %q for %q: %w", e.Source, e.Name, berr)
		}
		// Fold the backend fingerprint into the cache key: a config change
		// (new addr/project/region/creds) yields a new key, so a value cached
		// from the old backend is never served — hot-reload stays immediate.
		key := fingerprint + "\x00" + external.CacheKey(e.Source, e.RefPath, e.RefKey)
		if v, hit := r.cache.Get(key); hit {
			out[e.Name] = v
			continue
		}
		v, ferr := r.fetch(ctx, backend, e.RefPath, e.RefKey, key)
		if errors.Is(ferr, external.ErrSecretNotFound) {
			// Silent omit — the scheduler's diff turns this into a precise
			// "secrets not set on project" error.
			continue
		}
		if ferr != nil {
			return nil, fmt.Errorf("secrets: fetch %q from %s: %w", e.Name, e.Source, ferr)
		}
		out[e.Name] = v
	}
	return out, nil
}

// fetch resolves one external reference, deduplicating concurrent misses on
// the same cache key via singleflight (so a fan-out of N jobs on the same path
// hits the backend once, not N times) and bounding the backend call with the
// configured per-lookup timeout (so a hung backend can't stall dispatch). A
// successful value is cached; ErrSecretNotFound and errors are never cached.
//
// singleflight shares the leader's context: if the first caller's ctx is
// canceled the shared call fails for all waiters, but the per-lookup timeout
// caps the blast radius and a re-dispatch simply retries — acceptable for
// secret resolution, and the cost of dropping it is just a re-fetch.
func (r *CompositeResolver) fetch(ctx context.Context, backend external.Backend, path, refKey, key string) (string, error) {
	v, err, _ := r.sf.Do(key, func() (any, error) {
		// Another flight for the same key may have populated the cache while
		// we waited for the slot — re-check before hitting the backend.
		if cv, hit := r.cache.Get(key); hit {
			return cv, nil
		}
		fctx := ctx
		if r.timeout > 0 {
			var cancel context.CancelFunc
			fctx, cancel = context.WithTimeout(ctx, r.timeout)
			defer cancel()
		}
		fv, ferr := backend.Fetch(fctx, path, refKey)
		if ferr != nil {
			return "", ferr // includes ErrSecretNotFound — deliberately not cached
		}
		r.cache.Put(key, fv)
		return fv, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}
