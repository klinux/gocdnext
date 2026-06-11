package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newOIDCKeysHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)

	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())

	r := chi.NewRouter()
	r.Get("/api/v1/admin/oidc/keys", h.OIDCKeys)
	r.Post("/api/v1/admin/oidc/keys/rotate", h.RotateOIDCKey)
	return s, r
}

// TestOIDCKeys_ListMetadataOnly — the listing exposes lifecycle
// metadata (kid, dates) and NEVER key material.
func TestOIDCKeys_ListMetadataOnly(t *testing.T) {
	s, srv := newOIDCKeysHandler(t)
	if _, err := s.EnsureActiveOIDCKey(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	rr := request(srv, http.MethodGet, "/api/v1/admin/oidc/keys", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(got.Keys))
	}
	k := got.Keys[0]
	if k["kid"] == "" {
		t.Errorf("kid missing")
	}
	for _, forbidden := range []string{"private_key", "private_key_enc", "public_key_der", "n", "d"} {
		if _, present := k[forbidden]; present {
			t.Errorf("listing exposes %q — key material must never leave the store", forbidden)
		}
	}
}

// TestRotateOIDCKey_GracefulDefault — POST without a body rotates
// gracefully: new active kid, old key retired (still listed, with
// retired_at set).
func TestRotateOIDCKey_GracefulDefault(t *testing.T) {
	s, srv := newOIDCKeysHandler(t)
	old, err := s.EnsureActiveOIDCKey(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	rr := request(srv, http.MethodPost, "/api/v1/admin/oidc/keys/rotate", bytes.NewBufferString(`{}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Kid  string `json:"kid"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "graceful" {
		t.Errorf("mode = %q, want graceful default", resp.Mode)
	}
	if resp.Kid == "" || resp.Kid == old.Kid {
		t.Errorf("kid = %q, want a fresh key (old %q)", resp.Kid, old.Kid)
	}

	// Old key still serves in the JWKS inside the overlap.
	keys, err := s.ListOIDCJWKSKeys(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("jwks keys = %d, want 2 (graceful keeps the old key verifiable)", len(keys))
	}

	// Audit event recorded.
	assertAuditAction(t, s, "oidc_key.rotate")
}

// TestRotateOIDCKey_Emergency — {"mode":"emergency"} revokes: old
// key gone from the JWKS immediately.
func TestRotateOIDCKey_Emergency(t *testing.T) {
	s, srv := newOIDCKeysHandler(t)
	if _, err := s.EnsureActiveOIDCKey(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	rr := request(srv, http.MethodPost, "/api/v1/admin/oidc/keys/rotate", bytes.NewBufferString(`{"mode":"emergency"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	keys, err := s.ListOIDCJWKSKeys(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("jwks keys = %d, want 1 (emergency drops the old key immediately)", len(keys))
	}
}

// fakeRotator counts routed rotations, delegating to the store so
// the response/audit path still has a real key to report.
type fakeRotator struct {
	s     *store.Store
	calls int
	emerg []bool
}

func (f *fakeRotator) Rotate(ctx context.Context, emergency bool) (store.OIDCSigningKey, error) {
	f.calls++
	f.emerg = append(f.emerg, emergency)
	return f.s.RotateOIDCKey(ctx, emergency)
}

// TestRotateOIDCKey_RoutesThroughIssuer — with the issuer enabled,
// rotation MUST go through it (the issuer holds its signing-key
// lock across DB commit + cache swap; calling the store directly
// would reopen the sign-with-revoked-key window). Review round 2.
func TestRotateOIDCKey_RoutesThroughIssuer(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	s.SetAuthCipher(c)
	if _, err := s.EnsureActiveOIDCKey(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
	rot := &fakeRotator{s: s}
	h.SetOIDCRotator(rot)

	r := chi.NewRouter()
	r.Post("/api/v1/admin/oidc/keys/rotate", h.RotateOIDCKey)

	rr := request(r, http.MethodPost, "/api/v1/admin/oidc/keys/rotate", bytes.NewBufferString(`{"mode":"emergency"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rot.calls != 1 || len(rot.emerg) != 1 || !rot.emerg[0] {
		t.Errorf("rotator calls = %d emerg = %v, want exactly one emergency rotation routed through the issuer", rot.calls, rot.emerg)
	}

	// Failure path must NOT touch the rotator (nothing to rotate).
	rr = request(r, http.MethodPost, "/api/v1/admin/oidc/keys/rotate", bytes.NewBufferString(`{"mode":"bogus"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if rot.calls != 1 {
		t.Errorf("rotator touched on a failed request")
	}
}

// TestRotateOIDCKey_BadMode — unknown mode fails loud, no rotation.
func TestRotateOIDCKey_BadMode(t *testing.T) {
	s, srv := newOIDCKeysHandler(t)
	old, err := s.EnsureActiveOIDCKey(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	rr := request(srv, http.MethodPost, "/api/v1/admin/oidc/keys/rotate", bytes.NewBufferString(`{"mode":"yolo"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	cur, err := s.EnsureActiveOIDCKey(context.Background())
	if err != nil {
		t.Fatalf("ensure post: %v", err)
	}
	if cur.Kid != old.Kid {
		t.Errorf("key rotated despite bad mode")
	}
}

// assertAuditAction asserts at least one audit event with the given
// action exists.
func assertAuditAction(t *testing.T, s *store.Store, action string) {
	t.Helper()
	page, err := s.ListAuditEvents(context.Background(), store.ListAuditEventsFilter{Action: action, Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(page.Events) == 0 {
		t.Errorf("no audit event with action %q", action)
	}
}
