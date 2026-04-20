package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm/github"
)

// appClientWith spins up a server that always answers the install-
// token request with a known token + a caller-supplied handler for
// everything else. Factored out because every hooks test needs both.
func appClientWith(t *testing.T, handler http.HandlerFunc) (*github.AppClient, *httptest.Server) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/access_tokens") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-tok",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, 1)
	return c, srv
}

func TestListRepoHooks(t *testing.T) {
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/hooks" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer inst-tok" {
			t.Errorf("auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":     111,
				"active": true,
				"events": []string{"push"},
				"config": map[string]any{"url": "https://ops.example.com/hook"},
			},
			{
				"id":     222,
				"active": true,
				"events": []string{"push", "pull_request"},
				"config": map[string]any{"url": "https://gocdnext.dev/api/webhooks/github"},
			},
		})
	})

	hooks, err := c.ListRepoHooks(context.Background(), 100, "org", "repo")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks = %d", len(hooks))
	}
	if hooks[1].ID != 222 || hooks[1].Config.URL != "https://gocdnext.dev/api/webhooks/github" {
		t.Errorf("second hook: %+v", hooks[1])
	}
}

func TestCreateRepoHook(t *testing.T) {
	var seenBody map[string]any
	c, _ := appClientWith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/org/repo/hooks" {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     999,
			"active": true,
			"events": []string{"push", "pull_request"},
			"config": map[string]any{
				"url":          "https://gocdnext.dev/api/webhooks/github",
				"content_type": "json",
			},
		})
	})

	h, err := c.CreateRepoHook(context.Background(), 100, github.CreateHookInput{
		Owner:  "org",
		Repo:   "repo",
		URL:    "https://gocdnext.dev/api/webhooks/github",
		Secret: "hunter2",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h.ID != 999 {
		t.Errorf("id = %d", h.ID)
	}

	// Payload sanity — what we POSTed should include the secret + the
	// default events + insecure_ssl off.
	cfg, _ := seenBody["config"].(map[string]any)
	if cfg["secret"] != "hunter2" {
		t.Errorf("secret not sent")
	}
	if cfg["content_type"] != "json" {
		t.Errorf("content_type = %v", cfg["content_type"])
	}
	if cfg["insecure_ssl"] != "0" {
		t.Errorf("insecure_ssl = %v", cfg["insecure_ssl"])
	}
	events, _ := seenBody["events"].([]any)
	if len(events) != 2 || events[0] != "push" || events[1] != "pull_request" {
		t.Errorf("events default = %+v", events)
	}
}

func TestFindHookForURL(t *testing.T) {
	hooks := []github.Hook{
		{ID: 1, Config: github.HookConfig{URL: "https://ops.example.com/foo"}},
		{ID: 2, Config: github.HookConfig{URL: "https://GOCDNEXT.dev/api/webhooks/github"}},
	}
	got, ok := github.FindHookForURL(hooks, "https://gocdnext.dev/api/webhooks/github")
	if !ok {
		t.Fatal("expected to find match")
	}
	if got.ID != 2 {
		t.Errorf("id = %d, want 2", got.ID)
	}

	_, ok = github.FindHookForURL(hooks, "https://other.example.com")
	if ok {
		t.Error("expected no match")
	}
}
