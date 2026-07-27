package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

func TestCreateStatus(t *testing.T) {
	const sha = "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d"
	var captured atomic.Pointer[map[string]any]
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/org/repo/statuses/"+sha {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured.Store(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 1}`))
	})

	err := c.CreateStatus(context.Background(), 100, github.CreateStatusInput{
		Owner:       "org",
		Repo:        "repo",
		SHA:         sha,
		State:       "success",
		Context:     "ci/gocdnext/pr",
		TargetURL:   "https://gocdnext.dev/runs/abc",
		Description: "Run #7 on main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := *captured.Load()
	if body["state"] != "success" {
		t.Errorf("state = %v", body["state"])
	}
	if body["context"] != "ci/gocdnext/pr" {
		t.Errorf("context = %v", body["context"])
	}
	if body["target_url"] != "https://gocdnext.dev/runs/abc" {
		t.Errorf("target_url = %v", body["target_url"])
	}
	if body["description"] != "Run #7 on main" {
		t.Errorf("description = %v", body["description"])
	}
}

func TestCreateStatus_RejectsMissingFields(t *testing.T) {
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not call the API on invalid input: %s", r.URL.Path)
	})
	if err := c.CreateStatus(context.Background(), 100, github.CreateStatusInput{
		Owner: "org", Repo: "repo", State: "success", Context: "ci/gocdnext/pr", // SHA missing
	}); err == nil {
		t.Fatal("expected error for missing SHA")
	}
}

func TestCreateStatus_TruncatesLongDescription(t *testing.T) {
	const sha = "9f7c3d2e1b8a5f6c4e0d7a9b1c3d5e7f9a0b2c4d"
	var captured atomic.Pointer[map[string]any]
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured.Store(&body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	long := strings.Repeat("x", 300)
	if err := c.CreateStatus(context.Background(), 100, github.CreateStatusInput{
		Owner: "org", Repo: "repo", SHA: sha, State: "pending",
		Context: "ci/gocdnext/pr", Description: long,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	desc, _ := (*captured.Load())["description"].(string)
	if len([]rune(desc)) != 140 {
		t.Errorf("description length = %d, want 140 (GitHub cap)", len([]rune(desc)))
	}
}
