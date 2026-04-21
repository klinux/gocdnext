package webhook_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	gh "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
)

// fakeFetcher records calls and returns pre-staged files so the drift path
// stays deterministic in-process.
type fakeFetcher struct {
	files []gh.RawFile
	err   error
	calls int
	last  struct {
		scm        store.SCMSource
		ref        string
		configPath string
	}
}

func (f *fakeFetcher) Fetch(_ context.Context, scm store.SCMSource, ref, configPath string) ([]gh.RawFile, error) {
	f.calls++
	f.last.scm = scm
	f.last.ref = ref
	f.last.configPath = configPath
	return f.files, f.err
}

// HeadSHA is required by the configsync.Fetcher contract the
// handler consumes. Drift tests don't touch it (they go through
// Fetch at the push revision) so a deterministic stub keeps the
// interface satisfied without widening scope.
func (f *fakeFetcher) HeadSHA(_ context.Context, _ store.SCMSource, _ string) (string, error) {
	return "", nil
}

// seedSCMSourceOnly registers an scm_source bound to the given
// repo URL with testSecret as its webhook secret. branch defaults
// to "main" when empty. Sets up the cipher on the local store so
// the upsert can seal the secret; handler stores share the same
// DB pool, so the sealed ciphertext is visible to them after a
// SetAuthCipher(newTestCipher(t)) on their side.
func seedSCMSourceOnly(t *testing.T, pool *pgxpool.Pool, url, branch string) {
	t.Helper()
	if branch == "" {
		branch = "main"
	}
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gocdnext-webhook-test", Name: "gocdnext webhook test",
		// No pipelines yet — the drift sync is expected to add them.
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: branch,
			WebhookSecret: testSecret,
		},
	}); err != nil {
		t.Fatalf("seed scm_source: %v", err)
	}
}

const driftCiYAML = `name: ci
materials:
  - git:
      url: https://github.com/gocdnext/gocdnext.git
      branch: main
      on: [push]
stages: [build]
jobs:
  compile:
    stage: build
    script: [make]
`

func TestGitHubWebhook_DriftApplyOnScmSourceMatch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))

	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext", "main")

	fetcher := &fakeFetcher{files: []gh.RawFile{
		{Name: "ci.yaml", Content: driftCiYAML},
	}}
	h := webhook.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithConfigFetcher(fetcher)
	srv := http.HandlerFunc(h.HandleGitHub)

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetcher calls = %d, want 1", fetcher.calls)
	}
	if fetcher.last.scm.URL != "https://github.com/gocdnext/gocdnext" {
		t.Fatalf("fetcher scm.URL = %q", fetcher.last.scm.URL)
	}

	var got struct {
		Drift struct {
			Applied  bool   `json:"applied"`
			Revision string `json:"revision"`
		} `json:"drift"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !got.Drift.Applied {
		t.Fatalf("drift.applied = false, want true. body=%s", readBody(t, resp))
	}

	// The drift re-apply should have installed the `ci` pipeline fresh.
	var pipelineCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pipelines p JOIN projects pr ON pr.id = p.project_id WHERE pr.slug = 'gocdnext-webhook-test'`,
	).Scan(&pipelineCount)
	if pipelineCount != 1 {
		t.Fatalf("pipeline count after drift = %d, want 1", pipelineCount)
	}

	// And last_synced_* columns should be populated.
	var syncedAt *time.Time
	var syncedRev *string
	_ = pool.QueryRow(context.Background(),
		`SELECT last_synced_at, last_synced_revision FROM scm_sources
		 JOIN projects ON projects.id = scm_sources.project_id
		 WHERE projects.slug = 'gocdnext-webhook-test'`,
	).Scan(&syncedAt, &syncedRev)
	if syncedAt == nil {
		t.Fatalf("last_synced_at is null after drift")
	}
	if syncedRev == nil || *syncedRev == "" {
		t.Fatalf("last_synced_revision not set")
	}
}

func TestGitHubWebhook_DriftSkippedWhenFetcherUnset(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext", "main")

	// Handler without WithConfigFetcher — drift must be a no-op.
	srv := newServer(t, s)
	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		// With no fetcher + no matching material, response is 204 (legacy).
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestGitHubWebhook_DriftSkippedForNonDefaultBranch(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	// scm_source's default_branch is "main"; the push fixture is on "main",
	// so flip scm_source to "develop" to exercise the non-default skip path.
	seedSCMSourceOnly(t, pool, "https://github.com/gocdnext/gocdnext", "develop")

	fetcher := &fakeFetcher{}
	h := webhook.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithConfigFetcher(fetcher)
	srv := http.HandlerFunc(h.HandleGitHub)

	body := loadFixture(t, "push_main.json")
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()

	if fetcher.calls != 0 {
		t.Fatalf("fetcher called %d times for non-default branch, want 0", fetcher.calls)
	}
}
