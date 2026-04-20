package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	return k
}

func keyToPEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func newTestClient(t *testing.T, server *httptest.Server, appID int64) *github.AppClient {
	t.Helper()
	key := mustRSAKey(t)
	c, err := github.NewAppClient(github.AppConfig{
		AppID:         appID,
		PrivateKeyPEM: keyToPEM(t, key),
		APIBase:       server.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestNewAppClient_RejectsBadKey(t *testing.T) {
	_, err := github.NewAppClient(github.AppConfig{
		AppID:         123,
		PrivateKeyPEM: []byte("not a PEM block"),
	})
	if err == nil {
		t.Error("expected error")
	}
}

func TestNewAppClient_RequiresAppID(t *testing.T) {
	_, err := github.NewAppClient(github.AppConfig{
		PrivateKeyPEM: keyToPEM(t, mustRSAKey(t)),
	})
	if err == nil {
		t.Error("expected error for missing AppID")
	}
}

// verifyJWT ensures the Authorization header is a well-formed RS256
// JWT signed by `key`, with iss = appID and an `exp` in the future.
func verifyJWT(t *testing.T, auth string, appID int64, key *rsa.PrivateKey) {
	t.Helper()
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("Authorization header = %q", auth)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d segments", len(parts))
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.Iss != fmt.Sprintf("%d", appID) {
		t.Errorf("iss = %q, want %d", payload.Iss, appID)
	}
	if payload.Exp < time.Now().Unix() {
		t.Errorf("exp already passed: %d", payload.Exp)
	}
}

func TestAppClient_InstallationID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/repos/org/repo/installation" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("missing bearer auth: %q", auth)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 777)
	got, err := c.InstallationID(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("InstallationID: %v", err)
	}
	if got != 42 {
		t.Errorf("installation id = %d", got)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d", calls.Load())
	}
}

func TestAppClient_InstallationID_404_ReturnsErrNoInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 1)
	_, err := c.InstallationID(context.Background(), "org", "missing")
	if !errors.Is(err, github.ErrNoInstallation) {
		t.Errorf("err = %v, want ErrNoInstallation", err)
	}
}

func TestAppClient_InstallationToken_CachedBetweenCalls(t *testing.T) {
	var mints atomic.Int32
	expires := time.Now().Add(30 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			mints.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_fake_token",
				"expires_at": expires.Format(time.RFC3339),
			})
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 1)

	tok1, err := c.InstallationToken(context.Background(), 100)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	tok2, err := c.InstallationToken(context.Background(), 100)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if tok1 != "ghs_fake_token" || tok2 != tok1 {
		t.Errorf("tokens differ or wrong: %q %q", tok1, tok2)
	}
	if mints.Load() != 1 {
		t.Errorf("minted %d times, want 1 (cache miss the second call)", mints.Load())
	}
}

func TestAppClient_InstallationToken_DifferentInstallationsGetDifferentTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the installation id so we can verify keyed caching.
		var id int64
		_, _ = fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &id)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("tok-%d", id),
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 1)
	t1, _ := c.InstallationToken(context.Background(), 100)
	t2, _ := c.InstallationToken(context.Background(), 200)
	if t1 == t2 {
		t.Errorf("same token for different installations: %q %q", t1, t2)
	}
}

func TestAppClient_DoAsInstallation_AttachesToken(t *testing.T) {
	var tokenMinted int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			atomic.AddInt32(&tokenMinted, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-token",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		// Any other path asserts the installation token is attached.
		if got := r.Header.Get("Authorization"); got != "Bearer inst-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 1)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/repos/org/repo/hooks", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := c.DoAsInstallation(context.Background(), 100, req)
	if err != nil {
		t.Fatalf("DoAsInstallation: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestAppClient_MintJWTSignsWithConfiguredAppID(t *testing.T) {
	key := mustRSAKey(t)
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer srv.Close()

	c, err := github.NewAppClient(github.AppConfig{
		AppID:         9999,
		PrivateKeyPEM: keyToPEM(t, key),
		APIBase:       srv.URL,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := c.InstallationID(context.Background(), "a", "b"); err != nil {
		t.Fatalf("call: %v", err)
	}
	verifyJWT(t, captured, 9999, key)
}
