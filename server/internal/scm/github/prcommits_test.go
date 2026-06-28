package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

func TestFetchPRFirstCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/web/pulls/42/commits") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"commit": {"author": {"date": "2026-06-01T08:00:00Z"}, "committer": {"date": "2026-06-01T09:00:00Z"}}}
		]`))
	}))
	defer srv.Close()

	got, err := github.FetchPRFirstCommit(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "acme", Repo: "web"}, 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Prefers author date over committer date.
	if !got.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("first commit = %v, want author date 08:00", got)
	}
}

func TestFetchPRFirstCommit_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	got, err := github.FetchPRFirstCommit(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "acme", Repo: "web"}, 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("want zero time for no commits, got %v", got)
	}
}

func TestFetchPRFirstCommit_BadInput(t *testing.T) {
	if _, err := github.FetchPRFirstCommit(context.Background(), http.DefaultClient,
		github.Config{Owner: "acme", Repo: "web"}, 0); err == nil {
		t.Error("want error for non-positive number")
	}
}
