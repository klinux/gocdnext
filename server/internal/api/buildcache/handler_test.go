package buildcache

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	bctoken "github.com/gocdnext/gocdnext/server/internal/buildcache"
)

// --- fakes -----------------------------------------------------------------

type fakeBlobs struct{ m map[string][]byte }

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{m: map[string][]byte{}} }

func (f *fakeBlobs) Head(_ context.Context, key string) (int64, error) {
	b, ok := f.m[key]
	if !ok {
		return 0, artifacts.ErrNotFound
	}
	return int64(len(b)), nil
}
func (f *fakeBlobs) SignedGetURL(_ context.Context, key string, _ time.Duration, _ ...artifacts.GetOption) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{URL: "https://signed.example/" + key, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (f *fakeBlobs) SignedPutURL(_ context.Context, key string, _ time.Duration, _ ...artifacts.PutOption) (artifacts.SignedURL, error) {
	return artifacts.SignedURL{URL: "https://signed.example/put/" + key}, nil
}
func (f *fakeBlobs) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.m[key] = b
	return int64(len(b)), nil
}
func (f *fakeBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := f.m[key]
	if !ok {
		return nil, artifacts.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeBlobs) Delete(_ context.Context, key string) error { delete(f.m, key); return nil }

type fakeStore struct {
	slug    string
	hash    string
	id      uuid.UUID
	enabled bool
}

func (s *fakeStore) VerifyProjectCacheToken(_ context.Context, slug, tokenHash string) (uuid.UUID, bool, error) {
	if slug == s.slug && tokenHash == s.hash {
		return s.id, true, nil
	}
	return uuid.Nil, false, nil
}

func (s *fakeStore) ProjectCacheAuthBySlug(_ context.Context, slug string) (uuid.UUID, bool, error) {
	if slug == s.slug {
		return s.id, s.enabled, nil
	}
	return uuid.Nil, false, nil
}

// --- harness ---------------------------------------------------------------

var testSignKey = []byte("buildcache-test-signing-key-32byte!!")

func newTestHandler(t *testing.T, maxEntry int64) (*chi.Mux, *fakeBlobs, string, uuid.UUID) {
	t.Helper()
	token, err := badgetoken.NewToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	projID := uuid.New()
	blobs := newFakeBlobs()
	st := &fakeStore{slug: "proj", hash: badgetoken.HashToken(token), id: projID, enabled: true}
	h := New(st, blobs, nil, maxEntry, testSignKey)
	r := chi.NewRouter()
	h.Mount(r)
	return r, blobs, token, projID
}

const validGradleKey = "0123456789abcdef0123456789abcdef"

// --- tests -----------------------------------------------------------------

func TestBuildCache_PutThenGetRedirect(t *testing.T) {
	r, blobs, token, projID := newTestHandler(t, 0)

	// PUT stores the entry.
	put := httptest.NewRequest(http.MethodPut, "/api/v1/buildcache/proj/"+validGradleKey, strings.NewReader("entrybytes"))
	put.SetBasicAuth("", token)
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, put)
	if pw.Code != http.StatusOK {
		t.Fatalf("PUT code = %d, want 200", pw.Code)
	}
	wantKey := "gradle-cache/" + projID.String() + "/" + validGradleKey
	if string(blobs.m[wantKey]) != "entrybytes" {
		t.Fatalf("stored under %q = %q", wantKey, blobs.m[wantKey])
	}

	// GET on a hit streams the bytes back with 200 (Gradle rejects redirects).
	get := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	get.SetBasicAuth("", token)
	gw := httptest.NewRecorder()
	r.ServeHTTP(gw, get)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200", gw.Code)
	}
	if gw.Body.String() != "entrybytes" {
		t.Errorf("GET body = %q, want the stored entry bytes", gw.Body.String())
	}
}

func TestBuildCache_GetMiss404(t *testing.T) {
	r, _, token, _ := newTestHandler(t, 0)
	get := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	get.SetBasicAuth("", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, get)
	if w.Code != http.StatusNotFound {
		t.Fatalf("miss code = %d, want 404", w.Code)
	}
}

func TestBuildCache_AuthRequired(t *testing.T) {
	r, _, _, _ := newTestHandler(t, 0)

	// No credentials → 401 + WWW-Authenticate.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth code = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate on 401")
	}

	// Wrong token → 401.
	bad, _ := badgetoken.NewToken()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	req.SetBasicAuth("", bad)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token code = %d, want 401", w2.Code)
	}
}

func TestBuildCache_CrossProjectRejected(t *testing.T) {
	r, _, token, _ := newTestHandler(t, 0)
	// Valid token for "proj" used against a different slug → 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/other/"+validGradleKey, nil)
	req.SetBasicAuth("", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cross-project code = %d, want 401", w.Code)
	}
}

func TestBuildCache_InvalidKey(t *testing.T) {
	r, _, token, _ := newTestHandler(t, 0)
	for _, k := range []string{"short", "UPPERHEX0123456789abcdef", "has-dash-0123456789abcdef", "zz..%2f0123456789abcdef"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+k, nil)
		req.SetBasicAuth("", token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("key %q code = %d, want 400", k, w.Code)
		}
	}
}

func TestBuildCache_MintedTokenAuth(t *testing.T) {
	r, blobs, _, projID := newTestHandler(t, 0)

	// A WRITE minted token for the project authenticates the PUT.
	minted := bctoken.MintToken(projID, true, time.Minute, testSignKey)
	put := httptest.NewRequest(http.MethodPut, "/api/v1/buildcache/proj/"+validGradleKey, strings.NewReader("x"))
	put.SetBasicAuth("gocdnext", minted)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, put)
	if w.Code != http.StatusOK {
		t.Fatalf("minted write PUT = %d, want 200", w.Code)
	}
	if _, ok := blobs.m["gradle-cache/"+projID.String()+"/"+validGradleKey]; !ok {
		t.Error("minted PUT did not store")
	}

	// A READ-ONLY minted token can GET but NOT PUT (403) — PRs can't poison.
	ro := bctoken.MintToken(projID, false, time.Minute, testSignKey)
	roGet := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	roGet.SetBasicAuth("gocdnext", ro)
	wg := httptest.NewRecorder()
	r.ServeHTTP(wg, roGet)
	if wg.Code != http.StatusOK {
		t.Fatalf("read-only GET = %d, want 200", wg.Code)
	}
	roPut := httptest.NewRequest(http.MethodPut, "/api/v1/buildcache/proj/"+validGradleKey, strings.NewReader("y"))
	roPut.SetBasicAuth("gocdnext", ro)
	wp := httptest.NewRecorder()
	r.ServeHTTP(wp, roPut)
	if wp.Code != http.StatusForbidden {
		t.Fatalf("read-only PUT = %d, want 403", wp.Code)
	}

	// A minted token for a DIFFERENT project id is rejected on this slug.
	other := bctoken.MintToken(uuid.New(), true, time.Minute, testSignKey)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	req.SetBasicAuth("gocdnext", other)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("cross-project minted = %d, want 401", w2.Code)
	}
}

func TestBuildCache_DisableRevokesMintedToken(t *testing.T) {
	// A valid minted token must stop working the moment the project disables
	// the cache (enabled=false), even though the token hasn't expired.
	token, _ := badgetoken.NewToken()
	projID := uuid.New()
	st := &fakeStore{slug: "proj", hash: badgetoken.HashToken(token), id: projID, enabled: false}
	h := New(st, newFakeBlobs(), nil, 0, testSignKey)
	r := chi.NewRouter()
	h.Mount(r)

	minted := bctoken.MintToken(projID, true, time.Minute, testSignKey)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/buildcache/proj/"+validGradleKey, nil)
	req.SetBasicAuth("gocdnext", minted)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("minted token on disabled cache = %d, want 401", w.Code)
	}
}

func TestMintedTokenExpiry(t *testing.T) {
	projID := uuid.New()
	exp := bctoken.MintToken(projID, true, time.Second, testSignKey)
	if _, _, ok := bctoken.VerifyMintedToken(exp, testSignKey, time.Now().Add(time.Hour)); ok {
		t.Error("token verified an hour past a 1s ttl")
	}
}

func TestBuildCache_PutTooLarge(t *testing.T) {
	r, _, token, _ := newTestHandler(t, 8) // 8-byte cap
	req := httptest.NewRequest(http.MethodPut, "/api/v1/buildcache/proj/"+validGradleKey, strings.NewReader("way too many bytes"))
	req.SetBasicAuth("", token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized code = %d, want 413", w.Code)
	}
}
