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
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
)

// seedPathsMaterial seeds one pipeline whose implicit-material shape
// carries `paths:` globs (TriggerPaths lowered by configsync). url
// must match the fixture the test posts (push_main.json →
// gocdnext/gocdnext.git; pr_opened.json → org/demo.git).
func seedPathsMaterial(t *testing.T, pool *pgxpool.Pool, url string, paths, events []string) {
	t.Helper()
	branch := "main"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gocdnext-paths-test",
		Name: "gocdnext paths test",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: branch,
			WebhookSecret: testSecret,
		},
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(url, branch),
				AutoUpdate:  true,
				Git: &domain.GitMaterial{
					URL: url, Branch: branch,
					Events: events,
					Paths:  paths,
				},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		}},
	}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

// withCommitFiles rewrites the fixture's commits[] to a single commit
// carrying the given file lists (size kept consistent so the changed
// set reads as complete).
func withCommitFiles(t *testing.T, body []byte, files []string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	m["commits"] = []map[string]any{{
		"id":       "f00dface00000000000000000000000000000000",
		"message":  "test commit",
		"added":    files,
		"modified": []string{},
		"removed":  []string{},
		"author":   map[string]any{"name": "dev", "email": "dev@example.com"},
	}}
	m["size"] = 1
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return out
}

func countRuns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs`).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

func TestGitHubWebhook_PushPathsFiltered(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	seedPathsMaterial(t, pool, "https://github.com/gocdnext/gocdnext.git", []string{"**/*.go", "go.mod"}, []string{"push"})

	// Docs-only push: filtered, no run, 200 with the count.
	body := withCommitFiles(t, loadFixture(t, "push_main.json"),
		[]string{"README.md", "docs/guide.md"})
	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		FilteredByPaths int `json:"filtered_by_paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FilteredByPaths != 1 {
		t.Fatalf("filtered_by_paths = %d, want 1", got.FilteredByPaths)
	}
	if n := countRuns(t, pool); n != 0 {
		t.Fatalf("runs = %d, want 0 — docs-only push must not fire a paths-gated pipeline", n)
	}

	// Go push: matches, run created.
	body2 := withCommitFiles(t, loadFixture(t, "push_main.json"),
		[]string{"internal/store/dispatch.go"})
	resp2 := postSigned(t, srv, "push", body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp2.StatusCode, readBody(t, resp2))
	}
	if n := countRuns(t, pool); n != 1 {
		t.Fatalf("runs = %d, want 1 — matching push must fire", n)
	}
}

func TestGitHubWebhook_PushPathsFailOpenOnUnknownSet(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	seedPathsMaterial(t, pool, "https://github.com/gocdnext/gocdnext.git", []string{"**/*.go"}, []string{"push"})

	// Truncated payload: size says 30 commits, only one embedded —
	// changed set unknown → fail open, run fires.
	body := withCommitFiles(t, loadFixture(t, "push_main.json"), []string{"README.md"})
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["size"] = 30
	body, _ = json.Marshal(m)

	resp := postSigned(t, srv, "push", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (fail open); body=%s", resp.StatusCode, readBody(t, resp))
	}
	if n := countRuns(t, pool); n != 1 {
		t.Fatalf("runs = %d, want 1 — unknown changed set must fail open", n)
	}
}

// newWebhookHandlerWithPRFiles mirrors newServer but wires a stub
// PR-files fetcher so the test controls the changed set.
func newWebhookHandlerWithPRFiles(t *testing.T, s *store.Store, f webhook.PRFilesFetcher) http.Handler {
	t.Helper()
	s.SetAuthCipher(newTestCipher(t))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := webhook.NewHandler(s, logger).WithPRFilesFetcher(f)
	return http.HandlerFunc(h.HandleGitHub)
}

// stubPRFiles implements webhook.PRFilesFetcher for tests.
type stubPRFiles struct {
	files []string
	known bool
}

func (s stubPRFiles) PRChangedFiles(_ context.Context, _ store.SCMSource, _ int) ([]string, bool) {
	return s.files, s.known
}

func TestGitHubWebhook_PRPathsFiltered(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	seedPathsMaterial(t, pool, "https://github.com/org/demo.git", []string{"web/**"}, []string{"push", "pull_request"})

	tests := []struct {
		name     string
		stub     stubPRFiles
		wantRuns int
	}{
		{"non-matching PR filtered", stubPRFiles{files: []string{"internal/x.go"}, known: true}, 0},
		{"matching PR fires", stubPRFiles{files: []string{"web/src/app/page.tsx"}, known: true}, 1},
		{"unknown set fails open", stubPRFiles{known: false}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset runs AND modifications: the fixture is identical
			// across subtests, and a leftover modification row would
			// dedupe the next POST into runs:[] for the wrong reason.
			for _, table := range []string{"runs", "modifications"} {
				if _, err := pool.Exec(context.Background(), `DELETE FROM `+table); err != nil {
					t.Fatalf("reset %s: %v", table, err)
				}
			}
			h := newWebhookHandlerWithPRFiles(t, s, tt.stub)
			resp := postSigned(t, h, "pull_request", loadFixture(t, "pr_opened.json"))
			defer resp.Body.Close()
			if n := countRuns(t, pool); n != tt.wantRuns {
				t.Fatalf("runs = %d, want %d (status %d, body=%s)",
					n, tt.wantRuns, resp.StatusCode, readBody(t, resp))
			}
		})
	}
}
