package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedEventPipelines applies one project with several pipelines, each
// carrying an implicit-style git material on the SAME fingerprint
// (url+main) but a distinct when.event lowered onto it. A push to main
// matches every fingerprint — the event filter is what decides which
// actually fire.
func seedEventPipelines(t *testing.T, pool *pgxpool.Pool, url string, byName map[string][]string) {
	t.Helper()
	branch := "main"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := store.New(pool)
	s.SetAuthCipher(newTestCipher(t))
	var pipelines []*domain.Pipeline
	for name, events := range byName {
		pipelines = append(pipelines, &domain.Pipeline{
			Name:   name,
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type:        domain.MaterialGit,
				Fingerprint: domain.GitFingerprint(url, branch),
				AutoUpdate:  true,
				Git:         &domain.GitMaterial{URL: url, Branch: branch, Events: events},
			}},
			Jobs: []domain.Job{{
				Name: "c", Stage: "build",
				Tasks: []domain.Task{{Script: "true"}},
			}},
		})
	}
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gocdnext-event-test",
		Name: "event test",
		SCMSource: &store.SCMSourceInput{
			Provider:      "github",
			URL:           url,
			DefaultBranch: branch,
			WebhookSecret: testSecret,
		},
		Pipelines: pipelines,
	}); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

// A branch push must fire ONLY pipelines whose when.event includes
// "push" — a fingerprint match (URL+branch) is necessary but not
// sufficient. tag-only and manual-only pipelines keep an implicit
// material on the same branch and previously fanned out on every push.
func TestGitHubWebhook_PushFiltersByEvent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	url := "https://github.com/gocdnext/gocdnext.git"
	seedEventPipelines(t, pool, url, map[string][]string{
		"ci-push":    {"push"},
		"ci-prpush":  {"pull_request", "push"},
		"rel-tag":    {"tag"},
		"rel-manual": {"manual"},
	})

	resp := postSigned(t, srv, "push", loadFixture(t, "push_main.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Only ci-push + ci-prpush fire; rel-tag + rel-manual filtered out.
	if n := countRuns(t, pool); n != 2 {
		t.Fatalf("runs = %d, want 2 — tag-only and manual-only pipelines must not fire on a push", n)
	}
}

// A material with no on: (empty events) defaults to push — back-compat
// for rows applied before/without an explicit when.event.
func TestGitHubWebhook_PushDefaultEventFires(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newServer(t, s)
	url := "https://github.com/gocdnext/gocdnext.git"
	seedEventPipelines(t, pool, url, map[string][]string{
		"ci-default": nil, // no events → defaults to ["push"]
	})

	resp := postSigned(t, srv, "push", loadFixture(t, "push_main.json"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if n := countRuns(t, pool); n != 1 {
		t.Fatalf("runs = %d, want 1 — empty-events material defaults to push and must fire", n)
	}
}

// The GitLab/Bitbucket branch push rides the shared persistPush path —
// it gets the same when.event guard. A tag-only pipeline must not fire
// on a GitLab push to its branch.
func TestGitLabWebhook_PushFiltersByEvent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	srv := newGitLabServer(t, s)
	seedGitLabMRMaterial(t, pool, []string{"tag"}) // tag-only pipeline

	payload, err := json.Marshal(map[string]any{
		"object_kind": "push",
		"ref":         "refs/heads/main",
		"before":      "0000000000000000000000000000000000000001",
		"after":       "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"repository":  map[string]any{"git_http_url": "https://gitlab.example.com/group/demo.git"},
		"commits": []map[string]any{{
			"id":        "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			"message":   "feat: a normal change", // no skip-ci marker
			"timestamp": "2026-06-12T10:00:00Z",
			"author":    map[string]any{"name": "dev", "email": "dev@example.com"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", bytes.NewReader(payload))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	req.Header.Set("X-Gitlab-Token", testSecret)
	req.Header.Set("X-Gitlab-Event-UUID", "test-event-filter")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	resp := rr.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if n := countRuns(t, pool); n != 0 {
		t.Fatalf("runs = %d, want 0 — tag-only pipeline must not fire on a gitlab push", n)
	}
}
