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

func TestFetchPRFirstCommit_MinAcrossPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/web/pulls/42/commits") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Commits NOT oldest-first — the earliest (07:00) is in the middle, and
		// uses committer date (author date absent). The min must win.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"commit": {"author": {"date": "2026-06-01T09:00:00Z"}}},
		  {"commit": {"committer": {"date": "2026-06-01T07:00:00Z"}}},
		  {"commit": {"author": {"date": "2026-06-01T08:00:00Z"}}}
		]`))
	}))
	defer srv.Close()

	got, err := github.FetchPRFirstCommit(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "acme", Repo: "web"}, 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.Equal(time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("first commit = %v, want earliest 07:00", got)
	}
}

func TestFetchPRFirstCommit_TrailingSlashAPIBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A doubled slash (//repos) would 404 here; the path must be clean.
		if strings.Contains(r.URL.Path, "//") {
			t.Errorf("doubled slash in path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"commit": {"author": {"date": "2026-06-01T08:00:00Z"}}}]`))
	}))
	defer srv.Close()

	got, err := github.FetchPRFirstCommit(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL + "/", Owner: "acme", Repo: "web"}, 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("first commit = %v", got)
	}
}

func TestFetchPRFirstCommit_PaginatesToGlobalMin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			// A full page (100) of later commits → triggers a second request.
			w.Write([]byte("["))
			for i := 0; i < 100; i++ {
				if i > 0 {
					w.Write([]byte(","))
				}
				w.Write([]byte(`{"commit":{"author":{"date":"2026-06-02T00:00:00Z"}}}`))
			}
			w.Write([]byte("]"))
		case "2":
			// The real first commit lives on page 2.
			w.Write([]byte(`[{"commit":{"author":{"date":"2026-05-30T06:00:00Z"}}}]`))
		default:
			w.Write([]byte("[]"))
		}
	}))
	defer srv.Close()

	got, err := github.FetchPRFirstCommit(context.Background(), srv.Client(),
		github.Config{APIBase: srv.URL, Owner: "acme", Repo: "web"}, 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.Equal(time.Date(2026, 5, 30, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("first commit = %v, want page-2 earliest 2026-05-30 06:00", got)
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
