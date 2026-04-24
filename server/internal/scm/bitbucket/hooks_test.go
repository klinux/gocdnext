package bitbucket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeBitbucket struct {
	t          *testing.T
	hooks      []map[string]any
	lastCreate map[string]any
	lastUpdate map[string]any
	lastPath   string
	lastAuth   string
}

func (f *fakeBitbucket) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/repositories/", func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"values": f.hooks,
			})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &f.lastCreate)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"uuid": "{abc-def}",
				"url":  f.lastCreate["url"],
			})
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &f.lastUpdate)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"uuid": "{abc-def}",
				"url":  f.lastUpdate["url"],
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func TestCreateRepoHook_Bearer(t *testing.T) {
	fb := &fakeBitbucket{t: t}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	cfg := Config{
		APIBase:   srv.URL,
		Workspace: "acme",
		RepoSlug:  "svc",
		Token:     "oauth-token",
	}
	hook, err := CreateRepoHook(context.Background(), srv.Client(), cfg, CreateHookInput{
		Workspace: "acme", RepoSlug: "svc",
		URL:    "https://ci.example.com/api/webhooks/bitbucket",
		Secret: "shh",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if hook.UID != "{abc-def}" {
		t.Errorf("UID = %q", hook.UID)
	}
	if fb.lastAuth != "Bearer oauth-token" {
		t.Errorf("auth = %q, want Bearer", fb.lastAuth)
	}
	if fb.lastCreate["secret"] != "shh" {
		t.Errorf("secret not passed: %v", fb.lastCreate["secret"])
	}
	events, _ := fb.lastCreate["events"].([]any)
	if len(events) != 1 || events[0] != "repo:push" {
		t.Errorf("events = %v", events)
	}
	if !strings.Contains(fb.lastPath, "/repositories/acme/svc/hooks") {
		t.Errorf("path = %q", fb.lastPath)
	}
}

func TestCreateRepoHook_BasicAuth(t *testing.T) {
	fb := &fakeBitbucket{t: t}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	cfg := Config{
		APIBase:     srv.URL,
		Workspace:   "acme",
		RepoSlug:    "svc",
		Username:    "alice",
		AppPassword: "app-pwd",
	}
	_, err := CreateRepoHook(context.Background(), srv.Client(), cfg, CreateHookInput{
		Workspace: "acme", RepoSlug: "svc",
		URL: "https://ci.example.com/api/webhooks/bitbucket", Secret: "shh",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(fb.lastAuth, "Basic ") {
		t.Errorf("auth = %q, want Basic", fb.lastAuth)
	}
}

func TestListRepoHooks(t *testing.T) {
	fb := &fakeBitbucket{t: t, hooks: []map[string]any{
		{"uuid": "{a}", "url": "https://ci.example.com/api/webhooks/bitbucket"},
		{"uuid": "{b}", "url": "https://other.example.com/"},
	}}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	got, err := ListRepoHooks(context.Background(), srv.Client(), Config{
		APIBase: srv.URL, Workspace: "acme", RepoSlug: "svc", Token: "t",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hooks, want 2", len(got))
	}
	if _, ok := FindHookForURL(got, "https://ci.example.com"); !ok {
		t.Errorf("FindHookForURL should match ci hook")
	}
}

func TestUpdateRepoHook(t *testing.T) {
	fb := &fakeBitbucket{t: t}
	srv := httptest.NewServer(fb.handler())
	defer srv.Close()

	hook, err := UpdateRepoHook(context.Background(), srv.Client(),
		Config{APIBase: srv.URL, Workspace: "acme", RepoSlug: "svc", Token: "t"},
		"{xyz}",
		CreateHookInput{URL: "https://ci.example.com/api/webhooks/bitbucket", Secret: "new"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if hook.UID != "{abc-def}" {
		t.Errorf("UID = %q", hook.UID)
	}
	if fb.lastUpdate["secret"] != "new" {
		t.Errorf("secret not rotated: %v", fb.lastUpdate["secret"])
	}
}
