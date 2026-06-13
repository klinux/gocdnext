package cliconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GOCDNEXT_TOKEN", "")
	return dir
}

func TestSetAndResolveToken(t *testing.T) {
	dir := withTempConfig(t)

	if tok := ResolveToken("https://ci.example.com"); tok != "" {
		t.Fatalf("empty config should resolve no token, got %q", tok)
	}
	if err := SetToken("https://ci.example.com/", "tok-abc"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	// Trailing-slash normalization: lookups must hit the same entry.
	if tok := ResolveToken("https://ci.example.com"); tok != "tok-abc" {
		t.Fatalf("ResolveToken = %q, want tok-abc", tok)
	}

	// File permissions: the token file is a credential store.
	path := filepath.Join(dir, "gocdnext", "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config perms = %o, want 600", perm)
	}
}

func TestEnvTokenWinsOverConfig(t *testing.T) {
	withTempConfig(t)
	if err := SetToken("https://ci.example.com", "from-config"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	t.Setenv("GOCDNEXT_TOKEN", "from-env")
	if tok := ResolveToken("https://ci.example.com"); tok != "from-env" {
		t.Fatalf("env must win for CI/scripting overrides, got %q", tok)
	}
}

func TestDeleteToken(t *testing.T) {
	withTempConfig(t)
	_ = SetToken("https://ci.example.com", "tok")
	if err := DeleteToken("https://ci.example.com"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if tok := ResolveToken("https://ci.example.com"); tok != "" {
		t.Fatalf("token survived logout: %q", tok)
	}
}

func TestHTTPClientSendsBearer(t *testing.T) {
	withTempConfig(t)
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_ = SetToken(srv.URL, "tok-xyz")
	client := HTTPClient(srv.URL, 0)
	resp, err := client.Get(srv.URL + "/api/v1/projects")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if got != "Bearer tok-xyz" {
		t.Fatalf("Authorization = %q, want Bearer tok-xyz", got)
	}
}

func TestHTTPClientDoesNotLeakTokenAcrossOrigins(t *testing.T) {
	withTempConfig(t)

	// "attacker" origin: must never see the bearer token.
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer other.Close()

	// Configured server redirects off-origin (compromised or MITM'd).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()

	_ = SetToken(srv.URL, "tok-secret")
	client := HTTPClient(srv.URL, 0)
	resp, err := client.Get(srv.URL + "/api/v1/projects")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if leaked != "" {
		t.Fatalf("bearer token leaked to off-origin redirect target: %q", leaked)
	}

	// Direct off-origin request through the same client: also no header.
	resp, err = client.Get(other.URL + "/direct")
	if err != nil {
		t.Fatalf("direct get: %v", err)
	}
	_ = resp.Body.Close()
	if leaked != "" {
		t.Fatalf("bearer token sent to a different origin: %q", leaked)
	}
}

func TestHTTPClientNoTokenNoHeader(t *testing.T) {
	withTempConfig(t)
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	client := HTTPClient(srv.URL, 0)
	resp, err := client.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if got != "" {
		t.Fatalf("no token must mean no header (auth-disabled servers), got %q", got)
	}
}

func TestValidateToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Mirror the real authapi shape: store.User wrapped in "user".
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user": map[string]string{"email": "dev@example.com", "name": "Dev"},
		})
	}))
	defer srv.Close()

	who, err := ValidateToken(context.Background(), srv.URL, "good")
	if err != nil || who == "" {
		t.Fatalf("ValidateToken good = %q, %v", who, err)
	}
	if _, err := ValidateToken(context.Background(), srv.URL, "bad"); err == nil {
		t.Fatal("bad token must fail validation")
	}
}
