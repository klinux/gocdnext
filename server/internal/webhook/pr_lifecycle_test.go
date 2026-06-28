package webhook_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// stubCommits is a PRCommitsFetcher that always returns a fixed first-commit
// time, so the handler test doesn't reach the real GitHub API.
type stubCommits struct{ at time.Time }

func (s stubCommits) PRFirstCommitAt(context.Context, store.SCMSource, int) (time.Time, bool) {
	return s.at, true
}

func TestGitHubWebhook_FirstCommitRecorded(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})

	firstCommit := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	h := webhook.NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithPRCommitsFetcher(stubCommits{at: firstCommit})
	srv := http.HandlerFunc(h.HandleGitHub)

	opened := `{
      "action": "opened", "number": 42,
      "pull_request": {
        "html_url": "u", "title": "t", "merged": false,
        "created_at": "2026-06-01T10:00:00Z", "updated_at": "2026-06-01T10:00:00Z",
        "user": {"login": "dev"},
        "head": {"ref": "feat", "sha": "headsha"}, "base": {"ref": "main", "sha": "basesha"}
      },
      "repository": {"full_name": "org/demo", "clone_url": "https://github.com/org/demo.git"}
    }`
	if resp := postSigned(t, srv, "pull_request", []byte(opened)); resp.StatusCode >= 300 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	pr, err := s.PullRequest(context.Background(), "github", domain.NormalizeGitURL("https://github.com/org/demo.git"), 42)
	if err != nil {
		t.Fatalf("get pr: %v", err)
	}
	if !pr.FirstCommitAt.Equal(firstCommit) {
		t.Errorf("first_commit_at = %v, want %v", pr.FirstCommitAt, firstCommit)
	}
}

// A pull_request opened webhook persists opened_at; a pull_request_review
// (approved) webhook persists approved_at — both keyed by (provider, repo,
// number), independent of run dispatch.
func TestGitHubWebhook_PRLifecyclePersisted(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})
	srv := newServer(t, s)

	opened := `{
      "action": "opened",
      "number": 42,
      "pull_request": {
        "html_url": "https://github.com/org/demo/pull/42",
        "title": "Add feature", "merged": false,
        "created_at": "2026-06-01T10:00:00Z",
        "updated_at": "2026-06-01T10:00:00Z",
        "user": {"login": "dev"},
        "head": {"ref": "feat", "sha": "headsha"},
        "base": {"ref": "main", "sha": "basesha"}
      },
      "repository": {"full_name": "org/demo", "clone_url": "https://github.com/org/demo.git"}
    }`
	if resp := postSigned(t, srv, "pull_request", []byte(opened)); resp.StatusCode >= 300 {
		t.Fatalf("pull_request status = %d", resp.StatusCode)
	}

	review := `{
      "action": "submitted",
      "review": {"state": "approved", "submitted_at": "2026-06-01T15:30:00Z", "user": {"login": "lead"}},
      "pull_request": {"number": 42},
      "repository": {"full_name": "org/demo", "clone_url": "https://github.com/org/demo.git"}
    }`
	resp := postSigned(t, srv, "pull_request_review", []byte(review))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("review status = %d", resp.StatusCode)
	}

	// Persisted under the canonical clone_url (the authenticated identity), not
	// the unauthenticated repository.full_name.
	repo := domain.NormalizeGitURL("https://github.com/org/demo.git")
	pr, err := s.PullRequest(context.Background(), "github", repo, 42)
	if err != nil {
		t.Fatalf("get pr: %v", err)
	}
	if !pr.OpenedAt.Equal(time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("opened_at = %v", pr.OpenedAt)
	}
	if !pr.ApprovedAt.Equal(time.Date(2026, 6, 1, 15, 30, 0, 0, time.UTC)) {
		t.Errorf("approved_at = %v", pr.ApprovedAt)
	}
}

// A signed-but-malformed approval (no PR number / no submitted_at) is rejected
// with 400 rather than persisting a junk lifecycle row.
func TestGitHubWebhook_ReviewMissingFields_400(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	_ = seedPRMaterial(t, pool, []string{"push", "pull_request"})
	srv := newServer(t, s)

	body := `{
      "action": "submitted",
      "review": {"state": "approved", "user": {"login": "lead"}},
      "pull_request": {"number": 0},
      "repository": {"full_name": "org/demo", "clone_url": "https://github.com/org/demo.git"}
    }`
	resp := postSigned(t, srv, "pull_request_review", []byte(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
