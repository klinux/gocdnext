package configsync_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestMultiFetcher_GitHubAppFallback proves the App-token fallback path:
// no per-project / org-level PAT, but an installed GitHub App is wired
// — the fetcher mints an installation token, hits Contents API with it,
// and the private-repo `.gocdnext/` returns. Pre-fix this test fails
// because MultiFetcher only consulted the PAT-style CredentialResolver.
func TestMultiFetcher_GitHubAppFallback(t *testing.T) {
	srv, contentsHit := newGitHubMock(t, gitHubMockSpec{
		expectRef:         "gocdnext-tests",
		expectAuthBearer:  "ghs_fallback_token",
		mintedToken:       "ghs_fallback_token",
		mintedInstallID:   42,
		contentsYAMLFile:  "pipe.yaml",
		contentsYAMLBody:  "name: pipe-from-app\nstages: []\n",
		installationOwner: "octocat",
		installationRepo:  "hello-world",
	})
	defer srv.Close()

	app := newTestAppClient(t, srv.URL, 777)
	fetcher := &configsync.MultiFetcher{
		GitHubAPIBase: srv.URL,
		// No Resolver wired → no PAT → only the App can supply auth.
		GitHubApp: appTokenAdapter{app: app},
	}

	files, err := fetcher.Fetch(context.Background(),
		store.SCMSource{
			Provider: "github",
			URL:      "https://github.com/octocat/hello-world",
		},
		"gocdnext-tests", ".gocdnext")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(files) != 1 || files[0].Name != "pipe.yaml" {
		t.Fatalf("files = %+v", files)
	}
	if *contentsHit == 0 {
		t.Errorf("contents endpoint never called")
	}
}

// TestMultiFetcher_PATWinsOverApp asserts the App fallback never
// overrides an explicit PAT. The resolver returns a PAT; the App is
// also wired but its endpoints are not even mounted — calling them
// would 404 and the test would fail loudly.
func TestMultiFetcher_PATWinsOverApp(t *testing.T) {
	yaml := "name: pipe-from-pat\nstages: []\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octocat/hello-world/contents/.gocdnext",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer pat-token" {
				t.Errorf("Authorization = %q, want PAT", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"name":     "p.yaml",
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(yaml)),
			}})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fetcher := &configsync.MultiFetcher{
		GitHubAPIBase: srv.URL,
		Resolver:      staticResolver{token: "pat-token"},
		GitHubApp:     panicAppSource{t: t},
	}
	if _, err := fetcher.Fetch(context.Background(),
		store.SCMSource{Provider: "github", URL: "https://github.com/octocat/hello-world"},
		"main", ".gocdnext",
	); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// TestMultiFetcher_AppError_LogsAndFallsThrough covers the
// debugability concern from the review: when the App token mint
// fails (network/JWT/etc.), the fetcher logs a warn line via the
// wired Logger and proceeds without auth — instead of silently
// dropping the error and producing a misleading "config folder not
// found" downstream.
func TestMultiFetcher_AppError_LogsAndFallsThrough(t *testing.T) {
	contentsCalls := atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octocat/hello-world/contents/.gocdnext",
		func(w http.ResponseWriter, r *http.Request) {
			contentsCalls.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("expected unauth fall-through, got Authorization = %q",
					r.Header.Get("Authorization"))
			}
			// Simulate the private-repo silent 404.
			http.Error(w, "not found", http.StatusNotFound)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	fetcher := &configsync.MultiFetcher{
		GitHubAPIBase: srv.URL,
		Logger:        logger,
		GitHubApp:     errAppSource{err: errors.New("boom: jwt sign failed")},
	}
	_, err := fetcher.Fetch(context.Background(),
		store.SCMSource{Provider: "github", URL: "https://github.com/octocat/hello-world"},
		"main", ".gocdnext")
	if !errors.Is(err, configsync.ErrFolderNotFound) {
		t.Fatalf("err = %v, want wrapping ErrFolderNotFound", err)
	}
	if contentsCalls.Load() != 1 {
		t.Errorf("contents called %d times", contentsCalls.Load())
	}
	if !strings.Contains(logBuf.String(), "github app token mint failed") {
		t.Errorf("missing warn log; got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "boom: jwt sign failed") {
		t.Errorf("warn log missing underlying err; got:\n%s", logBuf.String())
	}
}

// TestRegistry_InstallationTokenFor_NilSafe locks down the
// defensive nil-receiver + nil-app branches in the Registry so a
// typed-nil interface value from a future call site doesn't panic
// the fetcher.
func TestRegistry_InstallationTokenFor_NilSafe(t *testing.T) {
	// Typed-nil receiver: var r *vcs.Registry; r.InstallationTokenFor(...)
	// Imported via configsync to avoid an import cycle in this test
	// package — same code path, exercised through the interface.
	var src configsync.GitHubAppTokenSource = typedNilRegistry()
	tok, base, err := src.InstallationTokenFor(context.Background(), "o", "r")
	if err != nil || tok != "" || base != "" {
		t.Errorf("typed-nil registry: got (%q, %q, %v); want (\"\",\"\",nil)", tok, base, err)
	}
}

// TestMultiFetcher_AppAPIBaseWinsOverGenericGitHubBase asserts the
// GHE multi-host concern from the review: when the App is configured
// against a GHE URL, that base is used for the contents call —
// preventing a fresh installation token from being sent to a
// different GitHub instance than the one that issued it.
func TestMultiFetcher_AppAPIBaseWinsOverGenericGitHubBase(t *testing.T) {
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("contents request hit the wrong (generic) base")
		w.WriteHeader(http.StatusOK)
	}))
	defer wrong.Close()

	gheSrv, _ := newGitHubMock(t, gitHubMockSpec{
		expectRef:         "main",
		expectAuthBearer:  "ghe-tok",
		mintedToken:       "ghe-tok",
		mintedInstallID:   99,
		contentsYAMLFile:  "pipe.yaml",
		contentsYAMLBody:  "name: ghe-pipe\nstages: []\n",
		installationOwner: "ghe-org",
		installationRepo:  "ghe-repo",
	})
	defer gheSrv.Close()

	app := newTestAppClient(t, gheSrv.URL, 1)
	fetcher := &configsync.MultiFetcher{
		GitHubAPIBase: wrong.URL,
		GitHubApp:     appTokenAdapter{app: app},
	}
	files, err := fetcher.Fetch(context.Background(),
		store.SCMSource{
			Provider: "github",
			URL:      "https://github.example.com/ghe-org/ghe-repo",
		},
		"main", ".gocdnext")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
}

// TestMultiFetcher_HeadSHAUsesAppToken locks down that HeadSHA goes
// through the same resolveGitHub path Fetch does. Pre-refactor only
// Fetch exercised resolveGitHub; without this test a future change
// could silently regress HeadSHA back to PAT-only.
func TestMultiFetcher_HeadSHAUsesAppToken(t *testing.T) {
	const tok = "ghs_head_token"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/installation",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
		})
	mux.HandleFunc("/app/installations/7/access_tokens",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      tok,
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		})
	mux.HandleFunc("/repos/o/r/branches/main",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+tok {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commit": map[string]any{"sha": "deadbeef"},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := newTestAppClient(t, srv.URL, 1)
	fetcher := &configsync.MultiFetcher{
		GitHubAPIBase: srv.URL,
		GitHubApp:     appTokenAdapter{app: app},
	}
	sha, err := fetcher.HeadSHA(context.Background(),
		store.SCMSource{Provider: "github", URL: "https://github.com/o/r"}, "main")
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q", sha)
	}
}

// --- helpers ---

type gitHubMockSpec struct {
	expectRef, expectAuthBearer        string
	mintedToken                        string
	mintedInstallID                    int64
	contentsYAMLFile, contentsYAMLBody string
	installationOwner, installationRepo string
}

// newGitHubMock returns an httptest server that mimics the three GitHub
// endpoints the App fetcher path consults: /repos/{o}/{r}/installation
// (JWT-authenticated), /app/installations/{id}/access_tokens (mint),
// and /repos/{o}/{r}/contents/.gocdnext (token-authenticated). Returns
// a counter pointer so the test can assert the contents call ran.
func newGitHubMock(t *testing.T, spec gitHubMockSpec) (*httptest.Server, *int32) {
	t.Helper()
	mux := http.NewServeMux()
	contentsHits := new(int32)

	installPath := "/repos/" + spec.installationOwner + "/" + spec.installationRepo + "/installation"
	mux.HandleFunc(installPath, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("installation lookup missing bearer JWT")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": spec.mintedInstallID})
	})
	tokenPath := "/app/installations/" +
		strings.TrimSpace(itoa(spec.mintedInstallID)) + "/access_tokens"
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      spec.mintedToken,
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	contentsPath := "/repos/" + spec.installationOwner + "/" + spec.installationRepo + "/contents/.gocdnext"
	mux.HandleFunc(contentsPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(contentsHits, 1)
		if r.Header.Get("Authorization") != "Bearer "+spec.expectAuthBearer {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("ref"); got != spec.expectRef {
			t.Errorf("ref query = %q, want %q", got, spec.expectRef)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"name":     spec.contentsYAMLFile,
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte(spec.contentsYAMLBody)),
		}})
	})
	return httptest.NewServer(mux), contentsHits
}

func newTestAppClient(t *testing.T, apiBase string, appID int64) *ghscm.AppClient {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	app, err := ghscm.NewAppClient(ghscm.AppConfig{
		AppID:         appID,
		PrivateKeyPEM: pemBytes,
		APIBase:       apiBase,
	})
	if err != nil {
		t.Fatalf("new app client: %v", err)
	}
	return app
}

// itoa avoids importing strconv for a one-liner formatter.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

type appTokenAdapter struct{ app *ghscm.AppClient }

func (a appTokenAdapter) InstallationTokenFor(ctx context.Context, owner, repo string) (string, string, error) {
	tok, err := a.app.TokenForRepo(ctx, owner, repo)
	if err != nil {
		return "", "", err
	}
	return tok, a.app.APIBase(), nil
}

type staticResolver struct{ token, apiBase string }

func (s staticResolver) ResolveAuthRef(_ context.Context, _, _, _ string) (string, string) {
	return s.token, s.apiBase
}

type panicAppSource struct{ t *testing.T }

func (p panicAppSource) InstallationTokenFor(context.Context, string, string) (string, string, error) {
	p.t.Fatalf("App fallback should not run when PAT is present")
	return "", "", nil
}

type errAppSource struct{ err error }

func (e errAppSource) InstallationTokenFor(context.Context, string, string) (string, string, error) {
	return "", "", e.err
}

// typedNilRegistry returns an interface value whose dynamic type is
// *nilSafeRegistry but whose value is nil — i.e. the typed-nil
// foot-gun the review flagged. The wrapper mirrors the same
// nil-receiver guard the real vcs.Registry uses, so this test stays
// local to the configsync package without importing vcs.
type nilSafeRegistry struct{}

func (r *nilSafeRegistry) InstallationTokenFor(context.Context, string, string) (string, string, error) {
	if r == nil {
		return "", "", nil
	}
	panic("unreachable")
}

func typedNilRegistry() *nilSafeRegistry { return nil }
