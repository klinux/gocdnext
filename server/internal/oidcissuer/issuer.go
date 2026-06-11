// Package oidcissuer turns the gocdnext server into an OIDC identity
// provider for pipeline jobs: it mints short-lived RS256 JWTs whose
// claims describe the job (project, pipeline, ref, cause, …) and
// serves the discovery + JWKS endpoints cloud providers use to
// verify them. Operators federate these tokens via GCP Workload
// Identity, AWS IAM OIDC, Azure federated credentials or Vault's JWT
// auth — eliminating long-lived cloud secrets from CI.
//
// Trust model: this package only SIGNS. It never parses or verifies
// untrusted tokens, so no JWT library is imported (the verification
// side is where that CVE class lives). Correctness is proven by an
// interop test that verifies minted tokens with coreos/go-oidc — an
// independent, ecosystem-standard verifier.
package oidcissuer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

const (
	// keyCacheTTL bounds how long a replica signs with a cached key
	// (and serves a cached JWKS) before re-reading Postgres. Must
	// stay well under the rotation overlap — see RetirementOverlap
	// and the TestCacheWindows_FitInsideRotationOverlap guard.
	keyCacheTTL = 60 * time.Second
	// httpCacheMaxAge is the Cache-Control max-age on the discovery
	// + JWKS endpoints. Cloud verifiers cache aggressively anyway;
	// this just sets the contract. Same overlap constraint applies.
	httpCacheMaxAge = 300 * time.Second
)

// RetirementOverlap is how long a gracefully-retired key keeps
// being served in the JWKS after rotation: every token signed by it
// (TTL) plus the verifier-side caching slack (HTTP max-age) plus
// margin must still verify.
func RetirementOverlap(tokenTTL time.Duration) time.Duration {
	return tokenTTL + 2*httpCacheMaxAge
}

// KeySource abstracts the store so tests run without Postgres.
type KeySource interface {
	ActiveOIDCKey(ctx context.Context) (store.OIDCSigningKey, error)
	OIDCJWKSKeys(ctx context.Context, retiredCutoff time.Time) ([]store.OIDCPublicKey, error)
}

// KeyRotator is the write-side counterpart, satisfied by
// *store.Store. Separate from KeySource so read-only deployments of
// the issuer (tests, future verifier-only tools) don't carry a
// rotation dependency.
type KeyRotator interface {
	RotateOIDCKey(ctx context.Context, emergency bool) (store.OIDCSigningKey, error)
}

// Issuer mints job tokens and serves the OIDC discovery surface.
// Safe for concurrent use; the signing key and the JWKS document are
// cached in memory (keyCacheTTL) so the dispatch hot path and the
// public endpoints never touch Postgres per call.
type Issuer struct {
	issuerURL string
	keys      KeySource
	rotator   KeyRotator // optional; enables Rotate
	ttl       time.Duration
	now       func() time.Time
	log       *slog.Logger

	mu          sync.Mutex
	cachedKey   store.OIDCSigningKey
	keyFetched  time.Time
	cachedJWKS  []byte
	jwksFetched time.Time
	// gen increments on every cache invalidation / rotation swap.
	// Mint signs OUTSIDE the mutex (RSA ~1ms; serializing it would
	// gate dispatch throughput on crypto) and re-validates gen
	// afterwards: a signature that raced a rotation on THIS replica
	// is discarded and re-signed with the fresh key. See Mint for
	// the precise (local-strict, remote-on-NOTIFY) contract.
	gen uint64
}

// New validates + normalizes the issuer URL (trailing slash
// stripped — GCP/AWS compare `iss` byte-for-byte) and wires the key
// source. A non-HTTPS scheme is allowed but warned: every major
// cloud refuses http:// issuers, so it only makes sense in dev.
func New(issuerURL string, keys KeySource, tokenTTL time.Duration, log *slog.Logger) (*Issuer, error) {
	if log == nil {
		log = slog.Default()
	}
	u, err := url.Parse(issuerURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("oidcissuer: issuer URL %q must be absolute (https://host)", issuerURL)
	}
	if u.Scheme != "https" {
		log.Warn("oidc issuer URL is not https — cloud providers (GCP/AWS/Azure) reject non-https issuers",
			"issuer", issuerURL)
	}
	if tokenTTL <= 0 {
		return nil, fmt.Errorf("oidcissuer: token TTL must be positive, got %v", tokenTTL)
	}
	return &Issuer{
		issuerURL: strings.TrimRight(issuerURL, "/"),
		keys:      keys,
		ttl:       tokenTTL,
		now:       time.Now,
		log:       log,
	}, nil
}

// IssuerURL returns the normalized issuer (the `iss` claim value).
func (i *Issuer) IssuerURL() string { return i.issuerURL }

// WithRotator wires the write side so Rotate works. Returns the
// issuer for builder chaining.
func (i *Issuer) WithRotator(r KeyRotator) *Issuer {
	i.rotator = r
	return i
}

// InvalidateCaches drops the cached signing key and JWKS document
// so the next mint / JWKS request re-reads the store, and bumps the
// generation so any in-flight Mint discards a signature made with
// the stale key.
func (i *Issuer) InvalidateCaches() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.invalidateLocked()
}

// HandleRotationNotice is the NOTIFY-listener entry point. The
// payload carries the NEW active kid; when the cached SIGNING KEY
// already is that kid it reflects post-rotation reality and is
// preserved (gen included — in-flight signatures with the current
// key stay valid): this is what keeps the rotating replica's own
// pg_notify from undoing Rotate's priming, and spares any replica
// that converged by other means a useless refetch.
//
// The JWKS cache is ALWAYS dropped, even on a same-kid notice. The
// two caches age independently: a replica can hold the new signing
// key (TTL refetch) while still serving a pre-rotation JWKS — which
// would publish a key set missing the kid it is actively signing
// with, or, on emergency, still containing the revoked key, for up
// to keyCacheTTL (amplified by the verifier's max-age). Dropping
// the document costs one re-read on the next JWKS request.
// Unknown/empty kid → invalidate everything (fail safe).
func (i *Issuer) HandleRotationNotice(kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if kid != "" && i.cachedKey.Private != nil && i.cachedKey.Kid == kid {
		i.cachedJWKS = nil
		i.jwksFetched = time.Time{}
		return
	}
	i.invalidateLocked()
}

func (i *Issuer) invalidateLocked() {
	i.cachedKey = store.OIDCSigningKey{}
	i.keyFetched = time.Time{}
	i.cachedJWKS = nil
	i.jwksFetched = time.Time{}
	i.gen++
}

// Rotate retires (graceful) or revokes (emergency) the active key
// and swaps the issuer's caches ATOMICALLY with the DB commit: the
// mutex is held across both, so no Mint can read the cached key in
// the window between commit and cache swap — the race that would
// otherwise let a dispatch sign with a just-revoked key. The fresh
// key is primed into the cache, so post-rotation mints don't even
// pay a store round-trip.
func (i *Issuer) Rotate(ctx context.Context, emergency bool) (store.OIDCSigningKey, error) {
	if i.rotator == nil {
		return store.OIDCSigningKey{}, fmt.Errorf("oidcissuer: rotate: no rotator wired")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	fresh, err := i.rotator.RotateOIDCKey(ctx, emergency)
	if err != nil {
		// DB state unknown-bad → drop caches defensively; the next
		// mint re-reads whatever the store now holds.
		i.invalidateLocked()
		return store.OIDCSigningKey{}, err
	}
	i.invalidateLocked()
	i.cachedKey, i.keyFetched = fresh, i.now()
	return fresh, nil
}

// TokenTTL returns the configured token lifetime.
func (i *Issuer) TokenTTL() time.Duration { return i.ttl }

// mintRetries bounds the gen-revalidation loop. Two rotations
// landing inside a single ~1ms signature window is already absurd;
// three means something is rotating in a tight loop and failing
// loud beats spinning.
const mintRetries = 3

// Mint signs a job token for the given claims and audiences. Called
// on the scheduler dispatch path — one RSA-2048 signature (~1ms) per
// declared token; the signing key is cached so Postgres is not in
// the hot path.
//
// The signature happens OUTSIDE the mutex (RSA would otherwise
// serialize dispatch), guarded by generation re-validation: if a
// rotation (local Rotate or NOTIFY-driven invalidation) lands while
// we sign, the stale signature is discarded and re-signed with the
// fresh key.
//
// Consistency contract, stated precisely: on the replica WHERE the
// rotation runs, no token signed by the revoked key is returned
// after Rotate's commit (lock + gen gate make that strict). On
// OTHER replicas the guarantee is convergence, not atomicity: they
// keep minting with the old key until the NOTIFY arrives
// (typically single-digit ms; keyCacheTTL is the backstop if the
// listener is reconnecting). That window is accepted by design —
// closing it would need a DB version fence on every mint, putting
// Postgres back in the hot path; and it is dwarfed by the
// verifier-side JWKS cache (minutes) that bounds revocation anyway.
func (i *Issuer) Mint(ctx context.Context, jc JobClaims, aud []string) (string, error) {
	if len(aud) == 0 {
		return "", fmt.Errorf("oidcissuer: mint: at least one audience required")
	}
	for attempt := 0; attempt < mintRetries; attempt++ {
		key, gen, err := i.signingKey(ctx)
		if err != nil {
			return "", err
		}
		payload := buildPayload(i.issuerURL, jc, aud, i.now(), i.ttl, uuid.NewString())
		token, err := signRS256(key.Private, key.Kid, payload)
		if err != nil {
			return "", err
		}
		i.mu.Lock()
		stale := i.gen != gen
		i.mu.Unlock()
		if !stale {
			return token, nil
		}
		i.log.Info("oidc mint: key rotated mid-sign, re-signing", "stale_kid", key.Kid)
	}
	return "", fmt.Errorf("oidcissuer: mint: key rotated %d times during signing — rotation storm, refusing", mintRetries)
}

// MountWellKnown registers the public discovery endpoints. Both are
// unauthenticated by OIDC design and serve from the in-memory cache
// — no DB hit per request.
func (i *Issuer) MountWellKnown(r chi.Router) {
	r.Get("/.well-known/openid-configuration", i.handleDiscovery)
	r.Get("/.well-known/jwks.json", i.handleJWKS)
}

func (i *Issuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":   i.issuerURL,
		"jwks_uri": i.issuerURL + "/.well-known/jwks.json",
		// We are an ISSUER only — no authorization/token endpoints.
		// response_types + subject_types are the minimum verifiers
		// (go-oidc, GCP STS) require to accept the document.
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{
			"iss", "sub", "aud", "exp", "nbf", "iat", "jti",
			"project_slug", "project_id", "pipeline", "pipeline_id",
			"job", "matrix_key", "run_id", "run_counter",
			"ref", "ref_type", "sha", "cause", "pr_number",
		},
	}
	writeCachedJSON(w, doc)
}

func (i *Issuer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	body, err := i.jwksDocument(r.Context())
	if err != nil {
		i.log.Error("oidc jwks: key listing failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(httpCacheMaxAge.Seconds())))
	_, _ = w.Write(body)
}

// signingKey returns the active key plus the generation it belongs
// to, re-reading the store at most once per keyCacheTTL. Callers
// re-validate the generation after signing (see Mint) — a changed
// gen means a rotation landed mid-sign and the signature must be
// discarded.
func (i *Issuer) signingKey(ctx context.Context) (store.OIDCSigningKey, uint64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cachedKey.Private != nil && i.now().Sub(i.keyFetched) < keyCacheTTL {
		return i.cachedKey, i.gen, nil
	}
	key, err := i.keys.ActiveOIDCKey(ctx)
	if err != nil {
		return store.OIDCSigningKey{}, 0, fmt.Errorf("oidcissuer: active key: %w", err)
	}
	i.cachedKey, i.keyFetched = key, i.now()
	return key, i.gen, nil
}

// jwksDocument returns the marshaled JWKS, re-reading the store at
// most once per keyCacheTTL.
func (i *Issuer) jwksDocument(ctx context.Context) ([]byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cachedJWKS != nil && i.now().Sub(i.jwksFetched) < keyCacheTTL {
		return i.cachedJWKS, nil
	}
	cutoff := i.now().Add(-RetirementOverlap(i.ttl))
	keys, err := i.keys.OIDCJWKSKeys(ctx, cutoff)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(toJWKS(keys))
	if err != nil {
		return nil, fmt.Errorf("oidcissuer: marshal jwks: %w", err)
	}
	i.cachedJWKS, i.jwksFetched = body, i.now()
	return body, nil
}

func writeCachedJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(httpCacheMaxAge.Seconds())))
	_ = json.NewEncoder(w).Encode(v)
}
