package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/auth"
)

// fakeGitHub stands in for api.github.com + the /login/oauth/access_token
// endpoint. oauth2.Exchange POSTs to the Endpoint.TokenURL — we don't
// override that in the provider so we can't fully test Exchange without
// monkey-patching the oauth2 endpoint. What we CAN test is the claims
// extraction (/user + /user/emails) via a direct fetch — so we expose
// that surface indirectly by driving Exchange with a stubbed endpoint.
//
// The provider's HTTPClient override is the hook: we redirect ALL
// traffic (token exchange + api calls) at the stub server.
func fakeGitHub(t *testing.T, user map[string]any, emails []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		// GitHub returns application/x-www-form-urlencoded by default;
		// oauth2 parses either that or JSON. Stick to JSON for clarity.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stub","token_type":"bearer","scope":"read:user user:email"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(user)
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(emails)
	})
	return httptest.NewServer(mux)
}

// newGitHubFromStub points both the OAuth token URL (via the stub
// server acting as api.github.com) and the API base at the stub.
func newGitHubFromStub(t *testing.T, srv *httptest.Server) auth.Provider {
	t.Helper()
	// We need oauth2 to POST the token exchange at the stub too.
	// Override by wiring a custom HTTPClient that rewrites the host
	// on outbound requests. Simpler: configure APIBase to the stub
	// and accept that token exchange still goes to github.com —
	// we'll short-circuit via a RoundTripper.
	rt := &rewriteTransport{target: srv.URL}
	p, err := auth.NewGitHubProvider(auth.GitHubConfig{
		ClientID:     "id",
		ClientSecret: "secret",
		CallbackURL:  "https://ci.example.com/auth/callback/github",
		APIBase:      srv.URL,
		HTTPClient:   &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

// rewriteTransport sends every outbound request at `target`, preserving
// the path + method. That captures both the oauth2 token exchange (to
// github.com/login/oauth/access_token) and the API calls (to our
// overridden APIBase) so the stub handles both.
type rewriteTransport struct {
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	idx := strings.Index(t.target, "://")
	host := t.target[idx+3:]
	req.URL.Host = host
	req.Host = host
	return http.DefaultTransport.RoundTrip(req)
}

func TestGitHubProvider_Exchange_PublicEmail(t *testing.T) {
	srv := fakeGitHub(t,
		map[string]any{
			"id":         int64(123),
			"login":      "alice",
			"name":       "Alice",
			"email":      "alice@example.com",
			"avatar_url": "https://cdn/alice.png",
		},
		nil, // /user/emails never called when /user has an email
	)
	defer srv.Close()

	p := newGitHubFromStub(t, srv)
	claims, err := p.Exchange(context.Background(), "code-abc", "state", "")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if claims.Subject != "123" || claims.Email != "alice@example.com" || claims.Name != "Alice" {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.AvatarURL != "https://cdn/alice.png" {
		t.Fatalf("avatar url = %q", claims.AvatarURL)
	}
}

func TestGitHubProvider_Exchange_PrivateEmailFallback(t *testing.T) {
	srv := fakeGitHub(t,
		map[string]any{
			"id":    int64(77),
			"login": "bob",
			"name":  "",
			"email": "", // hidden from public profile
		},
		[]map[string]any{
			{"email": "bob-wrong@example.com", "primary": false, "verified": true},
			{"email": "bob@example.com", "primary": true, "verified": true},
		},
	)
	defer srv.Close()

	p := newGitHubFromStub(t, srv)
	claims, err := p.Exchange(context.Background(), "code", "state", "")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if claims.Email != "bob@example.com" {
		t.Fatalf("did not pick primary+verified email, got %q", claims.Email)
	}
	if claims.Name != "bob" {
		t.Fatalf("name fallback = %q, want login", claims.Name)
	}
	if claims.Subject != "77" {
		t.Fatalf("subject = %q", claims.Subject)
	}
}

func TestGitHubProvider_Exchange_MissingClaims(t *testing.T) {
	srv := fakeGitHub(t,
		map[string]any{
			"id":    int64(0), // falsy id → Subject becomes "0" → still present
			"login": "",
			"name":  "",
			"email": "",
		},
		[]map[string]any{}, // empty list → no verified email → fallback stays blank
	)
	defer srv.Close()

	p := newGitHubFromStub(t, srv)
	_, err := p.Exchange(context.Background(), "code", "state", "")
	if !errors.Is(err, auth.ErrClaimsMissing) {
		t.Fatalf("err = %v, want ErrClaimsMissing", err)
	}
}

func TestGitHubProvider_ValidationAtBoot(t *testing.T) {
	tests := []struct {
		name string
		cfg  auth.GitHubConfig
	}{
		{"empty", auth.GitHubConfig{}},
		{"missing callback", auth.GitHubConfig{ClientID: "a", ClientSecret: "b"}},
		{"missing client", auth.GitHubConfig{ClientSecret: "b", CallbackURL: "https://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := auth.NewGitHubProvider(tt.cfg); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestGitHubProvider_AuthorizeURL_ContainsState(t *testing.T) {
	p, err := auth.NewGitHubProvider(auth.GitHubConfig{
		ClientID: "id", ClientSecret: "s",
		CallbackURL: "https://ci.example.com/cb",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	u := p.AuthorizeURL("stateXYZ", "")
	if !strings.Contains(u, "state=stateXYZ") {
		t.Fatalf("URL missing state: %s", u)
	}
	if !strings.Contains(u, "github.com") {
		t.Fatalf("URL not pointing at github: %s", u)
	}
}

func TestRegistry_InsertionOrder(t *testing.T) {
	p1 := stubProvider{name: auth.ProviderGitHub, display: "GitHub"}
	p2 := stubProvider{name: auth.ProviderGoogle, display: "Google"}

	reg := auth.NewRegistry(p1, p2)
	if reg.Len() != 2 {
		t.Fatalf("len = %d", reg.Len())
	}
	list := reg.List()
	if list[0].Name() != auth.ProviderGitHub || list[1].Name() != auth.ProviderGoogle {
		t.Fatalf("order lost: %v %v", list[0].Name(), list[1].Name())
	}
	if reg.Get(auth.ProviderGitHub) == nil {
		t.Fatalf("Get github missing")
	}
	if reg.Get(auth.ProviderKeycloak) != nil {
		t.Fatalf("Get keycloak should be nil")
	}
}

type stubProvider struct {
	name    auth.ProviderName
	display string
}

func (s stubProvider) Name() auth.ProviderName { return s.name }
func (s stubProvider) DisplayName() string     { return s.display }
func (s stubProvider) AuthorizeURL(string, string) string {
	return "https://idp.example.com/authorize"
}
func (s stubProvider) Exchange(context.Context, string, string, string) (auth.Claims, error) {
	return auth.Claims{}, nil
}
