package oidcissuer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// fakeKeySource implements KeySource in memory and counts calls so
// cache behaviour is assertable.
type fakeKeySource struct {
	key     store.OIDCSigningKey
	jwks    []store.OIDCPublicKey
	activeN atomic.Int64
	jwksN   atomic.Int64
}

func (f *fakeKeySource) ActiveOIDCKey(ctx context.Context) (store.OIDCSigningKey, error) {
	f.activeN.Add(1)
	return f.key, nil
}

func (f *fakeKeySource) OIDCJWKSKeys(ctx context.Context, cutoff time.Time) ([]store.OIDCPublicKey, error) {
	f.jwksN.Add(1)
	return f.jwks, nil
}

func newFakeKeySource(t *testing.T) *fakeKeySource {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	kid, err := store.JWKThumbprint(&priv.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	return &fakeKeySource{
		key:  store.OIDCSigningKey{Kid: kid, Alg: "RS256", Private: priv},
		jwks: []store.OIDCPublicKey{{Kid: kid, Alg: "RS256", Public: &priv.PublicKey}},
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestNew_NormalizesIssuerURL — trailing slash stripped (GCP/AWS
// compare iss byte-for-byte against the configured issuer), scheme
// validated.
func TestNew_NormalizesIssuerURL(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, err := New("https://ci.example.com/", ks, time.Hour, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if iss.IssuerURL() != "https://ci.example.com" {
		t.Errorf("issuer = %q, want trailing slash stripped", iss.IssuerURL())
	}

	if _, err := New("ci.example.com", ks, time.Hour, discard()); err == nil {
		t.Errorf("schemeless URL must be rejected")
	}
	if _, err := New("", ks, time.Hour, discard()); err == nil {
		t.Errorf("empty URL must be rejected")
	}
}

// TestMint_TokenRoundtrip — minted token carries the issuer URL,
// requested aud, and a fresh jti per call.
func TestMint_TokenRoundtrip(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, err := New("https://ci.example.com", ks, time.Hour, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	jc := JobClaims{ProjectSlug: "s", Pipeline: "p", Job: "j", RunID: "r", RunCounter: "1", Cause: "webhook", RefType: "branch", Ref: "main"}

	tok1, err := iss.Mint(context.Background(), jc, []string{"aud-x"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	tok2, err := iss.Mint(context.Background(), jc, []string{"aud-x"})
	if err != nil {
		t.Fatalf("mint 2: %v", err)
	}

	payload := decodePayload(t, tok1)
	if payload["iss"] != "https://ci.example.com" {
		t.Errorf("iss = %v", payload["iss"])
	}
	if payload["aud"] != "aud-x" {
		t.Errorf("aud = %v", payload["aud"])
	}
	if payload["jti"] == decodePayload(t, tok2)["jti"] {
		t.Errorf("jti must be unique per mint")
	}
}

// TestMint_RequiresAudience — empty aud list is a programming error
// upstream (parser enforces it); the issuer double-checks.
func TestMint_RequiresAudience(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, _ := New("https://ci.example.com", ks, time.Hour, discard())
	if _, err := iss.Mint(context.Background(), JobClaims{}, nil); err == nil {
		t.Fatalf("expected error on empty aud")
	}
}

// TestMint_CachesSigningKey — N mints within the cache TTL hit the
// KeySource once. Performance contract: dispatch hot path must not
// query Postgres per job.
func TestMint_CachesSigningKey(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, _ := New("https://ci.example.com", ks, time.Hour, discard())
	for i := 0; i < 10; i++ {
		if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if got := ks.activeN.Load(); got != 1 {
		t.Errorf("KeySource.ActiveOIDCKey called %d times for 10 mints, want 1 (cache)", got)
	}
}

// TestWellKnown_DiscoveryShape — the discovery doc fields verifiers
// actually read, plus cache headers.
func TestWellKnown_DiscoveryShape(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, _ := New("https://ci.example.com", ks, time.Hour, discard())
	r := chi.NewRouter()
	iss.MountWellKnown(r)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
		t.Errorf("Cache-Control = %q, want max-age=300", cc)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["issuer"] != "https://ci.example.com" {
		t.Errorf("issuer = %v", doc["issuer"])
	}
	if doc["jwks_uri"] != "https://ci.example.com/.well-known/jwks.json" {
		t.Errorf("jwks_uri = %v", doc["jwks_uri"])
	}
	algs, _ := doc["id_token_signing_alg_values_supported"].([]any)
	if len(algs) != 1 || algs[0] != "RS256" {
		t.Errorf("algs = %v, want exactly [RS256] (alg-confusion hardening)", doc["id_token_signing_alg_values_supported"])
	}
}

// TestWellKnown_JWKSShapeAndCache — JWK fields per RFC 7517, n/e
// unpadded base64url, and the in-memory cache keeps repeat requests
// off the KeySource.
func TestWellKnown_JWKSShapeAndCache(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, _ := New("https://ci.example.com", ks, time.Hour, discard())
	r := chi.NewRouter()
	iss.MountWellKnown(r)

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if i > 0 {
			continue
		}
		var jwks struct {
			Keys []map[string]string `json:"keys"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(jwks.Keys) != 1 {
			t.Fatalf("keys = %d, want 1", len(jwks.Keys))
		}
		k := jwks.Keys[0]
		if k["kty"] != "RSA" || k["use"] != "sig" || k["alg"] != "RS256" {
			t.Errorf("jwk = %v", k)
		}
		if k["kid"] != ks.key.Kid {
			t.Errorf("kid = %q, want %q", k["kid"], ks.key.Kid)
		}
		for _, f := range []string{"n", "e"} {
			if strings.ContainsAny(k[f], "+/=") {
				t.Errorf("%s contains non-base64url chars: %q", f, k[f])
			}
			if _, err := base64.RawURLEncoding.DecodeString(k[f]); err != nil {
				t.Errorf("%s not raw base64url: %v", f, err)
			}
		}
	}
	if got := ks.jwksN.Load(); got != 1 {
		t.Errorf("KeySource.OIDCJWKSKeys called %d times for 5 requests, want 1 (cache)", got)
	}
}

// TestInvalidateCaches_ForcesRefetch — after invalidation, the very
// next mint AND the very next JWKS request re-read the KeySource.
// This is the local half of the rotation contract: the replica
// handling an emergency rotate must not sign with the revoked key
// even once more.
func TestInvalidateCaches_ForcesRefetch(t *testing.T) {
	ks := newFakeKeySource(t)
	iss, _ := New("https://ci.example.com", ks, time.Hour, discard())
	r := chi.NewRouter()
	iss.MountWellKnown(r)

	if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if ks.activeN.Load() != 1 || ks.jwksN.Load() != 1 {
		t.Fatalf("pre-invalidation fetches = %d/%d, want 1/1", ks.activeN.Load(), ks.jwksN.Load())
	}

	iss.InvalidateCaches()

	if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
		t.Fatalf("mint post-invalidate: %v", err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if ks.activeN.Load() != 2 {
		t.Errorf("signing-key fetches = %d, want 2 (cache must be dropped)", ks.activeN.Load())
	}
	if ks.jwksN.Load() != 2 {
		t.Errorf("jwks fetches = %d, want 2 (cache must be dropped)", ks.jwksN.Load())
	}
}

// rotatingKeySource extends the fake with a Rotate that swaps the
// active key — the issuer-side contract test for atomic rotation.
type rotatingKeySource struct {
	*fakeKeySource
	t *testing.T
}

func (r *rotatingKeySource) RotateOIDCKey(ctx context.Context, emergency bool) (store.OIDCSigningKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		r.t.Fatalf("generate: %v", err)
	}
	kid, err := store.JWKThumbprint(&priv.PublicKey)
	if err != nil {
		r.t.Fatalf("thumbprint: %v", err)
	}
	r.key = store.OIDCSigningKey{Kid: kid, Alg: "RS256", Private: priv}
	r.jwks = []store.OIDCPublicKey{{Kid: kid, Alg: "RS256", Public: &priv.PublicKey}}
	return r.key, nil
}

// TestRotate_AtomicSwap — Rotate must (1) return the fresh key,
// (2) prime the signing cache with it so the next Mint uses the new
// kid WITHOUT a store round-trip, and (3) leave zero window where a
// mint could observe the revoked key after Rotate returned. The
// deterministic observable: every Mint issued after Rotate returns
// carries the new kid. Review round 2 MEDIUM.
func TestRotate_AtomicSwap(t *testing.T) {
	ks := &rotatingKeySource{fakeKeySource: newFakeKeySource(t), t: t}
	iss, err := New("https://ci.example.com", ks.fakeKeySource, time.Hour, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	iss = iss.WithRotator(ks)

	tok, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	oldKid := tokenKid(t, tok)

	// Flood mints concurrently with an emergency rotation; -race
	// covers memory safety, the post-rotation assertion covers the
	// protocol.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for j := 0; j < 50; j++ {
			if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
				t.Errorf("concurrent mint: %v", err)
				return
			}
		}
	}()

	fresh, err := iss.Rotate(context.Background(), true)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh.Kid == oldKid {
		t.Fatalf("rotate returned the old kid")
	}

	fetchesAfterRotate := ks.activeN.Load()
	tok2, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"})
	if err != nil {
		t.Fatalf("mint post-rotate: %v", err)
	}
	if got := tokenKid(t, tok2); got != fresh.Kid {
		t.Fatalf("post-rotate mint kid = %q, want %q — signed with a revoked key", got, fresh.Kid)
	}
	if ks.activeN.Load() != fetchesAfterRotate {
		t.Errorf("post-rotate mint hit the KeySource — Rotate must prime the cache with the fresh key")
	}
	<-done
}

func tokenKid(t *testing.T, token string) string {
	t.Helper()
	headRaw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var head struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headRaw, &head); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	return head.Kid
}

// TestHandleRotationNotice_IdempotentByKid — the rotating replica
// hears its own pg_notify; when the cached key already IS the
// notified kid, the notice must NOT dump the cache Rotate just
// primed (review round 3). A notice for a DIFFERENT kid must
// invalidate.
func TestHandleRotationNotice_IdempotentByKid(t *testing.T) {
	ks := &rotatingKeySource{fakeKeySource: newFakeKeySource(t), t: t}
	iss, err := New("https://ci.example.com", ks.fakeKeySource, time.Hour, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	iss = iss.WithRotator(ks)

	fresh, err := iss.Rotate(context.Background(), false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	fetches := ks.activeN.Load()

	// Cache a JWKS BEFORE the notice arrives — this is the replica
	// whose key cache converged but whose JWKS document predates
	// the rotation from its point of view.
	r := chi.NewRouter()
	iss.MountWellKnown(r)
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	jwksFetches := ks.jwksN.Load()

	// Self-notice: signing key already holds fresh.Kid → key cache
	// preserved (no mint refetch)…
	iss.HandleRotationNotice(fresh.Kid)
	if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ks.activeN.Load() != fetches {
		t.Errorf("self-notice dumped the primed signing key (fetches %d → %d)", fetches, ks.activeN.Load())
	}
	// …but the JWKS document MUST be re-read: the caches age
	// independently, and a preserved pre-rotation JWKS would
	// publish a key set inconsistent with the kid we sign with
	// (or still contain a revoked key). Review round 4.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if ks.jwksN.Load() != jwksFetches+1 {
		t.Errorf("same-kid notice preserved a stale JWKS document (fetches = %d, want %d)", ks.jwksN.Load(), jwksFetches+1)
	}

	// Foreign notice (another replica rotated to a kid we don't
	// hold) → must invalidate and refetch on next mint.
	iss.HandleRotationNotice("some-other-kid")
	if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ks.activeN.Load() != fetches+1 {
		t.Errorf("foreign notice did not invalidate (fetches = %d, want %d)", ks.activeN.Load(), fetches+1)
	}

	// Empty payload → fail safe: invalidate.
	iss.HandleRotationNotice("")
	if _, err := iss.Mint(context.Background(), JobClaims{Cause: "webhook"}, []string{"a"}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ks.activeN.Load() != fetches+2 {
		t.Errorf("empty-kid notice did not invalidate (fetches = %d, want %d)", ks.activeN.Load(), fetches+2)
	}
}

// TestCacheWindows_FitInsideRotationOverlap — the static safety
// relationship: in-memory cache + HTTP max-age must stay well below
// the graceful-rotation overlap, or a verifier could be told about
// a key set that no longer verifies in-flight tokens. Asserted so a
// future constant change can't silently break rotation.
func TestCacheWindows_FitInsideRotationOverlap(t *testing.T) {
	minOverlap := RetirementOverlap(5 * time.Minute) // smallest configurable TTL
	if keyCacheTTL+httpCacheMaxAge >= minOverlap {
		t.Fatalf("cache windows (%v + %v) must be < min rotation overlap (%v)",
			keyCacheTTL, httpCacheMaxAge, minOverlap)
	}
}

// TestInterop_GoOIDCVerifier — the load-bearing test: a REAL OIDC
// verifier (coreos/go-oidc, the library class Vault and much of the
// ecosystem use) discovers our issuer over HTTP, fetches our JWKS,
// and accepts a token we minted. If this passes, the cryptographic
// path is correct end-to-end.
func TestInterop_GoOIDCVerifier(t *testing.T) {
	ks := newFakeKeySource(t)

	// The issuer URL must match the httptest server URL — build the
	// server first with a placeholder mux, then the issuer.
	mux := chi.NewRouter()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	iss, err := New(srv.URL, ks, time.Hour, discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	iss.MountWellKnown(mux)

	jc := JobClaims{
		ProjectSlug: "shop", ProjectID: "pid", Pipeline: "ci", PipelineID: "plid",
		Job: "deploy", RunID: "rid", RunCounter: "1",
		Ref: "main", RefType: "branch", SHA: "abc", Cause: "webhook",
	}
	token, err := iss.Mint(context.Background(), jc, []string{"my-audience"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, srv.URL)
	if err != nil {
		t.Fatalf("go-oidc discovery failed: %v", err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: "my-audience"})
	idToken, err := verifier.Verify(ctx, token)
	if err != nil {
		t.Fatalf("go-oidc rejected our token: %v", err)
	}
	if idToken.Subject != jc.Subject() {
		t.Errorf("verified sub = %q, want %q", idToken.Subject, jc.Subject())
	}
	var claims struct {
		ProjectSlug string `json:"project_slug"`
		Cause       string `json:"cause"`
	}
	if err := idToken.Claims(&claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.ProjectSlug != "shop" || claims.Cause != "webhook" {
		t.Errorf("claims = %+v", claims)
	}
}

func decodePayload(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token segments = %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}
